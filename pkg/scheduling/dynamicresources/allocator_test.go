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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// fakeNodeClaim implements the NodeClaim interface for testing.
type fakeNodeClaim struct {
	id             dynamicresources.NodeClaimID
	nodePoolID     dynamicresources.NodePoolID
	requirements   scheduling.Requirements
	instanceTypes  []dynamicresources.InstanceTypeID
	resourceSlices map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice
}

func (f *fakeNodeClaim) ID() dynamicresources.NodeClaimID            { return f.id }
func (f *fakeNodeClaim) NodePoolID() dynamicresources.NodePoolID     { return f.nodePoolID }
func (f *fakeNodeClaim) Requirements() scheduling.Requirements       { return f.requirements }
func (f *fakeNodeClaim) InstanceTypes() []dynamicresources.InstanceTypeID { return f.instanceTypes }
func (f *fakeNodeClaim) ResourceSlices() map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice {
	return f.resourceSlices
}

func makeNodeClaim(itNames ...string) *fakeNodeClaim {
	return makeNodeClaimWithID("test-nc", itNames...)
}

func makeNodeClaimWithID(ncID string, itNames ...string) *fakeNodeClaim {
	itIDs := make([]dynamicresources.InstanceTypeID, len(itNames))
	for i, name := range itNames {
		itIDs[i] = unique.Make(name)
	}
	return &fakeNodeClaim{
		id:             unique.Make(ncID),
		nodePoolID:     unique.Make("test-np"),
		requirements:   scheduling.NewRequirements(),
		instanceTypes:  itIDs,
		resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
	}
}

func makeNodeClaimWithTemplates(itName string, templates ...*cloudprovider.ResourceSliceTemplate) *fakeNodeClaim {
	return makeNodeClaimWithTemplatesAndID("test-nc", itName, templates...)
}

func makeNodeClaimWithTemplatesAndID(ncID, itName string, templates ...*cloudprovider.ResourceSliceTemplate) *fakeNodeClaim {
	itID := unique.Make(itName)
	slices := make([]dynamicresources.ResourceSlice, len(templates))
	for i, t := range templates {
		slices[i] = dynamicresources.NewTemplateSlice(t)
	}
	return &fakeNodeClaim{
		id:            unique.Make(ncID),
		nodePoolID:    unique.Make("test-np"),
		requirements:  scheduling.NewRequirements(),
		instanceTypes: []dynamicresources.InstanceTypeID{itID},
		resourceSlices: map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice{
			itID: slices,
		},
	}
}

func makeMultiITNodeClaim(templates map[string][]*cloudprovider.ResourceSliceTemplate) *fakeNodeClaim {
	itIDs := make([]dynamicresources.InstanceTypeID, 0, len(templates))
	rs := make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice, len(templates))
	for itName, tmpls := range templates {
		itID := unique.Make(itName)
		itIDs = append(itIDs, itID)
		slices := make([]dynamicresources.ResourceSlice, len(tmpls))
		for i, t := range tmpls {
			slices[i] = dynamicresources.NewTemplateSlice(t)
		}
		rs[itID] = slices
	}
	return &fakeNodeClaim{
		id:             unique.Make("test-nc"),
		nodePoolID:     unique.Make("test-np"),
		requirements:   scheduling.NewRequirements(),
		instanceTypes:  itIDs,
		resourceSlices: rs,
	}
}

func makeTemplate(driver, pool string, deviceNames ...string) *cloudprovider.ResourceSliceTemplate {
	devices := make([]cloudprovider.Device, len(deviceNames))
	for i, name := range deviceNames {
		devices[i] = cloudprovider.Device{Name: unique.Make(name)}
	}
	return &cloudprovider.ResourceSliceTemplate{
		Driver:  unique.Make(driver),
		Pool:    cloudprovider.ResourcePool{Name: unique.Make(pool)},
		Devices: devices,
	}
}

func makeTemplateWithAttrs(driver, pool string, specs ...apiDeviceSpec) *cloudprovider.ResourceSliceTemplate {
	devices := make([]cloudprovider.Device, len(specs))
	for i, spec := range specs {
		devices[i] = cloudprovider.Device{
			Name:       unique.Make(spec.name),
			Attributes: spec.attrs,
		}
	}
	return &cloudprovider.ResourceSliceTemplate{
		Driver:  unique.Make(driver),
		Pool:    cloudprovider.ResourcePool{Name: unique.Make(pool)},
		Devices: devices,
	}
}

func makeClaim(name string, requests ...resourcev1.DeviceRequest) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: requests,
			},
		},
	}
}

func makeClaimWithConstraints(name string, constraints []resourcev1.DeviceConstraint, requests ...resourcev1.DeviceRequest) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests:    requests,
				Constraints: constraints,
			},
		},
	}
}

func exactRequest(name, className string, count int64) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{
		Name: name,
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: className,
			Count:           count,
		},
	}
}

func exactRequestWithSelector(name, className string, count int64, expr string) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{
		Name: name,
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: className,
			Count:           count,
			Selectors: []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: expr}},
			},
		},
	}
}

func makeAllocatedClaim(name string, nodeSelector *corev1.NodeSelector) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				NodeSelector: nodeSelector,
			},
		},
	}
}

func allRequest(name, className string) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{
		Name: name,
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: className,
			AllocationMode:  resourcev1.DeviceAllocationModeAll,
		},
	}
}

