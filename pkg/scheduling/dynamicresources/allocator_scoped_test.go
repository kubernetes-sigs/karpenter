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
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const (
	gpuModeKey = "example.com/driver-mode-gpu"
	nicModeKey = "example.com/driver-mode-nic"
)

// scopedSlice wraps a template from a DynamicResources set whose requirements are key In [value].
func scopedSlice(key, value string, t *cloudprovider.ResourceSliceTemplate) dynamicresources.ResourceSlice {
	return dynamicresources.NewScopedTemplateSlice(t, scheduling.NewRequirements(scheduling.NewRequirement(key, corev1.NodeSelectorOpIn, value)))
}

// scopedNodeClaim builds an in-flight NodeClaim whose instance types, in the given order, each expose the given slices.
func scopedNodeClaim(itNames []string, slices map[string][]dynamicresources.ResourceSlice, reqs ...*scheduling.Requirement) *fakeNodeClaim {
	nc := &fakeNodeClaim{
		id:             unique.Make("test-nc"),
		nodePoolID:     unique.Make("test-np"),
		requirements:   scheduling.NewRequirements(reqs...),
		resourceSlices: map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice{},
	}
	for _, name := range itNames {
		nc.instanceTypes = append(nc.instanceTypes, unique.Make(name))
		nc.resourceSlices[unique.Make(name)] = slices[name]
	}
	return nc
}

func allocatedPools(alloc *dynamicresources.Allocator, claim, it string) []string {
	meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: claim})
	Expect(meta).ToNot(BeNil())
	var pools []string
	for _, d := range meta.Devices[unique.Make(it)] {
		pools = append(pools, d.DeviceID.Pool.Value())
	}
	return pools
}

