/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scheduling

import (
	"errors"
	"fmt"
	"testing"

	"github.com/awslabs/operatorpkg/serrors"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func TestSchedulingErrorRank(t *testing.T) {
	nodepoolLimitExceeded := fmt.Errorf("all available instance types exceed limits for nodepool")
	nodepoolNodeLimitExhausted := fmt.Errorf("node limits have been exhausted for nodepool")
	resourceExceeded := fmt.Errorf("instance type exceeds resources")
	incompatibleRequirements := fmt.Errorf("incompatible requirements")
	offeringError := fmt.Errorf("no matching offering")
	hostPortError := fmt.Errorf("host port conflict")
	taintError := fmt.Errorf("did not tolerate taint")
	unknownError := fmt.Errorf("something unexpected")
	nilErr := error(nil)

	tests := []struct {
		name     string
		err      error
		wantRank int
	}{
		{"nodepool resource limits exceeded", nodepoolLimitExceeded, 0},
		{"nodepool node limits exhausted", nodepoolNodeLimitExhausted, 0},
		{"instance type exceeds resources", resourceExceeded, 0},
		{"incompatible requirements", incompatibleRequirements, 10},
		{"offering error", offeringError, 20},
		{"host port error", hostPortError, 30},
		{"emitted hostport error", fmt.Errorf("pod hostport conflicts with existing hostport configuration"), 30},
		{"volume attachment limit", volumeAttachmentLimitError(nil, nil), 30},
		{"taint error", taintError, 40},
		{"multiple taints with metadata", multierr.Combine(
			serrors.Wrap(taintError, "taint", "spot-offering:NoSchedule"),
			serrors.Wrap(taintError, "taint", "exceeds-limits:NoSchedule"),
		), 40},
		{"unknown error", unknownError, 50},
		{"instance type filter resources", InstanceTypeFilterError{hasOffering: true}, 0},
		{"instance type filter requirements", InstanceTypeFilterError{fits: true, hasOffering: true}, 10},
		{"instance type filter offerings", InstanceTypeFilterError{requirementsMet: true}, 20},
		{"instance type filter requirements and offerings", InstanceTypeFilterError{}, 10},
		{"instance type filter combined constraints", InstanceTypeFilterError{fits: true, requirementsMet: true, hasOffering: true}, 30},
		{"instance type filter min values", InstanceTypeFilterError{fits: true, requirementsMet: true, hasOffering: true, minValuesIncompatibleErr: fmt.Errorf("minValues requirement is not met")}, 10},
		{"wrapped incompatible requirements", fmt.Errorf("incompatible requirements, %w", unknownError), 10},
		{"scheduling context around metadata", fmt.Errorf("incompatible requirements, %w", serrors.Wrap(unknownError, "NodePool", klog.KRef("", "default"))), 10},
		{"nil error", nilErr, 100},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := schedulingErrorRank(testCase.err)
			if got != testCase.wantRank {
				t.Errorf("schedulingErrorRank(%v) = %d, want %d", testCase.err, got, testCase.wantRank)
			}
		})
		if testCase.err == nil {
			continue
		}
		for _, nodePoolName := range []string{"default", "spot-offering", "exceeds-limits", "exhausted", "host-port-volume"} {
			t.Run(testCase.name+"/"+nodePoolName, func(t *testing.T) {
				err := serrors.Wrap(testCase.err, "NodePool", klog.KRef("", nodePoolName))
				if got := schedulingErrorRank(err); got != testCase.wantRank {
					t.Errorf("schedulingErrorRank(%v) = %d, want %d", err, got, testCase.wantRank)
				}
				// Structured errors can carry metadata at multiple levels.
				err = serrors.Wrap(err, "NodeClaim", klog.KRef("", "exceeds-limits"))
				if got := schedulingErrorRank(err); got != testCase.wantRank {
					t.Errorf("schedulingErrorRank(%v) = %d, want %d", err, got, testCase.wantRank)
				}
			})
		}
	}
}

func TestSortSchedulingErrors(t *testing.T) {
	nodepoolLimitExceeded := fmt.Errorf("all available instance types exceed limits for nodepool")
	incompatibleRequirements := fmt.Errorf("incompatible requirements")
	taintError := fmt.Errorf("did not tolerate taint")
	unknownError := fmt.Errorf("something unexpected")

	t.Run("sortSchedulingErrors orders by rank", func(t *testing.T) {
		errs := []error{unknownError, taintError, nodepoolLimitExceeded, incompatibleRequirements}
		sortSchedulingErrors(errs)
		for idx := 1; idx < len(errs); idx++ {
			if schedulingErrorRank(errs[idx-1]) > schedulingErrorRank(errs[idx]) {
				t.Errorf("errors not sorted: rank(%v)=%d > rank(%v)=%d",
					errs[idx-1], schedulingErrorRank(errs[idx-1]),
					errs[idx], schedulingErrorRank(errs[idx]))
			}
		}
		if !errors.Is(errs[0], nodepoolLimitExceeded) {
			t.Errorf("expected nodepool limit error first, got %v", errs[0])
		}
	})

	t.Run("sortSchedulingErrors preserves metadata and equal-rank order", func(t *testing.T) {
		firstTaint := serrors.Wrap(taintError, "NodePool", klog.KRef("", "spot-offering"))
		secondTaint := serrors.Wrap(taintError, "NodePool", klog.KRef("", "exceeds-limits"))
		limits := serrors.Wrap(nodepoolLimitExceeded, "NodePool", klog.KRef("", "default"))
		errs := []error{firstTaint, secondTaint, limits}
		sortSchedulingErrors(errs)
		for i, want := range []error{limits, firstTaint, secondTaint} {
			if !errors.Is(errs[i], want) {
				t.Errorf("error at index %d = %v, want %v", i, errs[i], want)
			}
		}
	})
}

func TestSchedulingErrorRankFromSchedulingChecks(t *testing.T) {
	t.Run("unavailable offerings are not a resource shortage", func(t *testing.T) {
		instanceType := fake.NewInstanceType("test")
		for _, offering := range instanceType.Offerings {
			offering.Available = false
		}
		itFits, hasOffering := fits(instanceType, nil, scheduling.NewRequirements())
		err := InstanceTypeFilterError{fits: itFits, hasOffering: hasOffering, requirementsMet: true}
		if got := schedulingErrorRank(err); got != 20 {
			t.Errorf("schedulingErrorRank(%v) = %d, want 20", err, got)
		}
	})
	t.Run("resource shortage with available offerings", func(t *testing.T) {
		itFits, hasOffering := fits(fake.NewInstanceType("test"), corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000")}, scheduling.NewRequirements())
		err := InstanceTypeFilterError{fits: itFits, hasOffering: hasOffering, requirementsMet: true}
		if got := schedulingErrorRank(err); got != 0 {
			t.Errorf("schedulingErrorRank(%v) = %d, want 0", err, got)
		}
	})
	t.Run("multiple untolerated taints", func(t *testing.T) {
		err := scheduling.Taints{
			{Key: "spot-offering", Effect: corev1.TaintEffectNoSchedule},
			{Key: "exceeds-limits", Effect: corev1.TaintEffectNoSchedule},
		}.Tolerates(nil)
		if got := schedulingErrorRank(err); got != 40 {
			t.Errorf("schedulingErrorRank(%v) = %d, want 40", err, got)
		}
	})
}
