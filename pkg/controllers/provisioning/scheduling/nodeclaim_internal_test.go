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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

var _ = Describe("InstanceTypeFilterError", func() {
	offering := func(capacityType, zone string) cloudprovider.Offering {
		return cloudprovider.Offering{
			Available: true,
			Price:     1,
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:  capacityType,
				corev1.LabelTopologyZone: zone,
			}),
		}
	}
	instanceType := func(name, arch string, offerings ...cloudprovider.Offering) *cloudprovider.InstanceType {
		return fake.NewInstanceType(name,
			fake.WithArchitecture(arch),
			fake.WithResources(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("16"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
				corev1.ResourcePods:   resource.MustParse("10"),
			}),
			fake.WithOfferings(offerings...),
		)
	}
	filter := func(requirements map[string]string, requests corev1.ResourceList, its ...*cloudprovider.InstanceType) error {
		reqs := scheduling.NewLabelRequirements(requirements)
		remaining, _, err := filterInstanceTypesByRequirements(its, reqs, &corev1.Pod{}, requests,
			[]DaemonOverheadGroup{{InstanceTypes: its, HostPortUsage: scheduling.NewHostPortUsage()}}, requests, false)
		Expect(remaining).To(BeEmpty())
		return err
	}

	It("should report the offering as the missing criteria when an instance type has room but no compatible offering", func() {
		// The aggregate requirements intersect (zone-1 and spot are each offered) and the instance type has
		// room, but no single offering is both zone-1 and spot.
		err := filter(
			map[string]string{corev1.LabelTopologyZone: "test-zone-1", v1.CapacityTypeLabelKey: "spot"},
			corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			instanceType("big", "amd64", offering("on-demand", "test-zone-1"), offering("spot", "test-zone-2")),
		)
		Expect(err).To(MatchError(ContainSubstring("no instance type has the required offering")))
	})

	It("should report resources as the missing criteria when the offering is compatible but the pod is too large", func() {
		err := filter(
			map[string]string{corev1.LabelTopologyZone: "test-zone-1", v1.CapacityTypeLabelKey: "spot"},
			corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000")},
			instanceType("small", "amd64", offering("spot", "test-zone-1")),
		)
		Expect(err).To(MatchError(ContainSubstring("no instance type has enough resources")))
	})

	It("should report the requirements/resources pair when one instance type met both but lacked an offering", func() {
		// "arm-no-offering" meets the requirements and has room, but has no zone-1 spot offering.
		// "amd-launchable" has room and a zone-1 spot offering, but is the wrong architecture. Each individual
		// criteria is therefore met by some instance type, so the failure is only visible pairwise.
		err := filter(
			map[string]string{
				corev1.LabelArchStable:   "arm64",
				corev1.LabelTopologyZone: "test-zone-1",
				v1.CapacityTypeLabelKey:  "spot",
			},
			corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			instanceType("arm-no-offering", "arm64", offering("on-demand", "test-zone-1"), offering("spot", "test-zone-2")),
			instanceType("amd-launchable", "amd64", offering("spot", "test-zone-1")),
		)
		Expect(err).To(MatchError(ContainSubstring("no instance type which met the scheduling requirements and had enough resources, had a required offering")))
	})
})
