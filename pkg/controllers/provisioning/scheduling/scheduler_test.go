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
	"context"
	"errors"
	"fmt"

	"github.com/awslabs/operatorpkg/serrors"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

var _ = Describe("Scheduling error priorities", func() {
	DescribeTable("should rank typed scheduling failures",
		func(err error, rank int) {
			Expect(schedulingErrorRank(err)).To(Equal(rank))
			if err == nil {
				return
			}
			for _, nodePoolName := range []string{"default", "spot-offering", "exceeds-limits", "exhausted", "host-port-volume"} {
				wrapped := serrors.Wrap(err, "NodePool", klog.KRef("", nodePoolName))
				Expect(schedulingErrorRank(wrapped)).To(Equal(rank), "NodePool=%s", nodePoolName)
				wrapped = serrors.Wrap(wrapped, "NodeClaim", klog.KRef("", "exceeds-limits"))
				Expect(schedulingErrorRank(wrapped)).To(Equal(rank), "NodePool=%s with nested metadata", nodePoolName)
				Expect(schedulingErrorRank(fmt.Errorf("scheduling failed, %w", wrapped))).To(Equal(rank))
			}
		},
		Entry("NodePool resource limits exceeded", NewNodePoolLimitError(fmt.Errorf("all available instance types exceed limits for nodepool")), 0),
		Entry("NodePool node limits exhausted", NewNodePoolLimitError(fmt.Errorf("node limits have been exhausted for nodepool")), 0),
		Entry("incompatible requirements", NewIncompatibleRequirementsError(fmt.Errorf("incompatible requirements")), 10),
		Entry("offering error", NewOfferingError(fmt.Errorf("no matching offering")), 20),
		Entry("reserved offering error", NewOfferingError(NewReservedOfferingError(fmt.Errorf("compatible reserved offerings could not be reserved"))), 20),
		Entry("volume attachment limit", NewVolumeConstraintError(volumeAttachmentLimitError(nil, nil)), 30),
		Entry("volume topology", NewVolumeConstraintError(fmt.Errorf("incompatible volume requirements")), 30),
		Entry("taint error", NewTaintTolerationError(fmt.Errorf("did not tolerate taint")), 40),
		Entry("multiple taints with metadata", NewTaintTolerationError(multierr.Combine(
			serrors.Wrap(fmt.Errorf("did not tolerate taint"), "taint", "spot-offering:NoSchedule"),
			serrors.Wrap(fmt.Errorf("did not tolerate taint"), "taint", "exceeds-limits:NoSchedule"),
		)), 40),
		Entry("unknown error", fmt.Errorf("something unexpected"), 50),
		Entry("unknown error containing scheduling keywords", fmt.Errorf("exceeds limits, incompatible requirements, offering, hostport, volume, did not tolerate taint"), 50),
		Entry("instance type filter resources", InstanceTypeFilterError{hasOffering: true}, 0),
		Entry("instance type filter requirements", InstanceTypeFilterError{fits: true, hasOffering: true}, 10),
		Entry("instance type filter offerings", InstanceTypeFilterError{requirementsMet: true}, 20),
		Entry("instance type filter requirements and offerings", InstanceTypeFilterError{}, 10),
		Entry("instance type filter combined constraints", InstanceTypeFilterError{fits: true, requirementsMet: true, hasOffering: true}, 30),
		Entry("instance type filter min values", InstanceTypeFilterError{fits: true, requirementsMet: true, hasOffering: true, minValuesIncompatibleErr: fmt.Errorf("minValues requirement is not met")}, 10),
		Entry("requirements context around metadata", NewIncompatibleRequirementsError(fmt.Errorf("incompatible requirements, %w", serrors.Wrap(fmt.Errorf("unexpected"), "NodePool", klog.KRef("", "default")))), 10),
		Entry("nil error", nil, 100),
	)

	It("should order errors by priority", func() {
		limits := NewNodePoolLimitError(fmt.Errorf("limits exceeded"))
		requirements := NewIncompatibleRequirementsError(fmt.Errorf("incompatible requirements"))
		offering := NewOfferingError(fmt.Errorf("no matching offering"))
		volume := NewVolumeConstraintError(fmt.Errorf("volume limit exceeded"))
		taint := NewTaintTolerationError(fmt.Errorf("did not tolerate taint"))
		unknown := fmt.Errorf("something unexpected")
		errs := []error{nil, unknown, taint, volume, offering, limits, requirements}
		sortSchedulingErrors(errs)
		Expect(errs).To(Equal([]error{limits, requirements, offering, volume, taint, unknown, nil}))
	})

	It("should preserve metadata and the original order of equal-priority errors", func() {
		taint := NewTaintTolerationError(fmt.Errorf("did not tolerate taint"))
		firstTaint := serrors.Wrap(taint, "NodePool", klog.KRef("", "spot-offering"))
		secondTaint := serrors.Wrap(taint, "NodePool", klog.KRef("", "exceeds-limits"))
		limits := serrors.Wrap(NewNodePoolLimitError(fmt.Errorf("limits exceeded")), "NodePool", klog.KRef("", "default"))
		errs := []error{firstTaint, secondTaint, limits}
		sortSchedulingErrors(errs)
		Expect(errs).To(Equal([]error{limits, firstTaint, secondTaint}))
		Expect(errs[1]).To(BeIdenticalTo(firstTaint))
		Expect(errs[2]).To(BeIdenticalTo(secondTaint))
	})

	It("should preserve reserved offering detection and the underlying error", func() {
		original := fmt.Errorf("compatible reserved offerings could not be reserved")
		err := serrors.Wrap(NewOfferingError(NewReservedOfferingError(original)), "NodePool", klog.KRef("", "default"))
		Expect(IsReservedOfferingError(err)).To(BeTrue())
		Expect(errors.Is(err, original)).To(BeTrue())
	})

	It("should use the highest priority in combined errors with metadata", func() {
		err := multierr.Combine(
			serrors.Wrap(NewTaintTolerationError(fmt.Errorf("did not tolerate taint")), "NodePool", klog.KRef("", "exceeds-limits")),
			serrors.Wrap(NewNodePoolLimitError(fmt.Errorf("limits exceeded")), "NodePool", klog.KRef("", "spot-offering")),
		)
		Expect(schedulingErrorRank(err)).To(Equal(0))
		Expect(schedulingErrorRank(serrors.Wrap(err, "NodeClaim", klog.KRef("", "default")))).To(Equal(0))
	})

	It("should rank unavailable offerings separately from resource shortages", func() {
		instanceType := fake.NewInstanceType("test")
		for _, offering := range instanceType.Offerings {
			offering.Available = false
		}
		itFits, hasOffering := fits(instanceType, nil, scheduling.NewRequirements())
		err := InstanceTypeFilterError{fits: itFits, hasOffering: hasOffering, requirementsMet: true}
		Expect(schedulingErrorRank(err)).To(Equal(20))
	})

	It("should prioritize resource shortages when offerings are available", func() {
		itFits, hasOffering := fits(fake.NewInstanceType("test"), corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000")}, scheduling.NewRequirements())
		err := InstanceTypeFilterError{fits: itFits, hasOffering: hasOffering, requirementsMet: true}
		Expect(schedulingErrorRank(err)).To(Equal(0))
	})

	It("should rank multiple untolerated taints independently of their metadata", func() {
		nodeClaim := &NodeClaim{}
		nodeClaim.Spec.Taints = []corev1.Taint{
			{Key: "spot-offering", Effect: corev1.TaintEffectNoSchedule},
			{Key: "exceeds-limits", Effect: corev1.TaintEffectNoSchedule},
		}
		_, _, _, _, err := nodeClaim.CanAdd(context.Background(), &corev1.Pod{}, nil, nil, false, nil)
		Expect(err).To(HaveOccurred())
		Expect(schedulingErrorRank(err)).To(Equal(40))
	})
})