var _ = Describe("Requirement-scoped template sets", func() {
	var alloc *dynamicresources.Allocator

	BeforeEach(func() {
		v1.WellKnownLabels.Insert(gpuModeKey, nicModeKey)
		DeferCleanup(func() { v1.WellKnownLabels.Delete(gpuModeKey, nicModeKey) })
		ExpectApplied(ctx, env.Client,
			&resourcev1.DeviceClass{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu"},
				Spec: resourcev1.DeviceClassSpec{Selectors: []resourcev1.DeviceSelector{
					{CEL: &resourcev1.CELDeviceSelector{Expression: `device.driver == "gpu.example.com"`}},
				}},
			},
			&resourcev1.DeviceClass{
				ObjectMeta: metav1.ObjectMeta{Name: "nic"},
				Spec: resourcev1.DeviceClassSpec{Selectors: []resourcev1.DeviceSelector{
					{CEL: &resourcev1.CELDeviceSelector{Expression: `device.driver == "nic.example.com"`}},
				}},
			},
		)
		alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
	})

	It("should contribute a scoped set's requirements when one of its devices is allocated", func() {
		nc := scopedNodeClaim([]string{"it-1"}, map[string][]dynamicresources.ResourceSlice{
			"it-1": {scopedSlice(gpuModeKey, "dra", makeTemplate("gpu.example.com", "gpus", "gpu-0"))},
		})
		result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Requirements.Get(gpuModeKey).Values()).To(ConsistOf("dra"))
	})

	It("should not contribute requirements for an unscoped set", func() {
		nc := makeNodeClaimWithTemplates(makeTemplate("gpu.example.com", "gpus", "gpu-0"))
		result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Requirements).To(BeEmpty())
	})

	It("should treat a set whose requirements are incompatible with the NodeClaim's as ineligible", func() {
		nc := scopedNodeClaim([]string{"it-1"}, map[string][]dynamicresources.ResourceSlice{
			"it-1": {scopedSlice(gpuModeKey, "dra", makeTemplate("gpu.example.com", "gpus", "gpu-0"))},
		}, scheduling.NewRequirement(gpuModeKey, corev1.NodeSelectorOpIn, "device-plugin"))
		_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 1))})
		Expect(err).To(HaveOccurred())
	})

	It("should treat an undefined set key as open on an in-flight NodeClaim", func() {
		nc := scopedNodeClaim([]string{"it-1"}, map[string][]dynamicresources.ResourceSlice{
			"it-1": {scopedSlice(gpuModeKey, "dra", makeTemplate("gpu.example.com", "gpus", "gpu-0"))},
		})
		_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
	})

	It("should treat an undefined set key as unknown on an existing node", func() {
		nc := scopedNodeClaim([]string{"it-1"}, map[string][]dynamicresources.ResourceSlice{
			"it-1": {
				scopedSlice(gpuModeKey, "dra", makeTemplate("gpu.example.com", "scoped-gpus", "gpu-0")),
				dynamicresources.NewTemplateSlice(makeTemplate("gpu.example.com", "unscoped-gpus", "gpu-1")),
			},
		})
		nc.existing = true
		claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
		result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
		Expect(err).ToNot(HaveOccurred())
		result.Allocation.Commit(ctx)
		Expect(allocatedPools(alloc, "c1", "it-1")).To(ConsistOf("unscoped-gpus"))
	})

	It("should backtrack out of a set, undoing its requirements, when another set of the driver is needed", func() {
		// Set "x" has one device, set "y" has two. Two devices from one set are needed, so the DFS's first pick from
		// "x" has to be undone along with the requirement it added.
		nc := scopedNodeClaim([]string{"it-1"}, map[string][]dynamicresources.ResourceSlice{
			"it-1": {
				scopedSlice(gpuModeKey, "x", makeTemplate("gpu.example.com", "pool-x", "gpu-x0")),
				scopedSlice(gpuModeKey, "y", makeTemplate("gpu.example.com", "pool-y", "gpu-y0", "gpu-y1")),
			},
		})
		result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 2))})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Requirements.Get(gpuModeKey).Values()).To(ConsistOf("y"))
		result.Allocation.Commit(ctx)
		Expect(allocatedPools(alloc, "c1", "it-1")).To(ConsistOf("pool-y", "pool-y"))
	})

	It("should not allocate devices from mutually exclusive sets of one driver together", func() {
		nc := scopedNodeClaim([]string{"it-1"}, map[string][]dynamicresources.ResourceSlice{
			"it-1": {
				scopedSlice(gpuModeKey, "x", makeTemplate("gpu.example.com", "pool-x", "gpu-x0")),
				scopedSlice(gpuModeKey, "y", makeTemplate("gpu.example.com", "pool-y", "gpu-y0")),
			},
		})
		_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 2))})
		Expect(err).To(HaveOccurred())
	})

	It("should keep drivers independent", func() {
		slices := map[string][]dynamicresources.ResourceSlice{
			"it-1": {
				scopedSlice(gpuModeKey, "dra", makeTemplate("gpu.example.com", "gpus", "gpu-0")),
				scopedSlice(nicModeKey, "dra", makeTemplate("nic.example.com", "nics", "nic-0")),
			},
		}
		result, err := alloc.Allocate(ctx, scopedNodeClaim([]string{"it-1"}, slices), []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Requirements.Has(gpuModeKey)).To(BeTrue())
		Expect(result.Requirements.Has(nicModeKey)).To(BeFalse())

		// A NodeClaim already narrowed to the NIC's device-plugin mode can still allocate the GPU.
		nc := scopedNodeClaim([]string{"it-1"}, slices, scheduling.NewRequirement(nicModeKey, corev1.NodeSelectorOpIn, "device-plugin"))
		nc.id = unique.Make("test-nc-2")
		result, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c2", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Requirements.Get(gpuModeKey).Values()).To(ConsistOf("dra"))
	})

	It("should evaluate later instance types against the requirements earlier ones contributed", func() {
		nc := scopedNodeClaim([]string{"it-a", "it-b"}, map[string][]dynamicresources.ResourceSlice{
			"it-a": {scopedSlice(gpuModeKey, "x", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))},
			"it-b": {scopedSlice(gpuModeKey, "y", makeTemplate("gpu.example.com", "pool-b", "gpu-0"))},
		})
		result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.InstanceTypes).To(ConsistOf(unique.Make("it-a")))
		Expect(result.Requirements.Get(gpuModeKey).Values()).To(ConsistOf("x"))

		nc = scopedNodeClaim([]string{"it-a", "it-b"}, map[string][]dynamicresources.ResourceSlice{
			"it-a": {scopedSlice(gpuModeKey, "x", makeTemplate("gpu.example.com", "pool-a", "gpu-0"))},
			"it-b": {scopedSlice(gpuModeKey, "x", makeTemplate("gpu.example.com", "pool-b", "gpu-0"))},
		})
		nc.id = unique.Make("test-nc-2")
		result, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c2", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.InstanceTypes).To(ConsistOf(unique.Make("it-a"), unique.Make("it-b")))
	})

	It("should never satisfy a binding with devices from mutually exclusive sets", func() {
		gx := deviceID("gpu.example.com", "pool-x", "gpu-x0")
		gy := deviceID("gpu.example.com", "pool-y", "gpu-y0")
		gx1 := deviceID("gpu.example.com", "pool-x", "gpu-x1")
		bindings := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
			"test-np": {&cloudprovider.InstanceType{
				Name:              "it-1",
				AttributeBindings: []*cloudprovider.AttributeBinding{attrBinding("gpu.example.com/numa", gx, gy, gx1)},
			}},
		})
		claim := func(name string) *resourcev1.ResourceClaim {
			return makeClaimWithConstraints(name,
				[]resourcev1.DeviceConstraint{{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))}},
				exactRequest("req-1", "gpu", 2),
			)
		}

		alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, bindings, env.Client, nil)
		nc := scopedNodeClaim([]string{"it-1"}, map[string][]dynamicresources.ResourceSlice{
			"it-1": {
				scopedSlice(gpuModeKey, "x", makeTemplate("gpu.example.com", "pool-x", "gpu-x0")),
				scopedSlice(gpuModeKey, "y", makeTemplate("gpu.example.com", "pool-y", "gpu-y0")),
			},
		})
		_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim("c1")})
		Expect(err).To(HaveOccurred())

		// Control: the same binding is satisfied by two devices of one set.
		nc = scopedNodeClaim([]string{"it-1"}, map[string][]dynamicresources.ResourceSlice{
			"it-1": {
				scopedSlice(gpuModeKey, "x", makeTemplate("gpu.example.com", "pool-x", "gpu-x0", "gpu-x1")),
				scopedSlice(gpuModeKey, "y", makeTemplate("gpu.example.com", "pool-y", "gpu-y0")),
			},
		})
		nc.id = unique.Make("test-nc-2")
		result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim("c2")})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Requirements.Get(gpuModeKey).Values()).To(ConsistOf("x"))
	})

	It("should apply a scoped set's shared counters", func() {
		counters := []resourcev1.CounterSet{{Name: "memory", Counters: map[string]resourcev1.Counter{"mem": {Value: resource.MustParse("1")}}}}
		consume := counterConsumption("memory", map[string]resource.Quantity{"mem": resource.MustParse("1")})
		nc := scopedNodeClaim([]string{"it-1"}, map[string][]dynamicresources.ResourceSlice{
			"it-1": {scopedSlice(gpuModeKey, "dra", makeTemplateWithCounters("gpu.example.com", "gpus", counters,
				templateDevice("gpu-0", consume), templateDevice("gpu-1", consume)))},
		})
		_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c1", exactRequest("req-1", "gpu", 2))})
		Expect(err).To(HaveOccurred())
		result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{makeClaim("c2", exactRequest("req-1", "gpu", 1))})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Requirements.Get(gpuModeKey).Values()).To(ConsistOf("dra"))
	})
})
