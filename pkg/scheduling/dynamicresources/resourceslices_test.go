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

package dynamicresources_test

import (
	"unique"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// countingNodeClaim wraps a fakeNodeClaim and counts calls to ResourceSlices.
type countingNodeClaim struct {
	*fakeNodeClaim
	resourceSlicesCalls int
}

func (c *countingNodeClaim) ResourceSlices() map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice {
	c.resourceSlicesCalls++
	return c.fakeNodeClaim.ResourceSlices()
}

var _ = Describe("ResourceSlices", func() {
	var alloc *dynamicresources.Allocator

	BeforeEach(func() {
		ExpectApplied(ctx, env.Client,
			&resourcev1.DeviceClass{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu"},
				Spec: resourcev1.DeviceClassSpec{
					Selectors: []resourcev1.DeviceSelector{
						{CEL: &resourcev1.CELDeviceSelector{Expression: `device.driver == "gpu.example.com"`}},
					},
				},
			},
		)
		alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
	})

	// counterTemplate returns a template with two devices that each consume 40Gi from a counter set with the given budget.
	counterTemplate := func(budget string) *cloudprovider.ResourceSliceTemplate {
		return makeTemplateWithCounters("gpu.example.com", "pool-a",
			[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse(budget)})},
			templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
			templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
		)
	}

	It("should call ResourceSlices once per Allocate across multiple instance types", func() {
		nc := &countingNodeClaim{fakeNodeClaim: makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
			"it-1": {makeTemplate("gpu.example.com", "pool-a", "gpu-0", "gpu-1")},
			"it-2": {makeTemplate("gpu.example.com", "pool-a", "gpu-0", "gpu-1")},
			"it-3": {makeTemplate("gpu.example.com", "pool-a", "gpu-0", "gpu-1")},
		})}
		result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.InstanceTypes).To(HaveLen(3))
		Expect(nc.resourceSlicesCalls).To(Equal(1))
	})
	It("should compute each instance type's template counter budget from its own templates", func() {
		nc := &countingNodeClaim{fakeNodeClaim: makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
			"it-small": {counterTemplate("40Gi")},
			"it-large": {counterTemplate("80Gi")},
		})}
		// Both devices fit it-large's 80Gi budget, but only one fits it-small's 40Gi budget.
		result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 2))})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.InstanceTypes).To(ConsistOf(unique.Make("it-large")))
		Expect(nc.resourceSlicesCalls).To(Equal(1))
	})
	It("should prefer committed template counter budgets over the NodeClaim's templates", func() {
		nc := &countingNodeClaim{fakeNodeClaim: makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
			"it-small": {counterTemplate("40Gi")},
			"it-large": {counterTemplate("80Gi")},
		})}
		r1, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
		Expect(r1.InstanceTypes).To(HaveLen(2))
		r1.Allocation.Commit(ctx)

		// After the first commit, it-small has no budget left while it-large has 40Gi remaining. Recomputing budgets from
		// the templates would incorrectly leave it-small with a full 40Gi.
		r2, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c2", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
		Expect(r2.InstanceTypes).To(ConsistOf(unique.Make("it-large")))
		Expect(nc.resourceSlicesCalls).To(Equal(2))
	})
})