var _ = Describe("Allocator", func() {
	var (
		alloc *dynamicresources.Allocator
	)

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
	})

	Describe("Empty claims", func() {
		It("should return immediately with no claims", func() {
			nc := makeNodeClaim("it-1")
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			result, err := alloc.Allocate(ctx, nc, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(1))
		})
	})

	Describe("Single IT, in-cluster devices", func() {
		var inClusterSlices []dynamicresources.ResourceSlice

		BeforeEach(func() {
			inClusterSlices = []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1", "gpu-2", "gpu-3")),
			}
		})

		It("should allocate a single device", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.InstanceTypes).To(HaveLen(1))
			Expect(result.Allocation).ToNot(BeNil())
		})

		It("should allocate multiple devices for a single request", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 3))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should fail when not enough devices are available", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 5))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should handle multiple requests in a single claim", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1",
				exactRequest("req-1", "gpu", 2),
				exactRequest("req-2", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should fail when multiple requests exceed total devices", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1",
				exactRequest("req-1", "gpu", 3),
				exactRequest("req-2", "gpu", 3),
			)

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should handle multiple claims", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claims := []*resourcev1.ResourceClaim{
				makeClaim("c1", exactRequest("req-1", "gpu", 2)),
				makeClaim("c2", exactRequest("req-1", "gpu", 2)),
			}

			result, err := alloc.Allocate(ctx, nc, claims)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should skip already-allocated devices", func() {
			allocated := sets.New[cloudprovider.DeviceID](
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID,
				deviceID("gpu.example.com", "pool-a", "gpu-1").DeviceID,
				deviceID("gpu.example.com", "pool-a", "gpu-2").DeviceID,
			)
			alloc = dynamicresources.NewAllocator(inClusterSlices, allocated, nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should allocate remaining devices when some are already allocated", func() {
			allocated := sets.New[cloudprovider.DeviceID](
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID,
				deviceID("gpu.example.com", "pool-a", "gpu-1").DeviceID,
			)
			alloc = dynamicresources.NewAllocator(inClusterSlices, allocated, nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("CEL selector filtering", func() {
		var inClusterSlices []dynamicresources.ResourceSlice

		BeforeEach(func() {
			inClusterSlices = []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/model": {StringValue: ptr.To("H100")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/model": {StringValue: ptr.To("A100")},
						}),
						deviceWithAttrs("gpu-2", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/model": {StringValue: ptr.To("H100")},
						}),
					),
				),
			}

			ExpectApplied(ctx, env.Client,
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "h100"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].model == "H100"`}},
						},
					},
				},
			)
		})

		It("should only allocate devices matching the selector", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "h100", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should fail when not enough devices match the selector", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "h100", 3))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should filter with request-level selectors", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1",
				exactRequestWithSelector("req-1", "gpu", 1, `device.attributes["gpu.example.com"].model == "A100"`),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("Constraint satisfaction", func() {
		var inClusterSlices []dynamicresources.ResourceSlice

		BeforeEach(func() {
			inClusterSlices = []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-2", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
						}),
						deviceWithAttrs("gpu-3", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
						}),
					),
				),
			}
		})

		It("should satisfy MatchAttribute constraints", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should backtrack to satisfy constraints", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			// Request 3 devices that must share a NUMA node — only 2 per NUMA, so this should fail.
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 3),
			)

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should satisfy constraints with backtracking across requests", func() {
			// 2 devices on node-0, 2 on node-1. Two requests of 2 each.
			// Each constraint is scoped to one request, so req-1 gets node-0 pair and req-2 gets node-1 pair (or vice versa).
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{
						MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa")),
						Requests:       []string{"req-1"},
					},
					{
						MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa")),
						Requests:       []string{"req-2"},
					},
				},
				exactRequest("req-1", "gpu", 2),
				exactRequest("req-2", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("Single IT with templates", func() {
		var inClusterSlices []dynamicresources.ResourceSlice

		BeforeEach(func() {
			inClusterSlices = []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1")),
			}
		})

		It("should allocate from templates when in-cluster devices are insufficient", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1"),
			)
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 3))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.InstanceTypes).To(HaveLen(1))
		})

		It("should prefer in-cluster devices over templates", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1"),
			)
			// Request 2 devices — should be satisfied entirely by in-cluster.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("Multi-IT allocation", func() {
		It("should prune instance types that cannot satisfy requests", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// it-large has 2 template devices, it-small has 0.
			nc := makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
				"it-large": {makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1")},
				"it-small": {},
			})
			// Request 3 devices: 1 in-cluster + 2 template needed.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 3))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			// Only it-large should survive.
			Expect(result.InstanceTypes).To(HaveLen(1))
			Expect(result.InstanceTypes[0].Value()).To(Equal("it-large"))
		})

		It("should keep all instance types that can satisfy requests", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			nc := makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
				"it-a": {makeTemplate("gpu.example.com", "pool-b", "tgpu-0")},
				"it-b": {makeTemplate("gpu.example.com", "pool-c", "tgpu-0")},
			})
			// Request 2 devices: 1 in-cluster + 1 template. Both ITs have 1 template device.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(2))
		})

		It("should fail when no instance type can satisfy", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
				"it-a": {makeTemplate("gpu.example.com", "pool-b", "tgpu-0")},
				"it-b": {makeTemplate("gpu.example.com", "pool-c", "tgpu-0")},
			})
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 5))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Commit protocol", func() {
		It("should mark in-cluster devices as allocated after commit", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1", "gpu-2")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			// First allocation: 2 devices.
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result1, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Second allocation: should only have 1 device left.
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())
		})

		It("should update pool cache on commit", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result.Allocation.Commit(ctx)

			// Second allocation should succeed (uses cached pools).
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			result2, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result2).ToNot(BeNil())
		})
	})

	Describe("All-mode allocation", func() {
		It("should allocate all matching in-cluster devices", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1", "gpu-2")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", allRequest("req-1", "gpu"))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should allocate all matching in-cluster and template devices", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1"),
			)
			claim := makeClaim("c1", allRequest("req-1", "gpu"))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should include different template device counts per instance type", func() {
			// it-large has 3 template devices, it-small has 1.
			// All-mode should succeed for both, but they allocate different total counts.
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
				"it-large": {makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1", "tgpu-2")},
				"it-small": {makeTemplate("gpu.example.com", "pool-c", "tgpu-0")},
			})
			claim := makeClaim("c1", allRequest("req-1", "gpu"))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(2))
		})

		It("should fail when an already-allocated device is in the all set", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1")),
			}
			allocated := sets.New[cloudprovider.DeviceID](
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID,
			)
			alloc = dynamicresources.NewAllocator(inClusterSlices, allocated, nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", allRequest("req-1", "gpu"))

			// All mode requires every eligible device to be allocated, but gpu-0 is taken.
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should work with All-mode and ExactCount mixed in the same claim", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/type": {StringValue: ptr.To("compute")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/type": {StringValue: ptr.To("compute")},
						}),
						deviceWithAttrs("nic-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/type": {StringValue: ptr.To("network")},
						}),
					),
				),
			}
			ExpectApplied(ctx, env.Client,
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "compute"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].type == "compute"`}},
						},
					},
				},
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "network"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].type == "network"`}},
						},
					},
				},
			)

			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1",
				allRequest("all-compute", "compute"),
				exactRequest("one-nic", "network", 1),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should satisfy MatchAttribute constraints in All mode", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				allRequest("req-1", "gpu"),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("Cross-NodeClaim device contention", func() {
		var inClusterSlices []dynamicresources.ResourceSlice

		BeforeEach(func() {
			inClusterSlices = []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1", "gpu-2")),
			}
		})

		It("should block devices allocated by a different NodeClaim", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// NC-A allocates 2 devices and commits.
			ncA := makeNodeClaimWithID("nc-a", "it-1")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// NC-B should only see 1 remaining device.
			ncB := makeNodeClaimWithID("nc-b", "it-1")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			// NC-B can allocate 1 device.
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 1))
			result3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(result3).ToNot(BeNil())
		})

		It("should allow the same device on the same NodeClaim for a different instance type", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// NC-A with IT-A allocates 2 devices.
			ncA := makeNodeClaimWithID("nc-a", "it-a")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Same NC-A, but with IT-B: should be able to allocate the same in-cluster devices
			// because only one IT will be provisioned.
			ncAWithITB := makeNodeClaimWithID("nc-a", "it-b")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			result2, err := alloc.Allocate(ctx, ncAWithITB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result2).ToNot(BeNil())
		})

		It("should block the same device on the same NodeClaim and same instance type from a prior pod", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// Pod 1 allocates 2 devices for NC-A/IT-A and commits.
			ncA := makeNodeClaimWithID("nc-a", "it-a")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Pod 2 on same NC-A/IT-A: only 1 device remains.
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			_, err = alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 1))
			result3, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(result3).ToNot(BeNil())
		})

		It("should handle multi-IT NodeClaim device contention across NodeClaims", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// NC-A has two ITs and allocates 2 devices per IT.
			ncA := &fakeNodeClaim{
				id:             unique.Make("nc-a"),
				nodePoolID:     unique.Make("test-np"),
				requirements:   scheduling.NewRequirements(),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-a"), unique.Make("it-b")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			Expect(result1.InstanceTypes).To(HaveLen(2))
			result1.Allocation.Commit(ctx)

			// NC-B: devices used by NC-A (any IT) are blocked.
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ReleaseInstanceType", func() {
		var inClusterSlices []dynamicresources.ResourceSlice

		BeforeEach(func() {
			inClusterSlices = []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1")),
			}
		})

		It("should free devices for other NodeClaims after releasing the only instance type", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// NC-A allocates and commits.
			ncA := makeNodeClaimWithID("nc-a", "it-1")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// NC-B can't allocate — devices are reserved.
			ncB := makeNodeClaimWithID("nc-b", "it-1")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			// Release NC-A's IT.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-1"))

			// NC-B can now allocate.
			result3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result3).ToNot(BeNil())
		})

		It("should free devices only when the last instance type referencing them is released", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// NC-A with two ITs allocates and commits.
			ncA := &fakeNodeClaim{
				id:             unique.Make("nc-a"),
				nodePoolID:     unique.Make("test-np"),
				requirements:   scheduling.NewRequirements(),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-a"), unique.Make("it-b")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result.Allocation.Commit(ctx)

			// Release only it-a — it-b still holds the devices.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))

			// NC-B still can't allocate.
			ncB := makeNodeClaimWithID("nc-b", "it-1")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			// Release it-b — now devices are free.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-b"))

			result3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result3).ToNot(BeNil())
		})

		It("should be a no-op when releasing an instance type that was never committed", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// Release a non-existent NC/IT — should not panic.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-nonexistent"), unique.Make("it-nonexistent"))

			// Allocation should still work normally.
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("Topology requirement narrowing", func() {
		It("should accumulate topology requirements from zonal devices", func() {
			// Two zonal slices: one in us-west-2a, one in us-west-2b.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-us-west-2a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-us-west-2b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("gpu-1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			// The result should carry topology requirements from the allocated device's zone.
			Expect(result.Requirements).ToNot(BeEmpty())
			Expect(result.Requirements.Has(corev1.LabelTopologyZone)).To(BeTrue())
			Expect(result.Requirements.Get(corev1.LabelTopologyZone).Values()).To(HaveLen(1))
			expectedZone := result.Requirements.Get(corev1.LabelTopologyZone).Values()[0]

			result.Allocation.Commit(ctx)
			meta := alloc.ResourceClaimAllocationMetadataForClaim("c1")
			Expect(meta).ToNot(BeNil())
			Expect(meta.Devices).To(HaveKey(unique.Make("it-1")))
			Expect(meta.Devices[unique.Make("it-1")]).To(HaveLen(1))
			Expect(meta.Devices[unique.Make("it-1")][0].Pool.Value()).To(ContainSubstring(expectedZone))
		})

		It("should narrow pools when a zonal device tightens requirements", func() {
			// Two pools in different zones. Request 2 devices — must come from the same zone
			// since the first device tightens requirements and eliminates the other zone's pool.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0", "gpu-a1"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("gpu-b0", "gpu-b1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			// 2 devices from the "gpu" class — both must come from pool-zone-a since the first
			// allocation tightens to us-west-2a which eliminates pool-zone-b.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should backtrack and restore requirements when a zonal device path fails", func() {
			// Zone A has 1 GPU, Zone B has 2 GPUs. Request 2 GPUs.
			// The DFS picks zone A first (tightens to zone A), but only 1 device there → backtracks.
			// Requirements are restored to the broad set, then zone B succeeds.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("gpu-b0", "gpu-b1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should reject a device whose topology is incompatible with accumulated requirements", func() {
			// Only one zone available, but the device is in a different zone.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "eu-west-1a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			// The device is in eu-west-1a but the NC requires us-west-2a → pool is excluded.
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Uncommitted allocation state isolation", func() {
		It("should not reserve devices when allocation is not committed", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			// Allocate but don't commit.
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			// Deliberately not calling Commit().

			// Second allocation should see all devices still available.
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			result2, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result2).ToNot(BeNil())
		})

		It("should not reserve devices for a different NodeClaim when not committed", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			ncA := makeNodeClaimWithID("nc-a", "it-1")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			_, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			// Not committed.

			// NC-B should see all devices as available.
			ncB := makeNodeClaimWithID("nc-b", "it-1")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			result2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result2).ToNot(BeNil())
		})
	})

	Describe("Template device tracking after commit", func() {
		It("should block template devices for the same NC/IT after commit", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1"),
			)

			// Pod 1: allocate both template devices.
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result1, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Pod 2: same NC/IT, no devices left.
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())
		})

		It("should allow template devices for a different IT on the same NC after commit", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// NC-A with IT-A has 2 template devices.
			ncAITA := makeNodeClaimWithTemplatesAndID("nc-a", "it-a",
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1"),
			)
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result1, err := alloc.Allocate(ctx, ncAITA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Same NC-A but IT-B has its own template devices.
			ncAITB := makeNodeClaimWithTemplatesAndID("nc-a", "it-b",
				makeTemplate("gpu.example.com", "pool-c", "tgpu-0", "tgpu-1"),
			)
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			result2, err := alloc.Allocate(ctx, ncAITB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result2).ToNot(BeNil())
		})
	})

	Describe("Constraint + template integration", func() {
		It("should backtrack from in-cluster to template devices to satisfy constraints", func() {
			// In-cluster: 2 GPUs with different NUMA nodes. Templates: 2 GPUs with same NUMA.
			// Request 2 GPUs with NUMA constraint. In-cluster can't satisfy (different NUMA),
			// so backtracking should find the template devices.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
						}),
					),
				),
			}

			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplateWithAttrs("gpu.example.com", "pool-b",
					deviceWithAttrs("tgpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-2")},
					}),
					deviceWithAttrs("tgpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-2")},
					}),
				),
			)
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should satisfy multiple constraints on the same claim", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
							"gpu.example.com/pcie": {StringValue: ptr.To("root-0")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
							"gpu.example.com/pcie": {StringValue: ptr.To("root-0")},
						}),
						deviceWithAttrs("gpu-2", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
							"gpu.example.com/pcie": {StringValue: ptr.To("root-1")},
						}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/pcie"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should fail when multiple constraints cannot be simultaneously satisfied", func() {
			// NUMA and PCIE constraints: gpu-0 and gpu-1 share NUMA but not PCIE,
			// gpu-0 and gpu-2 share PCIE but not NUMA. No pair satisfies both.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
							"gpu.example.com/pcie": {StringValue: ptr.To("root-0")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
							"gpu.example.com/pcie": {StringValue: ptr.To("root-1")},
						}),
						deviceWithAttrs("gpu-2", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
							"gpu.example.com/pcie": {StringValue: ptr.To("root-0")},
						}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/pcie"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should use attribute binding fallback end-to-end through Allocate", func() {
			// Template devices lack the "numa" attribute, but bindings declare tgpu-0 and tgpu-1 share it.
			devA := deviceID("gpu.example.com", "pool-b", "tgpu-0")
			devB := deviceID("gpu.example.com", "pool-b", "tgpu-1")
			devC := deviceID("gpu.example.com", "pool-b", "tgpu-2")

			bindings := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"test-np": {
					&cloudprovider.InstanceType{
						Name: "it-1",
						DynamicResources: cloudprovider.DynamicResources{
							AttributeBindings: []*cloudprovider.AttributeBinding{
								{
									Attribute: "gpu.example.com/numa",
									Devices:   []cloudprovider.DeviceID{devA.DeviceID, devB.DeviceID},
								},
							},
						},
					},
				},
			})

			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), bindings, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1", "tgpu-2"),
			)
			// Request 2 with NUMA constraint. tgpu-0 and tgpu-1 are bound, tgpu-2 is not.
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())

			// Request 3 should fail — only 2 are bound, tgpu-2 can't join.
			claim3 := makeClaimWithConstraints("c3",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 3),
			)
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())

			// Suppress unused variable warning.
			_ = devC
		})
	})

	Describe("Multi-claim competition", func() {
		It("should consume devices across claims within a single Allocate call", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1", "gpu-2")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			// Two claims competing: claim 1 wants 2, claim 2 wants 2. Only 3 available.
			claims := []*resourcev1.ResourceClaim{
				makeClaim("c1", exactRequest("req-1", "gpu", 2)),
				makeClaim("c2", exactRequest("req-1", "gpu", 2)),
			}
			_, err := alloc.Allocate(ctx, nc, claims)
			Expect(err).To(HaveOccurred())
		})

		It("should succeed when claims fit within the total device count", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1", "gpu-2", "gpu-3")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			claims := []*resourcev1.ResourceClaim{
				makeClaim("c1", exactRequest("req-1", "gpu", 2)),
				makeClaim("c2", exactRequest("req-1", "gpu", 2)),
			}
			result, err := alloc.Allocate(ctx, nc, claims)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should maintain independent constraints across claims", func() {
			// 2 GPUs on node-0, 2 on node-1. Two claims each with NUMA constraint requesting 2.
			// Each claim should independently find a NUMA pair.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-2", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
						}),
						deviceWithAttrs("gpu-3", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
						}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			claims := []*resourcev1.ResourceClaim{
				makeClaimWithConstraints("c1",
					[]resourcev1.DeviceConstraint{
						{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
					},
					exactRequest("req-1", "gpu", 2),
				),
				makeClaimWithConstraints("c2",
					[]resourcev1.DeviceConstraint{
						{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
					},
					exactRequest("req-1", "gpu", 2),
				),
			}

			result, err := alloc.Allocate(ctx, nc, claims)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("Multi-pool devices", func() {
		It("should allocate devices from multiple pools", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0")),
				makeAPISlice("s2", "gpu.example.com", "pool-b", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-1")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should treat same device name in different pools as distinct", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("dev-0")),
				makeAPISlice("s2", "gpu.example.com", "pool-b", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("dev-0")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("Combined class and request selectors", func() {
		It("should require both class and request selectors to match", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/model":  {StringValue: ptr.To("H100")},
							"gpu.example.com/memory": {IntValue: ptr.To(int64(80))},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/model":  {StringValue: ptr.To("H100")},
							"gpu.example.com/memory": {IntValue: ptr.To(int64(40))},
						}),
						deviceWithAttrs("gpu-2", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/model":  {StringValue: ptr.To("A100")},
							"gpu.example.com/memory": {IntValue: ptr.To(int64(80))},
						}),
					),
				),
			}

			ExpectApplied(ctx, env.Client,
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "h100-class"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].model == "H100"`}},
						},
					},
				},
			)

			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			// Class requires H100, request requires memory > 60. Only gpu-0 matches both.
			claim := makeClaim("c1",
				exactRequestWithSelector("req-1", "h100-class", 1, `device.attributes["gpu.example.com"].memory > 60`),
			)
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())

			// Requesting 2 should fail — only 1 device matches both selectors.
			claim2 := makeClaim("c2",
				exactRequestWithSelector("req-1", "h100-class", 2, `device.attributes["gpu.example.com"].memory > 60`),
			)
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Edge cases", func() {
		It("should succeed with templates when in-cluster satisfies everything", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1"),
			)
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should fail with zero instance types", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:             unique.Make("test-nc"),
				nodePoolID:     unique.Make("test-np"),
				requirements:   scheduling.NewRequirements(),
				instanceTypes:  nil,
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("In-cluster allocated claim handling", func() {
		It("should pass through claims with no nodeSelector", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeAllocatedClaim("c1", nil)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(1))
		})

		It("should propagate topology requirements from the allocation nodeSelector", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeAllocatedClaim("c1", &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2a"}},
					}},
				},
			})

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requirements).ToNot(BeNil())
			// Requirements should include the zone from the allocated claim.
			Expect(result.Requirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))
		})

		It("should fail when the allocation nodeSelector is incompatible with NodeClaim requirements", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeAllocatedClaim("c1", &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"eu-west-1a"}},
					}},
				},
			})

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("incompatible"))
		})

		It("should tighten baseline requirements for subsequent unallocated claims", func() {
			// An in-cluster allocated claim pins zone to us-west-2a.
			// An unallocated claim needs a device from a zonal pool.
			// Only the us-west-2a pool should be available.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("gpu-b0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}

			allocatedClaim := makeAllocatedClaim("c-allocated", &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2a"}},
					}},
				},
			})
			unallocatedClaim := makeClaim("c-unalloc", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{allocatedClaim, unallocatedClaim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			// The zone should be tightened to us-west-2a from the allocated claim.
			Expect(result.Requirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))
		})

		It("should handle a mix of allocated and unallocated claims", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			claims := []*resourcev1.ResourceClaim{
				makeAllocatedClaim("c-already-done", nil),
				makeClaim("c-pending", exactRequest("req-1", "gpu", 1)),
			}
			result, err := alloc.Allocate(ctx, nc, claims)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should return early when all claims are already allocated", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			claims := []*resourcev1.ResourceClaim{
				makeAllocatedClaim("c1", nil),
				makeAllocatedClaim("c2", nil),
			}
			result, err := alloc.Allocate(ctx, nc, claims)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(1))
		})

		It("should fail when two in-cluster allocated claims have incompatible zones", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}

			claims := []*resourcev1.ResourceClaim{
				makeAllocatedClaim("c-zone-a", &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{MatchExpressions: []corev1.NodeSelectorRequirement{
							{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2a"}},
						}},
					},
				}),
				makeAllocatedClaim("c-zone-b", &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{MatchExpressions: []corev1.NodeSelectorRequirement{
							{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2b"}},
						}},
					},
				}),
			}

			_, err := alloc.Allocate(ctx, nc, claims)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("incompatible"))
		})

		It("should succeed when two in-cluster allocated claims have compatible zones", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}

			claims := []*resourcev1.ResourceClaim{
				makeAllocatedClaim("c1", &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{MatchExpressions: []corev1.NodeSelectorRequirement{
							{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2a"}},
						}},
					},
				}),
				makeAllocatedClaim("c2", &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{MatchExpressions: []corev1.NodeSelectorRequirement{
							{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2a"}},
						}},
					},
				}),
			}

			result, err := alloc.Allocate(ctx, nc, claims)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.Requirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))
		})

		It("should fail when an in-cluster claim and in-memory claim have incompatible zones", func() {
			// First: allocate a claim on NC-A that pins to us-west-2a via a zonal device.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			ncA := &fakeNodeClaim{
				id:         unique.Make("nc-a"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			inMemoryClaim := makeClaim("zonal-claim", exactRequest("req-1", "gpu", 1))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{inMemoryClaim})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Now: a pod references both the in-memory claim (zone A) and an in-cluster
			// allocated claim pinned to zone B. These should be incompatible.
			ncB := &fakeNodeClaim{
				id:         unique.Make("nc-b"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			inClusterClaim := makeAllocatedClaim("cluster-claim", &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2b"}},
					}},
				},
			})

			// Order: in-cluster claim first (pins to 2b), then in-memory claim (needs 2a) → incompatible.
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{inClusterClaim, inMemoryClaim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("incompatible"))
		})
	})

	Describe("Cross-IT requirement accumulation", func() {
		It("should prune later instance types when earlier IT pins topology via in-cluster device", func() {
			// Two zone pools: zone-A has 2 devices, zone-B has 2 devices.
			// Request 2 devices. IT ordering: [it-a, it-b].
			// it-a evaluates first: DFS picks zone-A devices (first in pool order).
			// This tightens accumulated requirements to zone-A.
			// it-b evaluates second: zone-B devices are now excluded by requirements.
			// But it-b can still use zone-A devices (same pool). So it-b also succeeds.
			//
			// To demonstrate pruning, we need IT-B to ONLY have access to zone-B devices.
			// We can achieve this by giving IT-B template devices in a pool that doesn't
			// overlap with zone-A, and having no in-cluster devices available for IT-B
			// (all zone-A devices consumed by IT-A is not the case since state resets per IT).
			//
			// Actually, per allocator.go:487, state resets per IT (restoreState), but requirements
			// are NOT reset (line 748 comment). So IT-B starts fresh on devices but with tightened reqs.
			// Zone-B in-cluster devices are filtered out by FilterPools using the tightened requirements.
			// If zone-A has exactly 2 devices and IT-B needs 2, it will find zone-A devices too.
			//
			// The only way to prune IT-B is if zone-A doesn't have enough devices for IT-B,
			// and zone-B is excluded. Let's set: zone-A has 1 device, zone-B has 1 device.
			// Request 1 device. IT-A picks zone-A, tightens to zone-A. IT-B also picks zone-A. Both pass.
			//
			// For pruning: zone-A has 1 device. Request 2 devices. IT-A has 1 template + 1 in-cluster = 2.
			// Requirements tighten to zone-A. IT-B has 1 template too but the in-cluster zone-B device
			// is now filtered out, leaving only 1 in-cluster (zone-A) + 1 template = 2. So IT-B also passes.
			//
			// Hmm - the issue is that in-cluster devices are shared across ITs and templates are per-IT.
			// The pruning happens when after tightening, an IT can't find enough devices.
			//
			// Simplest approach: use ONLY in-cluster devices, 1 per zone. Request 2.
			// IT-A tries zone-A first, picks it. Needs 1 more. Zone-B still available at this point
			// (requirements haven't tightened yet during DFS for the same IT). IT-A picks zone-B too.
			// Wait - but zone-A device tightens requirements, zone-B device would be incompatible.
			// So IT-A picks zone-A (tightens to A), then zone-B fails compatibility check, backtracks.
			// IT-A can't satisfy 2 with only 1 zone-A device. IT-A fails.
			// Then IT-B tries similarly and also fails. Both pruned = error.
			//
			// Let me try: zone-A has 2 devices, zone-B has 2 devices. Request 2.
			// IT-A picks zone-A gpu-a0 (tightens to A), picks zone-A gpu-a1. IT-A succeeds with zone-A.
			// Requirements now tightened to zone-A for subsequent ITs.
			// IT-B starts with tightened requirements (zone-A only). FilterPools returns only zone-A pool.
			// IT-B picks zone-A gpu-a0, gpu-a1. IT-B succeeds. Both pass. No pruning.
			//
			// To force pruning of IT-B: make zone-A have only 1 device, give IT-A a template.
			// zone-A: 1 device. IT-A has 1 template. Request 2. IT-A: 1 in-cluster (zone-A) + 1 template = 2. Success.
			// Requirements tighten to zone-A. IT-B has 1 template too. IT-B: 1 in-cluster (zone-A) + 1 template = 2. Also passes.
			//
			// The cross-IT tightening prevents the scenario where IT-A would choose zone-A and IT-B would choose zone-B.
			// The test should verify that IT-B CAN'T choose zone-B after IT-A chose zone-A.
			// With 2 devices per zone and request=2, without cross-IT tightening both could succeed independently,
			// but with tightening IT-B is forced to also use zone-A.
			//
			// Best approach for demonstrating pruning:
			// zone-A: 1 device. zone-B: 1 device. IT-A gets 1 template. IT-B gets NO template.
			// Request 2. IT-A: 1 zone-A in-cluster + 1 template = 2. Succeeds (zone-A tightened).
			// IT-B: requirements tightened to zone-A. Only zone-A pool visible. Only 1 device. No template. Fails. Pruned.
			inClusterSlices2 := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("gpu-b0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices2, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			nc2 := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes: []dynamicresources.InstanceTypeID{unique.Make("it-a"), unique.Make("it-b")},
				resourceSlices: map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice{
					unique.Make("it-a"): {dynamicresources.NewTemplateSlice(makeTemplate("gpu.example.com", "pool-tmpl", "tgpu-0"))},
					unique.Make("it-b"): {},
				},
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			// Only IT-A should survive (it has template to supplement zone-A).
			// IT-B is pruned because after zone-A tightening it only has 1 device.
			Expect(result.InstanceTypes).To(HaveLen(1))
			Expect(result.InstanceTypes[0].Value()).To(Equal("it-a"))
		})

		It("should allow instance types with compatible topology after prior IT contributions", func() {
			// Both ITs can use zone-A devices. After IT-A tightens to zone-A, IT-B also succeeds.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0", "gpu-a1"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("gpu-b0", "gpu-b1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-a"), unique.Make("it-b")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			// Both ITs succeed using zone-A (or zone-B, whichever is first).
			// Either way, both survive because there are 2 devices in the pinned zone.
			Expect(result.InstanceTypes).To(HaveLen(2))
		})
	})

	Describe("ReleaseInstanceType requirement recomputation", func() {
		It("should relax TotalRequirements when a restricting instance type is released", func() {
			// Both ITs allocate the same zone-A in-cluster device and contribute zone-A.
			// After releasing one IT, TotalRequirements still has zone-A from the remaining IT.
			// After releasing both, TotalRequirements is empty.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-a"), unique.Make("it-b")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(2))
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim("c1")
			Expect(meta).ToNot(BeNil())
			// Both ITs contributed zone-A from the in-cluster device.
			Expect(meta.TotalRequirements.Has(corev1.LabelTopologyZone)).To(BeTrue())
			Expect(meta.TotalRequirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))

			// Release IT-A. IT-B still contributes zone-A.
			alloc.ReleaseInstanceType(ctx, unique.Make("test-nc"), unique.Make("it-a"))
			Expect(meta.TotalRequirements.Has(corev1.LabelTopologyZone)).To(BeTrue())
			Expect(meta.TotalRequirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))

			// Release IT-B. No ITs remain → TotalRequirements should be empty.
			alloc.ReleaseInstanceType(ctx, unique.Make("test-nc"), unique.Make("it-b"))
			Expect(meta.TotalRequirements).To(BeEmpty())
		})

		It("should empty TotalRequirements when all instance types are released", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0", "gpu-a1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-a"), unique.Make("it-b")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim("c1")
			Expect(meta).ToNot(BeNil())
			Expect(meta.TotalRequirements.Has(corev1.LabelTopologyZone)).To(BeTrue())

			// Release both ITs.
			alloc.ReleaseInstanceType(ctx, unique.Make("test-nc"), unique.Make("it-a"))
			alloc.ReleaseInstanceType(ctx, unique.Make("test-nc"), unique.Make("it-b"))

			Expect(meta.TotalRequirements).To(BeEmpty())
		})

		It("should correctly intersect remaining ContributedRequirements after partial release", func() {
			// Three ITs all allocating zone-A devices. Release middle IT. TotalRequirements remains zone-A.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0", "gpu-a1", "gpu-a2"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes: []dynamicresources.InstanceTypeID{unique.Make("it-a"), unique.Make("it-b"), unique.Make("it-c")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(3))
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim("c1")
			Expect(meta).ToNot(BeNil())
			Expect(meta.TotalRequirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))

			// Release IT-B. IT-A and IT-C remain, both contributed zone-A.
			alloc.ReleaseInstanceType(ctx, unique.Make("test-nc"), unique.Make("it-b"))

			// TotalRequirements should still be zone-A.
			Expect(meta.TotalRequirements.Has(corev1.LabelTopologyZone)).To(BeTrue())
			Expect(meta.TotalRequirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))
		})
	})

	Describe("Validation errors through Allocate()", func() {
		It("should return error when DeviceClass does not exist", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplate("gpu.example.com", "pool-a", "tgpu-0"),
			)
			claim := makeClaim("c1", exactRequest("req-1", "nonexistent-class", 1))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		It("should return error for unsupported selector type", func() {
			// Use a request-level non-CEL selector (API server validates class selectors).
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplate("gpu.example.com", "pool-a", "tgpu-0"),
			)
			// A claim with a request that has a non-CEL selector (empty DeviceSelector).
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req-1",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "gpu",
									Count:           1,
									Selectors: []resourcev1.DeviceSelector{
										{}, // No CEL field
									},
								},
							},
						},
					},
				},
			}

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported selector"))
		})

		It("should return error for FirstAvailable request", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplate("gpu.example.com", "pool-a", "tgpu-0"),
			)
			// A request with no Exactly field (FirstAvailable).
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{Name: "req-1"},
						},
					},
				},
			}

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("only Exactly requests"))
		})

		It("should return error for unsupported constraint type", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplate("gpu.example.com", "pool-a", "tgpu-0"),
			)
			// A claim with an empty constraint (no MatchAttribute set).
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Constraints: []resourcev1.DeviceConstraint{
							{}, // No MatchAttribute
						},
						Requests: []resourcev1.DeviceRequest{
							{Name: "req-1", Exactly: &resourcev1.ExactDeviceRequest{
								DeviceClassName: "gpu",
								Count:           1,
							}},
						},
					},
				},
			}

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported constraint"))
		})
	})

	Describe("Constraint path exclusion during DFS", func() {
		It("should fail when no consistent constraint evaluation path exists across devices", func() {
			// Device-A (in-cluster) has numa attribute concretely.
			// Device-B (template) does NOT have the attribute but IS bound via AttributeBindings.
			// The MatchAttribute constraint has two mutually exclusive paths:
			//   - Concrete: first device pins value, subsequent must match concretely
			//   - Binding: first device establishes binding, subsequent must be bound
			// If device-A is tried first → concrete path → device-B rejected (no attribute, can't use binding).
			// If device-B is tried first → binding path → device-A rejected (has concrete attribute, can't mix).
			// Result: no valid pair exists.
			devA := deviceID("gpu.example.com", "pool-tmpl", "tgpu-0")
			devB := deviceID("gpu.example.com", "pool-tmpl", "tgpu-1")

			bindings := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"test-np": {
					&cloudprovider.InstanceType{
						Name: "it-1",
						DynamicResources: cloudprovider.DynamicResources{
							AttributeBindings: []*cloudprovider.AttributeBinding{
								{
									Attribute: "gpu.example.com/numa",
									Devices:   []cloudprovider.DeviceID{devA.DeviceID, devB.DeviceID},
								},
							},
						},
					},
				},
			})

			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("dev-concrete", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
					),
				),
			}

			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), bindings, env.Client)
			// Template device WITHOUT the numa attribute (will use binding fallback path).
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplateWithAttrs("gpu.example.com", "pool-tmpl",
					deviceWithAttrs("tgpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{}),
				),
			)

			// Request 2 devices with MatchAttribute on numa.
			// dev-concrete has the attribute → concrete path.
			// tgpu-0 lacks attribute → binding path.
			// Paths are mutually exclusive → no valid pair.
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should succeed when all devices use binding path consistently", func() {
			// Both template devices lack the attribute but are bound to each other.
			devA := deviceID("gpu.example.com", "pool-tmpl", "tgpu-0")
			devB := deviceID("gpu.example.com", "pool-tmpl", "tgpu-1")

			bindings := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"test-np": {
					&cloudprovider.InstanceType{
						Name: "it-1",
						DynamicResources: cloudprovider.DynamicResources{
							AttributeBindings: []*cloudprovider.AttributeBinding{
								{
									Attribute: "gpu.example.com/numa",
									Devices:   []cloudprovider.DeviceID{devA.DeviceID, devB.DeviceID},
								},
							},
						},
					},
				},
			})

			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), bindings, env.Client)
			nc := makeNodeClaimWithTemplates("it-1",
				makeTemplateWithAttrs("gpu.example.com", "pool-tmpl",
					deviceWithAttrs("tgpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{}),
					deviceWithAttrs("tgpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{}),
				),
			)

			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should succeed when all devices use concrete path consistently", func() {
			// Both in-cluster devices have the attribute concretely with the same value.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
					),
				),
			}

			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("All-mode + ExactCount under shared constraint", func() {
		It("should satisfy MatchAttribute spanning All-mode and ExactCount requests", func() {
			// 2 in-cluster devices marked as "compute" with same NUMA.
			// 1 in-cluster device marked as "extra" with same NUMA.
			// Constraint on NUMA applies to both requests (All-mode "compute" + ExactCount "extra").
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/type": {StringValue: ptr.To("compute")},
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/type": {StringValue: ptr.To("compute")},
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-2", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/type": {StringValue: ptr.To("extra")},
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
					),
				),
			}
			ExpectApplied(ctx, env.Client,
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "compute-class"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].type == "compute"`}},
						},
					},
				},
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "extra-class"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].type == "extra"`}},
						},
					},
				},
			)

			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			// All "compute" devices + 1 "extra" device, all sharing NUMA constraint.
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				allRequest("all-compute", "compute-class"),
				exactRequest("one-extra", "extra-class", 1),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should fail when mixed-mode requests cannot share constraint value", func() {
			// All-mode devices have NUMA node-0, ExactCount device has NUMA node-1.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/type": {StringValue: ptr.To("compute")},
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/type": {StringValue: ptr.To("compute")},
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-2", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/type": {StringValue: ptr.To("extra")},
							"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
						}),
					),
				),
			}
			ExpectApplied(ctx, env.Client,
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "compute-class2"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].type == "compute"`}},
						},
					},
				},
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "extra-class2"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].type == "extra"`}},
						},
					},
				},
			)

			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				allRequest("all-compute", "compute-class2"),
				exactRequest("one-extra", "extra-class2", 1),
			)

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ContributedRequirements tracking", func() {
		It("should track ContributedRequirements independently per instance type", func() {
			// Both ITs allocate from the same zone-A pool (same in-cluster device).
			// Each IT should have its own entry in ContributedRequirements.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-a"), unique.Make("it-b")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(2))
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim("c1")
			Expect(meta).ToNot(BeNil())
			// Both ITs should have independent entries.
			Expect(meta.ContributedRequirements).To(HaveKey(unique.Make("it-a")))
			Expect(meta.ContributedRequirements).To(HaveKey(unique.Make("it-b")))
			// Both contributed zone-A.
			Expect(meta.ContributedRequirements[unique.Make("it-a")].Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))
			Expect(meta.ContributedRequirements[unique.Make("it-b")].Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))
			// TotalRequirements is the intersection: zone-A.
			Expect(meta.TotalRequirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))
		})

		It("should have empty TotalRequirements when IT uses non-topology device", func() {
			// IT allocates from an AllNodes pool (no topology). ContributedRequirements empty for that IT.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-global",
					withAllNodes(),
					withAPIDevices("gpu-g0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim("c1")
			Expect(meta).ToNot(BeNil())
			// AllNodes device has no topology → TotalRequirements should be empty.
			Expect(meta.TotalRequirements).To(BeEmpty())
		})

		It("should track ContributedRequirements independently per claim", func() {
			// Two zonal pools with different topology breadth: one with only zone=A, the other with
			// zone=A AND capacity-type=on-demand. Two claims each request from a different pool.
			// Each claim's ContributedRequirements should only contain the topology from its own device.
			// With the original bug (storing itReqs instead of claimITReqs), both claims would
			// incorrectly share the same itReqs object, giving both claims the union of all topology.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-only",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "nic.example.com", "pool-zone-and-ct",
					withRawNodeSelector(&corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{MatchExpressions: []corev1.NodeSelectorRequirement{
								{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2a"}},
								{Key: "karpenter.sh/capacity-type", Operator: corev1.NodeSelectorOpIn, Values: []string{"on-demand"}},
							}},
						},
					}),
					withAPIDevices("nic-0"),
					withGeneration(1, 1),
				),
			}
			ExpectApplied(ctx, env.Client,
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "nic"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.driver == "nic.example.com"`}},
						},
					},
				},
			)
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
					scheduling.NewRequirement("karpenter.sh/capacity-type", corev1.NodeSelectorOpIn, "on-demand", "spot"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claims := []*resourcev1.ResourceClaim{
				makeClaim("c1", exactRequest("req-1", "gpu", 1)),
				makeClaim("c2", exactRequest("req-1", "nic", 1)),
			}

			result, err := alloc.Allocate(ctx, nc, claims)
			Expect(err).ToNot(HaveOccurred())
			result.Allocation.Commit(ctx)

			metaC1 := alloc.ResourceClaimAllocationMetadataForClaim("c1")
			metaC2 := alloc.ResourceClaimAllocationMetadataForClaim("c2")
			Expect(metaC1).ToNot(BeNil())
			Expect(metaC2).ToNot(BeNil())

			// Claim 1 (zone-only pool): should only have zone=A, NOT capacity-type.
			c1Reqs := metaC1.ContributedRequirements[unique.Make("it-1")]
			Expect(c1Reqs.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))
			Expect(c1Reqs.Has("karpenter.sh/capacity-type")).To(BeFalse())

			// Claim 2 (zone+capacity-type pool): should have both zone=A and capacity-type=on-demand.
			c2Reqs := metaC2.ContributedRequirements[unique.Make("it-1")]
			Expect(c2Reqs.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))
			Expect(c2Reqs.Get("karpenter.sh/capacity-type").Values()).To(ConsistOf("on-demand"))
		})
	})

	Describe("All-mode validation failures through Allocate()", func() {
		It("should return error when All-mode pool has duplicate device names", func() {
			// Create two slices for the same pool with duplicate device names.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2), withAPIDevices("gpu-0", "gpu-1")),
				makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2), withAPIDevices("gpu-0", "gpu-2")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", allRequest("req-1", "gpu"))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid"))
		})

		It("should return error when All-mode pool is incomplete", func() {
			// Slice declares sliceCount=2 but only 1 slice exists.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2), withAPIDevices("gpu-0", "gpu-1")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", allRequest("req-1", "gpu"))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("incomplete"))
		})
	})

	Describe("Constraint scoped to request subset", func() {
		It("should allow non-scoped requests to cross constraint boundaries", func() {
			// 3 requests, constraint scoped to [req-a, req-b].
			// req-a and req-b must share NUMA. req-c can use a different NUMA.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					withAPIDevicesWithAttrs(
						deviceWithAttrs("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
						}),
						deviceWithAttrs("gpu-2", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
						}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{
						MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa")),
						Requests:       []string{"req-a", "req-b"},
					},
				},
				exactRequest("req-a", "gpu", 1),
				exactRequest("req-b", "gpu", 1),
				exactRequest("req-c", "gpu", 1),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("Multiple pools in All-mode", func() {
		It("should aggregate devices from multiple pools in All-mode", func() {
			// 3 pools with same driver, 1 device each.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0")),
				makeAPISlice("s2", "gpu.example.com", "pool-b", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-1")),
				makeAPISlice("s3", "gpu.example.com", "pool-c", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-2")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", allRequest("req-1", "gpu"))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("Empty pool set", func() {
		It("should fail when no pools match and no templates available", func() {
			// NC requires zone-C but all pools are zone-A/B.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("gpu-1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2c"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("In-memory allocated claim handling", func() {
		It("should skip DFS for an in-memory allocated claim on the same NodeClaim (in-cluster only)", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			nc := makeNodeClaim("it-1")

			// Pod 1: allocate and commit.
			claim := makeClaim("shared-claim", exactRequest("req-1", "gpu", 1))
			result1, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Pod 2: references the same claim (still unallocated in API, but in-memory allocated).
			// Should succeed without re-running the DFS.
			result2, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result2).ToNot(BeNil())
			Expect(result2.InstanceTypes).To(HaveLen(1))
		})

		It("should allow an in-memory in-cluster-only claim to be used from a different NodeClaim", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// Pod 1 on NC-A.
			ncA := makeNodeClaimWithID("nc-a", "it-1")
			claim := makeClaim("shared-claim", exactRequest("req-1", "gpu", 1))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Pod 2 on NC-B: same claim, should succeed since it used in-cluster devices only.
			ncB := makeNodeClaimWithID("nc-b", "it-1")
			result2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result2).ToNot(BeNil())
		})

		It("should fail when a template-allocated claim is referenced from a different NodeClaim", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// Pod 1 on NC-A with template devices.
			ncA := makeNodeClaimWithTemplatesAndID("nc-a", "it-1",
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0"),
			)
			claim := makeClaim("template-claim", exactRequest("req-1", "gpu", 1))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Pod 2 on NC-B: same claim, should fail since it used template devices.
			ncB := makeNodeClaimWithTemplatesAndID("nc-b", "it-2",
				makeTemplate("gpu.example.com", "pool-c", "tgpu-0"),
			)
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bound to a different in-flight NodeClaim"))
		})

		It("should succeed when a template-allocated claim is referenced from the same NodeClaim", func() {
			alloc = dynamicresources.NewAllocator(nil, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			// Pod 1 on NC-A with template devices.
			ncA := makeNodeClaimWithTemplatesAndID("nc-a", "it-1",
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1"),
			)
			claim := makeClaim("template-claim", exactRequest("req-1", "gpu", 1))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Pod 2 on same NC-A: should succeed.
			result2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result2).ToNot(BeNil())
		})

		It("should track UsedTemplateDevices per-claim, not globally", func() {
			// Pod 1 allocates two claims simultaneously: one from in-cluster, one from templates.
			// The in-cluster claim should be reusable from a different NC.
			// The template claim should NOT be reusable from a different NC.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0")),
			}

			ExpectApplied(ctx, env.Client,
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "fpga"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.driver == "fpga.example.com"`}},
						},
					},
				},
			)

			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			ncA := makeNodeClaimWithTemplatesAndID("nc-a", "it-1",
				makeTemplate("fpga.example.com", "pool-fpga", "fpga-0"),
			)

			inClusterClaim := makeClaim("in-cluster-claim", exactRequest("req-1", "gpu", 1))
			templateClaim := makeClaim("template-claim", exactRequest("req-1", "fpga", 1))

			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{inClusterClaim, templateClaim})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// The next pod has an identical template claim. This claim should fail to allocate since the only device that can
			// satisfy it is already allocated on this NodeClaim.
			templateClaim2 := makeClaim("template-claim-2", exactRequest("req-1", "fpga", 1))
			_, err = alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{templateClaim2})
			Expect(err).To(HaveOccurred())

			// We next try with both the in-cluster and template claim on a new NodeClaim. Neither device should be allocated
			// since the in-cluster device is already allocated to a different ResourceClaim on a different NodeClaim.
			ncB := makeNodeClaimWithTemplatesAndID("nc-b", "it-1",
				makeTemplate("fpga.example.com", "pool-fpga2", "fpga-0"),
			)
			inClusterClaim2 := makeClaim("in-cluster-claim-2", exactRequest("req-1", "gpu", 1))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{inClusterClaim2, templateClaim2})
			Expect(err).To(HaveOccurred())

			// Finally, we try to allocate the template device only on a new NodeClaim. This allocation should succeed since
			// the template device has not been previously allocated on this NodeClaim, even though it has been on others.
			result2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{templateClaim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result2).ToNot(BeNil())
		})

		It("should propagate in-memory topology requirements to the allocation result", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			ncA := &fakeNodeClaim{
				id:         unique.Make("nc-a"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}

			// Pod 1: allocate a zonal device, committing zone us-west-2a.
			claim := makeClaim("zonal-claim", exactRequest("req-1", "gpu", 1))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Pod 2 on NC-B: in-memory claim should propagate the zone requirement.
			ncB := &fakeNodeClaim{
				id:         unique.Make("nc-b"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			result2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result2.Requirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf("us-west-2a"))
		})

		It("should fail when in-memory topology requirements are incompatible with NodeClaim", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			ncA := &fakeNodeClaim{
				id:         unique.Make("nc-a"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}

			// Pod 1: allocate a zonal device pinning to us-west-2a.
			claim := makeClaim("zonal-claim", exactRequest("req-1", "gpu", 1))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			// Pod 2 on NC-B which only allows eu-west-1a: should fail.
			ncB := &fakeNodeClaim{
				id:         unique.Make("nc-b"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "eu-west-1a"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("incompatible"))
		})

		It("should merge in-memory requirements into the baseline for subsequent unallocated claims", func() {
			ExpectApplied(ctx, env.Client,
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-class"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.driver == "gpu.example.com"`}},
						},
					},
				},
				&resourcev1.DeviceClass{
					ObjectMeta: metav1.ObjectMeta{Name: "fpga-class"},
					Spec: resourcev1.DeviceClassSpec{
						Selectors: []resourcev1.DeviceSelector{
							{CEL: &resourcev1.CELDeviceSelector{Expression: `device.driver == "fpga.example.com"`}},
						},
					},
				},
			)

			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "gpu-pool-us-west-2a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "gpu-pool-us-west-2b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("gpu-b0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s3", "fpga.example.com", "fpga-pool-us-west-2a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("fpga-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s4", "fpga.example.com", "fpga-pool-us-west-2b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("fpga-b0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)
			ncA := &fakeNodeClaim{
				id:         unique.Make("nc-a"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpExists),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}

			claim1 := makeClaim("zonal-claim", exactRequest("req-1", "gpu-class", 1))
			result1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			result1.Allocation.Commit(ctx)

			claim1Meta := alloc.ResourceClaimAllocationMetadataForClaim("zonal-claim")
			Expect(claim1Meta).ToNot(BeNil())
			Expect(claim1Meta.TotalRequirements).ToNot(BeEmpty())
			Expect(claim1Meta.TotalRequirements.Has(corev1.LabelTopologyZone)).To(BeTrue())
			Expect(claim1Meta.TotalRequirements.Get(corev1.LabelTopologyZone).Values()).To(HaveLen(1))

			expectedZone := claim1Meta.TotalRequirements.Get(corev1.LabelTopologyZone).Values()[0]

			// The second pod we allocate references the previously allocated claim and a new claim. We should merge the
			// previously allocated claim's requirements into the baseline and only allocate a devices which satisfies those
			// constraints for the second claim.
			ncB := &fakeNodeClaim{
				id:         unique.Make("nc-b"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpExists),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claims := []*resourcev1.ResourceClaim{
				claim1,
				makeClaim("new-claim", exactRequest("req-1", "fpga-class", 1)),
			}
			result2, err := alloc.Allocate(ctx, ncB, claims)
			Expect(err).ToNot(HaveOccurred())
			Expect(result2).ToNot(BeNil())
			result2.Allocation.Commit(ctx)
			Expect(result2.Requirements.Has(corev1.LabelTopologyZone)).To(BeTrue())
			Expect(result2.Requirements.Get(corev1.LabelTopologyZone).Values()).To(HaveLen(1))
			Expect(result2.Requirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf(expectedZone))

			claim2Meta := alloc.ResourceClaimAllocationMetadataForClaim("new-claim")
			Expect(claim2Meta).ToNot(BeNil())
			Expect(claim2Meta.Devices).To(HaveKey(unique.Make("it-1")))
			claim2Devices := claim2Meta.Devices[unique.Make("it-1")]
			Expect(claim2Devices).To(HaveLen(1))
			Expect(claim2Devices[0].Pool.Value()).To(ContainSubstring(expectedZone))
		})

		It("should fail when two in-memory allocated claims have incompatible zones", func() {
			// Create two in-memory claims pinned to different zones.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("gpu-b0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, sets.New[cloudprovider.DeviceID](), nil, env.Client)

			broadReqs := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
			)

			// Pod 1 on NC-A: allocate from zone A.
			ncA := &fakeNodeClaim{
				id: unique.Make("nc-a"), nodePoolID: unique.Make("test-np"),
				requirements: broadReqs, instanceTypes: []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claimA := makeClaim("claim-zone-a", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claimA})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Pod 2 on NC-B: allocate from zone B.
			ncB := &fakeNodeClaim{
				id: unique.Make("nc-b"), nodePoolID: unique.Make("test-np"),
				requirements: broadReqs, instanceTypes: []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claimB := makeClaim("claim-zone-b", exactRequest("req-1", "gpu", 1))
			r2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claimB})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Pod 3 references both in-memory claims. Zone A and Zone B are incompatible.
			ncC := &fakeNodeClaim{
				id: unique.Make("nc-c"), nodePoolID: unique.Make("test-np"),
				requirements: broadReqs, instanceTypes: []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			_, err = alloc.Allocate(ctx, ncC, []*resourcev1.ResourceClaim{claimA, claimB})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("incompatible"))
		})
	})
})
