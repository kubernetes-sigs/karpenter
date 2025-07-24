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

package cloudprovider_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func TestCloudProvider(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "CloudProvider Suite")
}

var _ = Describe("CloudProvider", func() {
	It("should support unwrapping for NodeClaimNotFound", func() {
		err := cloudprovider.NewNodeClaimNotFoundError(&BaseError{})
		_, ok := lo.ErrorsAs[*BaseError](err)
		Expect(ok).To(BeTrue())
	})
	It("should support unwrapping for InsufficientCapacity", func() {
		err := cloudprovider.NewInsufficientCapacityError(&BaseError{})
		_, ok := lo.ErrorsAs[*BaseError](err)
		Expect(ok).To(BeTrue())
	})
	It("should support unwrapping for NodeClassNotReady", func() {
		err := cloudprovider.NewNodeClassNotReadyError(&BaseError{})
		_, ok := lo.ErrorsAs[*BaseError](err)
		Expect(ok).To(BeTrue())
	})
	It("should support unwrapping for CreateError", func() {
		err := cloudprovider.NewCreateError(&BaseError{}, "", "")
		_, ok := lo.ErrorsAs[*BaseError](err)
		Expect(ok).To(BeTrue())
	})
})

var _ = Describe("Instance Type", func() {
	It("should reserve additional allocatable memory for when HugePages is defined resources", func() {
		it := cloudprovider.InstanceType{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:                   resource.MustParse("5"),
				v1.ResourceMemory:                resource.MustParse("20Gi"),
				v1.ResourceEphemeralStorage:      resource.MustParse("100Gi"),
				v1.ResourcePods:                  resource.MustParse("57"),
				v1.ResourceName("hugepages-2Mi"): resource.MustParse("5Gi"),
				v1.ResourceName("hugepages-1Gi"): resource.MustParse("10Gi"),
			},
			Overhead: &cloudprovider.InstanceTypeOverhead{
				KubeReserved: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("125m"),
					v1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		}

		result := it.Allocatable()
		expectedMemory := resource.MustParse("4Gi")
		expectedCPU := resource.MustParse("4875m")
		Expect(result.Memory().Value()).To(BeNumerically("==", expectedMemory.Value()))
		Expect(result.Cpu().Value()).To(BeNumerically("==", expectedCPU.Value()))
	})
	It("should set memory to zero for when HugePages use all the memory on the instance type", func() {
		it := cloudprovider.InstanceType{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:                   resource.MustParse("5"),
				v1.ResourceMemory:                resource.MustParse("20Gi"),
				v1.ResourceEphemeralStorage:      resource.MustParse("100Gi"),
				v1.ResourcePods:                  resource.MustParse("57"),
				v1.ResourceName("hugepages-2Mi"): resource.MustParse("50Gi"),
				v1.ResourceName("hugepages-1Gi"): resource.MustParse("10Gi"),
			},
			Overhead: &cloudprovider.InstanceTypeOverhead{
				KubeReserved: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("125m"),
					v1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		}

		result := it.Allocatable()
		expectedMemory := resource.MustParse("0")
		expectedCPU := resource.MustParse("4875m")
		Expect(result.Memory().Value()).To(BeNumerically("~", expectedMemory.Value()))
		Expect(result.Cpu().Value()).To(BeNumerically("~", expectedCPU.Value()))
	})
})

type BaseError struct {
	error
}
