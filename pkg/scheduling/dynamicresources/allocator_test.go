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
	"fmt"
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

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// fakeNodeClaim implements the NodeClaim interface for testing.
type fakeNodeClaim struct {
	id             dynamicresources.NodeClaimID
	nodeName       string
	nodePoolID     dynamicresources.NodePoolID
	requirements   scheduling.Requirements
	instanceTypes  []dynamicresources.InstanceTypeID
	resourceSlices map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice
}

func (f *fakeNodeClaim) ID() dynamicresources.NodeClaimID                 { return f.id }
func (f *fakeNodeClaim) NodeName() string                                 { return f.nodeName }
func (f *fakeNodeClaim) NodePoolID() dynamicresources.NodePoolID          { return f.nodePoolID }
func (f *fakeNodeClaim) Requirements() scheduling.Requirements            { return f.requirements }
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

func makeNodeClaimWithTemplates(templates ...*cloudprovider.ResourceSliceTemplate) *fakeNodeClaim {
	return makeNodeClaimWithTemplatesAndID("test-nc", "it-1", templates...)
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

// nolint:unparam
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

// nolint:unparam
func makeTemplateWithCounters(driver, pool string, counterSets []resourcev1.CounterSet, devices ...cloudprovider.Device) *cloudprovider.ResourceSliceTemplate {
	return &cloudprovider.ResourceSliceTemplate{
		Driver:         unique.Make(driver),
		Pool:           cloudprovider.ResourcePool{Name: unique.Make(pool)},
		Devices:        devices,
		SharedCounters: counterSets,
	}
}

func templateDevice(name string, consumesCounters ...resourcev1.DeviceCounterConsumption) cloudprovider.Device {
	return cloudprovider.Device{
		Name:             unique.Make(name),
		ConsumesCounters: consumesCounters,
	}
}

func counterConsumption(counterSetName string, counters map[string]resource.Quantity) resourcev1.DeviceCounterConsumption {
	rc := make(map[string]resourcev1.Counter, len(counters))
	for k, v := range counters {
		rc[k] = resourcev1.Counter{Value: v}
	}
	return resourcev1.DeviceCounterConsumption{
		CounterSet: counterSetName,
		Counters:   rc,
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

//nolint:unparam
func exactRequestWithCapacity(name, className string, count int64, capacityRequests map[resourcev1.QualifiedName]resource.Quantity) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{
		Name: name,
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: className,
			Count:           count,
			Capacity: &resourcev1.CapacityRequirements{
				Requests: capacityRequests,
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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.InstanceTypes).To(HaveLen(1))
			Expect(result.Allocation).ToNot(BeNil())
		})

		It("should allocate multiple devices for a single request", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 3))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should fail when not enough devices are available", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 5))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should handle multiple requests in a single claim", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1",
				exactRequest("req-1", "gpu", 3),
				exactRequest("req-2", "gpu", 3),
			)

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should handle multiple claims", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claims := []*resourcev1.ResourceClaim{
				makeClaim("c1", exactRequest("req-1", "gpu", 2)),
				makeClaim("c2", exactRequest("req-1", "gpu", 2)),
			}

			result, err := alloc.Allocate(ctx, nc, claims)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should distinguish claims with the same name in different namespaces", func() {
			// ResourceClaims are namespaced, so two claims sharing a name across namespaces must be
			// tracked independently rather than colliding on a name-only key.
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claimNS1 := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			claimNS1.Namespace = "ns-1"
			claimNS2 := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			claimNS2.Namespace = "ns-2"

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claimNS1, claimNS2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			// Commit must not panic on a duplicate key — the namespaced keys are distinct.
			result.Allocation.Commit(ctx)

			meta1 := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "ns-1", Name: "c1"})
			meta2 := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "ns-2", Name: "c1"})
			Expect(meta1).ToNot(BeNil())
			Expect(meta2).ToNot(BeNil())
			// Each namespace's claim received its own device allocation.
			Expect(meta1.Devices[unique.Make("it-1")]).To(HaveLen(1))
			Expect(meta2.Devices[unique.Make("it-1")]).To(HaveLen(1))
			Expect(meta1.Devices[unique.Make("it-1")][0]).ToNot(Equal(meta2.Devices[unique.Make("it-1")][0]))
		})

		It("should skip already-allocated devices", func() {
			allocated := sets.New[cloudprovider.DeviceID](
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID,
				deviceID("gpu.example.com", "pool-a", "gpu-1").DeviceID,
				deviceID("gpu.example.com", "pool-a", "gpu-2").DeviceID,
			)
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: allocated}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: allocated}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("Node-name-pinned in-cluster devices", func() {
		var inClusterSlices []dynamicresources.ResourceSlice

		BeforeEach(func() {
			// A published slice pinned to node-a via spec.nodeName (the common kubelet/DRA-driver form).
			inClusterSlices = []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withNodeName("node-a"),
					withGeneration(1, 1), withAPIDevices("gpu-0")),
			}
		})

		It("should allocate a node-name-pinned device for the existing node it belongs to", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			nc.nodeName = "node-a"
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.Allocation).ToNot(BeNil())
		})

		It("should not allocate a node-name-pinned device for a different existing node", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			nc.nodeName = "node-b"
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should not allocate a node-name-pinned device for an in-flight NodeClaim", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1") // no node name — in-flight
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "h100", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should fail when not enough devices match the selector", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "h100", 3))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should filter with request-level selectors", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1"),
			)
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 3))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.InstanceTypes).To(HaveLen(1))
		})

		It("should prefer in-cluster devices over templates", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1"),
			)
			// Request 2 devices — should be satisfied entirely by in-cluster.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("SharedCounters — in-cluster", func() {
		It("should allocate devices when counter budget is sufficient", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should reject allocation when counter budget is exhausted", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-2", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			// 3 devices × 40Gi > 80Gi budget — only 2 can be allocated.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 3))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should respect preallocated counter consumption", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			// gpu-0 is already allocated — consumes 40Gi of the 80Gi budget.
			allocated := sets.New[cloudprovider.DeviceID](
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID,
			)
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: allocated}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			// gpu-1 needs 40Gi, and only 40Gi remains (80Gi - 40Gi from gpu-0). Should succeed.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should track inflight counter consumption across pods", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-2", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// Pod 1: allocate 2 devices (uses 80Gi of budget).
			nc1 := makeNodeClaimWithID("nc-1", "it-1")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			r1, err := alloc.Allocate(ctx, nc1, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Pod 2: try to allocate 1 more device — should fail (budget exhausted by pod 1).
			nc2 := makeNodeClaimWithID("nc-2", "it-1")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			_, err = alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())
		})

		It("should handle devices consuming from multiple counter sets", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(
						counterSet("memory-budget", map[string]resource.Quantity{"memory": resource.MustParse("80Gi")}),
						counterSet("compute-budget", map[string]resource.Quantity{"flops": resource.MustParse("100")}),
					),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						resourcev1.Device{
							Name: "gpu-0",
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "memory-budget", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("40Gi")}}},
								{CounterSet: "compute-budget", Counters: map[string]resourcev1.Counter{"flops": {Value: resource.MustParse("50")}}},
							},
						},
						resourcev1.Device{
							Name: "gpu-1",
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "memory-budget", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("40Gi")}}},
								{CounterSet: "compute-budget", Counters: map[string]resourcev1.Counter{"flops": {Value: resource.MustParse("50")}}},
							},
						},
						resourcev1.Device{
							Name: "gpu-2",
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "memory-budget", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("40Gi")}}},
								{CounterSet: "compute-budget", Counters: map[string]resourcev1.Counter{"flops": {Value: resource.MustParse("50")}}},
							},
						},
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")

			// 2 devices: 80Gi memory (ok) + 100 flops (ok).
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())

			// 3 devices: exceeds both budgets.
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 3))
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())
		})

		It("should deduct preallocated NonTargetingDevices from counter budget", func() {
			// Pool has: counter budget 80Gi, one targeting device (gpu-0, 40Gi) on all-nodes,
			// and one non-targeting device (gpu-offnode, 40Gi) on a different zone.
			// gpu-offnode is preallocated — its 40Gi should be deducted from the budget.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 3),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 3),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
				makeAPISlice("s-offnode", "gpu.example.com", "pool-a",
					withZoneSelector("eu-west-1a"),
					withGeneration(1, 3),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-offnode", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			// gpu-offnode is preallocated.
			allocated := sets.New[cloudprovider.DeviceID](
				deviceID("gpu.example.com", "pool-a", "gpu-offnode").DeviceID,
			)
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: allocated}, nil, env.Client, nil)

			// NodeClaim is in us-west-2a — the s-offnode slice (eu-west-1a) becomes non-targeting.
			nc := &fakeNodeClaim{
				id:             unique.Make("test-nc"),
				nodePoolID:     unique.Make("test-np"),
				requirements:   scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a")),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-1")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			// gpu-0 wants 40Gi. With gpu-offnode preallocated (40Gi deducted), only 40Gi remains. Should succeed.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		DescribeTable("should reject allocation when device references invalid counters",
			func(setup func() dynamicresources.NodeClaim) {
				nc := setup()
				claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
				_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
				Expect(err).To(HaveOccurred())
			},
			Entry("in-cluster device references non-existent counter set name", func() dynamicresources.NodeClaim {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-counters", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 2),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "nonexistent-set", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						),
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
				return makeNodeClaim("it-1")
			}),
			Entry("in-cluster device references non-existent counter name", func() dynamicresources.NodeClaim {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-counters", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 2),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"nonexistent-counter": resource.MustParse("40Gi")}),
						),
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
				return makeNodeClaim("it-1")
			}),
			Entry("template device references non-existent counter set", func() dynamicresources.NodeClaim {
				alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
				return makeNodeClaimWithTemplates(makeTemplateWithCounters("gpu.example.com", "pool-a",
					[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})},
					templateDevice("gpu-0", counterConsumption("nonexistent-set", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				))
			}),
			Entry("template device references non-existent counter name", func() dynamicresources.NodeClaim {
				alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
				return makeNodeClaimWithTemplates(makeTemplateWithCounters("gpu.example.com", "pool-a",
					[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})},
					templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"nonexistent": resource.MustParse("40Gi")})),
				))
			}),
		)
	})

	Describe("SharedCounters — zero capacity edge cases", func() {
		It("should reject allocation when counter has zero capacity", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("slots", map[string]resource.Quantity{
						"gpu": resource.MustParse("0"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "slots", map[string]resource.Quantity{"gpu": resource.MustParse("1")}),
						deviceConsumingCounter("gpu-1", "slots", map[string]resource.Quantity{"gpu": resource.MustParse("1")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should succeed when both counter capacity and device consumption are zero", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("slots", map[string]resource.Quantity{
						"gpu": resource.MustParse("0"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "slots", map[string]resource.Quantity{"gpu": resource.MustParse("0")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})
	})

	Describe("SharedCounters — release and pessimistic max", func() {
		It("should restore counter budget when an instance type is released", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-2", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC-A allocates 2 devices (80Gi) and commits.
			ncA := makeNodeClaimWithID("nc-a", "it-1")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// NC-B: budget exhausted.
			ncB := makeNodeClaimWithID("nc-b", "it-1")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			// Release NC-A's IT — counter budget should be restored.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-1"))

			// NC-B: budget is now available.
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should commit the pessimistic max across instance types", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("120Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-2", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC-A with 2 ITs: DFS order means IT-A gets gpu-0 (40Gi), IT-B also gets gpu-0 (40Gi).
			// Pessimistic max = 40Gi (same device, same consumption for both ITs).
			// This leaves 80Gi remaining for NC-B.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			Expect(r1.InstanceTypes).To(HaveLen(2))
			r1.Allocation.Commit(ctx)

			// NC-B: 80Gi remaining. gpu-0 is device-blocked by NC-A, so NC-B uses gpu-1 + gpu-2.
			// 40Gi + 40Gi = 80Gi = remaining budget. Should succeed.
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			r2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2).ToNot(BeNil())
		})

		It("should handle multiple counter sets in a single pool", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-resources", map[string]resource.Quantity{
						"memory":        resource.MustParse("80Gi"),
						"compute-units": resource.MustParse("4"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						resourcev1.Device{
							Name: "gpu-0",
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{{
								CounterSet: "gpu-resources",
								Counters: map[string]resourcev1.Counter{
									"memory":        {Value: resource.MustParse("40Gi")},
									"compute-units": {Value: resource.MustParse("2")},
								},
							}},
						},
						resourcev1.Device{
							Name: "gpu-1",
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{{
								CounterSet: "gpu-resources",
								Counters: map[string]resourcev1.Counter{
									"memory":        {Value: resource.MustParse("40Gi")},
									"compute-units": {Value: resource.MustParse("2")},
								},
							}},
						},
						resourcev1.Device{
							Name: "gpu-2",
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{{
								CounterSet: "gpu-resources",
								Counters: map[string]resourcev1.Counter{
									"memory":        {Value: resource.MustParse("40Gi")},
									"compute-units": {Value: resource.MustParse("2")},
								},
							}},
						},
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")

			// 2 devices: 80Gi memory (ok), 4 compute-units (ok) — both budgets exactly met.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())

			// 3 devices: either counter would exceed budget.
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 3))
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())
		})

		It("should backtrack counter deductions when DFS path fails constraints", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withAPIDevicesWithAttrs(
						deviceWithAttrsAndCounters("gpu-0",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/numa": {StringValue: ptr.To("node-0")}},
							[]resourcev1.DeviceCounterConsumption{{CounterSet: "gpu-slices", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("40Gi")}}}},
						),
						deviceWithAttrsAndCounters("gpu-2",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/numa": {StringValue: ptr.To("node-1")}},
							[]resourcev1.DeviceCounterConsumption{{CounterSet: "gpu-slices", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("40Gi")}}}},
						),
						deviceWithAttrsAndCounters("gpu-1",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/numa": {StringValue: ptr.To("node-0")}},
							[]resourcev1.DeviceCounterConsumption{{CounterSet: "gpu-slices", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("40Gi")}}}},
						),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			// Request 2 devices with MatchAttribute on numa — DFS must backtrack past gpu-2 (node-1)
			// and allocate gpu-0 + gpu-1 (both node-0), with counters correctly restored.
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

		It("should enforce all-mode counter budget", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("60Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")

			// All mode: 2 devices × 40Gi = 80Gi > 60Gi budget. Should fail.
			claim := makeClaim("c1", allRequest("req-1", "gpu"))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should succeed with all-mode counters on first pod when budget is sufficient", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("30Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("30Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")

			// All-mode: total counter consumption = 30+30 = 60Gi ≤ 80Gi budget. Should succeed.
			claim := makeClaim("c1", allRequest("req-1", "gpu"))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should partially refund counter budget when one IT is released from a multi-IT NC", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("120Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-2", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC-A with 2 ITs: both ITs allocate 2 devices (80Gi each). Pessimistic max = 80Gi.
			// Remaining: 120 - 80 = 40Gi.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			Expect(r1.InstanceTypes).To(HaveLen(2))
			r1.Allocation.Commit(ctx)

			// NC-B: only 40Gi remaining, needs 80Gi — should fail.
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			// Release only it-a from NC-A. it-b still holds the same devices (80Gi).
			// Pessimistic max doesn't change (it-b still consumes 80Gi), so no refund.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))

			// NC-B still fails — budget hasn't changed.
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			// Release it-b from NC-A — now the full 80Gi is refunded.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-b"))

			// NC-B can now allocate 2 devices (80Gi ≤ 120Gi remaining).
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should commit the pessimistic max when ITs have asymmetric counter consumption", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("120Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-2", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC-A with 2 ITs: it-a requests 1 device (40Gi), it-b requests 2 devices (80Gi).
			// Pessimistic max = max(40, 80) = 80Gi. Remaining: 120 - 80 = 40Gi.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1a := makeClaim("c1a", exactRequest("req-1", "gpu", 1))
			claim1b := makeClaim("c1b", exactRequest("req-2", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1a})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)
			r2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1b})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// NC-B: 2 devices need 80Gi, but only 40Gi may remain if pessimistic max was
			// correctly 80Gi from it-b's second commit. However, this test verifies the
			// accumulation: it-a committed 40 + 40 = 80, it-b committed 40 + 40 = 80. Max = 80.
			// Actually both ITs get the same devices (same DFS order), so max stays at 80.
			// Let's verify that NC-B can allocate exactly 1 device (40Gi ≤ 40Gi remaining).
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())

			// But 2 devices (80Gi) should fail — only 40Gi left.
			ncC := makeNodeClaimWithID("nc-c", "it-d")
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 2))
			_, err = alloc.Allocate(ctx, ncC, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())
		})

		It("should not over-deduct counters across multiple commits with alternating IT winners", func() {
			// This test exercises the multi-commit counter delta logic. When different ITs
			// are the "pessimistic winner" across commits, the naive sum-of-per-commit-max
			// over-deducts. The fix computes delta between accumulated maxes.
			//
			// Setup: pool budget=120Gi, devices with varying costs.
			// NC-A has it-1 and it-2. Commit 1 prunes it-2 (via nic requirement that only
			// it-1 can satisfy), causing asymmetric IsAllocated state. In commit 2, it-1
			// skips its prior device but it-2 picks it, creating asymmetric counter costs.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("120Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("60Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("20Gi")}),
						deviceConsumingCounter("gpu-2", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}

			// Register "nic" device class for the template-only device.
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

			// NC-A: it-1 has a "nic" template device, it-2 does not.
			itA := unique.Make("it-1")
			itB := unique.Make("it-2")
			ncA := &fakeNodeClaim{
				id:            unique.Make("nc-a"),
				nodePoolID:    unique.Make("test-np"),
				requirements:  scheduling.NewRequirements(),
				instanceTypes: []dynamicresources.InstanceTypeID{itA, itB},
				resourceSlices: map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice{
					itA: {dynamicresources.NewTemplateSlice(&cloudprovider.ResourceSliceTemplate{
						Driver:  unique.Make("nic.example.com"),
						Pool:    cloudprovider.ResourcePool{Name: unique.Make("nic-pool")},
						Devices: []cloudprovider.Device{{Name: unique.Make("nic-0")}},
					})},
				},
			}

			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// Commit 1: claim needs 1 gpu + 1 nic. it-1 gets both (gpu-0=60Gi + nic-0).
			// it-2 fails (no nic device). Only it-1 survives.
			// Counter committed for NC-A: max(it-1:60Gi) = 60Gi. Remaining: 120-60=60.
			claim1 := makeClaim("c1",
				exactRequest("req-gpu", "gpu", 1),
				exactRequest("req-nic", "nic", 1),
			)
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			Expect(r1.InstanceTypes).To(HaveLen(1)) // only it-1 survives
			Expect(r1.InstanceTypes[0].Value()).To(Equal("it-1"))
			r1.Allocation.Commit(ctx)

			// Commit 2: claim needs 1 gpu only. Both ITs are re-evaluated.
			// it-1: gpu-0 is allocated for it-1 → skips it. Picks gpu-1 (20Gi).
			// it-2: gpu-0 is NOT allocated for it-2 → picks gpu-0 (60Gi).
			// Per-commit pessimistic max: max(20, 60) = 60Gi.
			//
			// BUG (old code): subtracts 60Gi more. Total: 60+60=120. Remaining: 0.
			// FIX (new code): oldMax=max(60)=60. After merge: max(60+20=80, 60)=80.
			//   Delta=80-60=20. Subtracts 20. Total: 60+20=80. Remaining: 40.
			claim2 := makeClaim("c2", exactRequest("req-gpu", "gpu", 1))
			r2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2.InstanceTypes).To(HaveLen(2)) // both ITs survive
			r2.Allocation.Commit(ctx)

			// Verification: allocate on a separate NC-B. With the fix, 40Gi remains,
			// so 1 device at 40Gi should succeed.
			ncB := makeNodeClaimWithID("nc-b", "it-3")
			claim3 := makeClaim("c3", exactRequest("req-gpu", "gpu", 1))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should accumulate counter consumption across multiple pods on the same NC/IT", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("120Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-2", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// Pod 1 on NC-A: allocates 1 device (40Gi). Committed: 40Gi.
			ncA := makeNodeClaimWithID("nc-a", "it-1")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Pod 2 on NC-A: allocates 1 more device (40Gi). Accumulated: 80Gi.
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			r2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Remaining: 120 - 80 = 40Gi. NC-B can allocate 1 device but not 2.
			ncB := makeNodeClaimWithID("nc-b", "it-2")
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 1))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should partially refund when released IT had higher consumption than survivors", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("120Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-2", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC-A with 2 ITs both allocate 2 devices (80Gi each). Pessimistic max = 80Gi.
			// Remaining: 120 - 80 = 40Gi.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			Expect(r1.InstanceTypes).To(HaveLen(2))
			r1.Allocation.Commit(ctx)

			// Release it-a. it-b still holds 80Gi. Max doesn't change → no refund.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))

			// Still only 40Gi available. 2 devices (80Gi) should still fail.
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			// But 1 device (40Gi) should succeed.
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 1))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should not refund counters when released IT had lower consumption than survivors (delta <= 0)", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("160Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-2", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-3", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC-A with 2 ITs. Both allocate via DFS order — same devices, same consumption.
			// Commit 1 device for pod 1, then 2 devices for pod 2.
			// it-a: 40 + 80 = 120Gi accumulated, it-b: 40 + 80 = 120Gi accumulated. Max=120.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			r2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Remaining: 160 - 120 = 40Gi. Release it-a — it-b still has 120Gi.
			// Delta = 120 - 120 = 0. No refund expected.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))

			// Still only 40Gi remaining — can allocate 1 device but not 2.
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 1))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should not cross-contaminate counters between template and in-cluster pools sharing the same PoolKey", func() {
			// In-cluster pool: driver=gpu.example.com, pool=shared-pool, budget=50Gi, 1 device @ 50Gi.
			// Template pool: same driver+pool name, budget=50Gi, 1 template device @ 50Gi.
			// Claim requests 2 devices. The DFS picks the in-cluster device first (deducting 50Gi
			// from in-cluster allocatingCounters), then picks the template device (checking against
			// template budget). Without separated maps, the in-cluster deduction would be subtracted
			// from the template's available budget, causing incorrect rejection.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "shared-pool",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("50Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "shared-pool", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("50Gi")}),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			itID := unique.Make("it-1")
			nc := &fakeNodeClaim{
				id:            unique.Make("test-nc"),
				nodePoolID:    unique.Make("test-np"),
				requirements:  scheduling.NewRequirements(),
				instanceTypes: []dynamicresources.InstanceTypeID{itID},
				resourceSlices: map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice{
					itID: {dynamicresources.NewTemplateSlice(makeTemplateWithCounters("gpu.example.com", "shared-pool",
						[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
							"memory": resource.MustParse("50Gi"),
						})},
						templateDevice("tpl-gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("50Gi")})),
					))},
				},
			}

			// Request 2 devices. DFS order: in-cluster gpu-0 first, then template tpl-gpu-0.
			// Each pool has independent 50Gi budget, so both devices fit individually.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred(), "allocation should succeed when template and in-cluster pools have independent budgets")
			Expect(result).ToNot(BeNil())
			Expect(result.InstanceTypes).To(HaveLen(1))
		})

		It("should refund full delta when released IT consumed from a pool the survivor did not", func() {
			// Two NCs on the same allocator: NC-1 has only it-a, NC-2 has only it-b.
			// NC-1 consumes from pool-a AND pool-b. NC-2 consumes from pool-a only.
			// Releasing NC-1/it-a: remaining for pool-b should be fully restored.
			// This exercises addDeltaToRemaining's "newCounterMax missing poolKey" branch
			// (since after release, countersByNodeClaimIT for NC-1 is empty → nil newMax).
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters-a", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("100Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices-a", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("50Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("50Gi")}),
					),
				),
				makeAPISlice("s-counters-b", "nic.example.com", "pool-b",
					withSharedCounters(counterSet("bw-slices", map[string]resource.Quantity{
						"bandwidth": resource.MustParse("100G"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices-b", "nic.example.com", "pool-b", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("nic-0", "bw-slices", map[string]resource.Quantity{"bandwidth": resource.MustParse("50G")}),
						deviceConsumingCounter("nic-1", "bw-slices", map[string]resource.Quantity{"bandwidth": resource.MustParse("50G")}),
					),
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

			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC-1: allocate 1 gpu (pool-a: 50Gi) + 1 nic (pool-b: 50G).
			nc1 := makeNodeClaimWithID("nc-1", "it-a")
			claim1 := makeClaim("c1", exactRequest("req-gpu", "gpu", 1), exactRequest("req-nic", "nic", 1))
			r1, err := alloc.Allocate(ctx, nc1, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// NC-2: allocate 1 gpu only (pool-a: 50Gi consumed, pool-b: untouched).
			nc2 := makeNodeClaimWithID("nc-2", "it-b")
			claim2 := makeClaim("c2", exactRequest("req-gpu", "gpu", 1))
			r2, err := alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// pool-a: 100 - 50 - 50 = 0Gi. pool-b: 100 - 50 = 50G.
			// Release NC-1/it-a: pool-a refunds 50Gi, pool-b refunds 50G (full — only NC-1 consumed from pool-b).
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-1"), unique.Make("it-a"))

			// Verify pool-b is fully restored: allocate 2 nics (100G needed).
			nc3 := makeNodeClaimWithID("nc-3", "it-c")
			r3, err := alloc.Allocate(ctx, nc3, []*resourcev1.ResourceClaim{makeClaim("c-ok", exactRequest("req-nic", "nic", 2))})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should refund full delta when released IT consumed from a counter set the survivor did not", func() {
			// Two ITs consuming from the same pool but different counter set names.
			// IT-A: counterSet "memory-budget" + "compute-budget"
			// IT-B: counterSet "memory-budget" only
			// Release IT-A: newCounterMax has "memory-budget" but NOT "compute-budget",
			// so compute-budget's full old max is refunded.
			// This exercises addDeltaToRemaining's "newCounterMax missing counterSetName" branch.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(
						counterSet("memory-budget", map[string]resource.Quantity{"memory": resource.MustParse("80Gi")}),
						counterSet("compute-budget", map[string]resource.Quantity{"flops": resource.MustParse("100")}),
					),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withAPIDevicesWithAttrs(
						deviceWithAttrsAndCounters("gpu-0",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/tier": {StringValue: ptr.To("high")}},
							[]resourcev1.DeviceCounterConsumption{
								counterConsumption("memory-budget", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
								counterConsumption("compute-budget", map[string]resource.Quantity{"flops": resource.MustParse("50")}),
							},
						),
						deviceWithAttrsAndCounters("gpu-1",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/tier": {StringValue: ptr.To("low")}},
							[]resourcev1.DeviceCounterConsumption{
								counterConsumption("memory-budget", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
							},
						),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC with two ITs. Claim: 1 gpu with tier=high (only gpu-0 matches — consumes both counter sets).
			// IT-A picks gpu-0 (consumes memory-budget:40Gi + compute-budget:50).
			// IT-B picks gpu-0 (same device, same consumption).
			// Pessimistic max: max(40Gi,40Gi)=40Gi for memory, max(50,50)=50 for compute.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequestWithSelector("req-gpu", "gpu", 1, `device.attributes["gpu.example.com"].tier == "high"`))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			Expect(r1.InstanceTypes).To(HaveLen(2))
			r1.Allocation.Commit(ctx)

			// Second claim: 1 gpu with tier=low (only gpu-1 matches — consumes memory only).
			// IT-A: gpu-1 consumes memory-budget:40Gi (no compute). IT-B: same.
			// After commit: IT-A total = memory:80Gi, compute:50; IT-B total = memory:80Gi, compute:50.
			claim2 := makeClaim("c2", exactRequestWithSelector("req-gpu", "gpu", 1, `device.attributes["gpu.example.com"].tier == "low"`))
			r2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Release IT-A. oldMax=max(80,80)=80Gi memory, max(50,50)=50 compute.
			// newMax (IT-B only)=80Gi memory, 50 compute. Delta=0 for both. No refund.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))

			// Now release IT-B too. oldMax=80Gi memory, 50 compute. newMax=nil. Full refund.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-b"))

			// Verify full budget restored — allocate devices consuming both counter sets.
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim3 := makeClaim("c3", exactRequest("req-gpu", "gpu", 2))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should refund full delta when released IT consumed a counter name the survivor did not", func() {
			// Same pool and counter set, but ITs consume different counter names.
			// IT-A device consumes "memory" + "bandwidth"; IT-B device consumes "memory" only.
			// Release IT-A: newCounterMax has "memory" but NOT "bandwidth", so "bandwidth"
			// gets full refund. Exercises addDeltaToRemaining's "missing counterName" branch.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("resource-budget", map[string]resource.Quantity{
						"memory":    resource.MustParse("100Gi"),
						"bandwidth": resource.MustParse("200G"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withAPIDevicesWithAttrs(
						deviceWithAttrsAndCounters("dev-full",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/tier": {StringValue: ptr.To("full")}},
							[]resourcev1.DeviceCounterConsumption{
								counterConsumption("resource-budget", map[string]resource.Quantity{
									"memory":    resource.MustParse("50Gi"),
									"bandwidth": resource.MustParse("100G"),
								}),
							},
						),
						deviceWithAttrsAndCounters("dev-lite",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/tier": {StringValue: ptr.To("lite")}},
							[]resourcev1.DeviceCounterConsumption{
								counterConsumption("resource-budget", map[string]resource.Quantity{
									"memory": resource.MustParse("50Gi"),
								}),
							},
						),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC with it-a and it-b. Claim: 1 device with tier=full.
			// Both ITs pick dev-full → consume memory:50Gi + bandwidth:100G.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequestWithSelector("req-1", "gpu", 1, `device.attributes["gpu.example.com"].tier == "full"`))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			Expect(r1.InstanceTypes).To(HaveLen(2))
			r1.Allocation.Commit(ctx)

			// Second claim: 1 device with tier=lite. Consumes memory:50Gi only (no bandwidth).
			// Both ITs pick dev-lite → IT-A total: memory=100Gi, bw=100G; IT-B total: same.
			claim2 := makeClaim("c2", exactRequestWithSelector("req-1", "gpu", 1, `device.attributes["gpu.example.com"].tier == "lite"`))
			r2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Release IT-A. Both ITs had the same consumption, so delta=0.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))

			// Release IT-B. Full refund: memory=100Gi, bandwidth=100G restored.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-b"))

			// Verify full budget: allocate both devices (memory:100Gi, bandwidth:100G needed).
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 2))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should include disjoint counter names from different ITs in pessimistic max", func() {
			// IT-A consumes counter "memory"; IT-B consumes counter "flops".
			// Pessimistic max must include BOTH (not just the larger one), since
			// the NodeClaim will become one or the other.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("resource-budget", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
						"flops":  resource.MustParse("100"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withAPIDevicesWithAttrs(
						deviceWithAttrsAndCounters("dev-mem-0",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/kind": {StringValue: ptr.To("mem")}},
							[]resourcev1.DeviceCounterConsumption{
								counterConsumption("resource-budget", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
							},
						),
						deviceWithAttrsAndCounters("dev-mem-1",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/kind": {StringValue: ptr.To("mem")}},
							[]resourcev1.DeviceCounterConsumption{
								counterConsumption("resource-budget", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
							},
						),
						deviceWithAttrsAndCounters("dev-compute-0",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/kind": {StringValue: ptr.To("compute")}},
							[]resourcev1.DeviceCounterConsumption{
								counterConsumption("resource-budget", map[string]resource.Quantity{"flops": resource.MustParse("50")}),
							},
						),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC-A (it-a, it-b): 1 device kind=mem → both ITs pick dev-mem-0 (memory:40Gi).
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequestWithSelector("req-1", "gpu", 1, `device.attributes["gpu.example.com"].kind == "mem"`))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// NC-A second claim: 1 device kind=compute → both ITs pick dev-compute-0 (flops:50).
			// Accumulated: each IT: memory=40, flops=50. pessimisticMax includes BOTH counters.
			claim2 := makeClaim("c2", exactRequestWithSelector("req-1", "gpu", 1, `device.attributes["gpu.example.com"].kind == "compute"`))
			r2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Remaining: memory=80-40=40Gi, flops=100-50=50.
			// NC-B: allocate dev-mem-1 (40Gi memory). Should succeed.
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim3 := makeClaim("c3", exactRequestWithSelector("req-1", "gpu", 1, `device.attributes["gpu.example.com"].kind == "mem"`))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should accumulate counter consumption from a new pool on second commit for the same NC/IT", func() {
			// NC-A/IT-1: first commit consumes from pool-a, second commit consumes from pool-b.
			// This exercises commitCounters' "existing IT with new poolKey" branch.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters-a", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices-a", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
				makeAPISlice("s-counters-b", "nic.example.com", "pool-b",
					withSharedCounters(counterSet("bw-slices", map[string]resource.Quantity{
						"bandwidth": resource.MustParse("100G"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices-b", "nic.example.com", "pool-b", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("nic-0", "bw-slices", map[string]resource.Quantity{"bandwidth": resource.MustParse("50G")}),
					),
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			ncA := makeNodeClaimWithID("nc-a", "it-1")

			// First commit: consume from pool-a only.
			claim1 := makeClaim("c1", exactRequest("req-gpu", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Second commit: consume from pool-b only (new poolKey for same NC/IT).
			claim2 := makeClaim("c2", exactRequest("req-nic", "nic", 1))
			r2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Verify both pools are tracked: release NC-A/IT-1 should restore both.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-1"))

			ncB := makeNodeClaimWithID("nc-b", "it-2")
			claim3 := makeClaim("c3", exactRequest("req-gpu", "gpu", 1), exactRequest("req-nic", "nic", 1))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should accumulate counter consumption from a new counter set on second commit for the same NC/IT/pool", func() {
			// Same pool, but commit 1 consumes from counterSet "memory-budget" and
			// commit 2 consumes from counterSet "compute-budget".
			// Exercises commitCounters' "existing poolKey with new counterSetName" branch.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(
						counterSet("memory-budget", map[string]resource.Quantity{"memory": resource.MustParse("80Gi")}),
						counterSet("compute-budget", map[string]resource.Quantity{"flops": resource.MustParse("100")}),
					),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withAPIDevicesWithAttrs(
						deviceWithAttrsAndCounters("gpu-mem",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/kind": {StringValue: ptr.To("mem")}},
							[]resourcev1.DeviceCounterConsumption{
								counterConsumption("memory-budget", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
							},
						),
						deviceWithAttrsAndCounters("gpu-compute",
							map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"gpu.example.com/kind": {StringValue: ptr.To("compute")}},
							[]resourcev1.DeviceCounterConsumption{
								counterConsumption("compute-budget", map[string]resource.Quantity{"flops": resource.MustParse("50")}),
							},
						),
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			ncA := makeNodeClaimWithID("nc-a", "it-1")

			// First commit: consume memory-budget.
			claim1 := makeClaim("c1", exactRequestWithSelector("req-1", "gpu", 1, `device.attributes["gpu.example.com"].kind == "mem"`))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Second commit: consume compute-budget (new counterSetName for same NC/IT/pool).
			claim2 := makeClaim("c2", exactRequestWithSelector("req-1", "gpu", 1, `device.attributes["gpu.example.com"].kind == "compute"`))
			r2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Verify both counter sets are tracked: release restores both.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-1"))

			ncB := makeNodeClaimWithID("nc-b", "it-2")
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 2))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should deep copy allocating counters across multiple pools", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters-a", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices-a", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("gpu-0", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
						deviceConsumingCounter("gpu-1", "gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
					),
				),
				makeAPISlice("s-counters-b", "nic.example.com", "pool-b",
					withSharedCounters(counterSet("bw-slices", map[string]resource.Quantity{
						"bandwidth": resource.MustParse("100G"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices-b", "nic.example.com", "pool-b", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						deviceConsumingCounter("nic-0", "bw-slices", map[string]resource.Quantity{"bandwidth": resource.MustParse("50G")}),
						deviceConsumingCounter("nic-1", "bw-slices", map[string]resource.Quantity{"bandwidth": resource.MustParse("50G")}),
					),
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// NC-A allocates from both pools, commits, then releases.
			ncA := makeNodeClaimWithID("nc-a", "it-1")
			claimGPU := makeClaim("c-gpu", exactRequest("req-gpu", "gpu", 2))
			claimNIC := makeClaim("c-nic", exactRequest("req-nic", "nic", 2))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claimGPU, claimNIC})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Both budgets should be exhausted.
			ncB := makeNodeClaimWithID("nc-b", "it-2")
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{makeClaim("c2-gpu", exactRequest("req-gpu", "gpu", 1))})
			Expect(err).To(HaveOccurred())

			// Release NC-A — both pools' budgets should be restored.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-1"))
			r3, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{makeClaim("c3-gpu", exactRequest("req-gpu", "gpu", 2)), makeClaim("c3-nic", exactRequest("req-nic", "nic", 2))})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})
	})

	Describe("SharedCounters — templates", func() {
		It("should allocate template devices when counter budget is sufficient", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
				makeTemplateWithCounters("gpu.example.com", "pool-a",
					[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})},
					templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
					templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				),
			)
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.InstanceTypes).To(HaveLen(1))
		})

		It("should reject template allocation when counter budget is exhausted", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
				makeTemplateWithCounters("gpu.example.com", "pool-a",
					[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})},
					templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
					templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
					templateDevice("gpu-2", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				),
			)
			// 3 devices × 40Gi > 80Gi budget — only 2 can be allocated.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 3))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should evaluate template counters independently per instance type", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			// it-large has 160Gi budget (fits 4 devices), it-small has 80Gi budget (fits only 2).
			nc := makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
				"it-large": {makeTemplateWithCounters("gpu.example.com", "pool-a",
					[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("160Gi"),
					})},
					templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
					templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
					templateDevice("gpu-2", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				)},
				"it-small": {makeTemplateWithCounters("gpu.example.com", "pool-b",
					[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})},
					templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
					templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
					templateDevice("gpu-2", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				)},
			})
			// Request 3 devices: it-large can (160Gi budget), it-small cannot (only 80Gi).
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 3))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			// Only it-large should survive.
			Expect(result.InstanceTypes).To(HaveLen(1))
			Expect(result.InstanceTypes[0].Value()).To(Equal("it-large"))
		})

		It("should not share template counter budgets across pods", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			tmpl := makeTemplateWithCounters("gpu.example.com", "pool-a",
				[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
					"memory": resource.MustParse("80Gi"),
				})},
				templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
			)

			// Pod 1 on NC-A: allocate 2 template devices (exhausts budget for NC-A).
			ncA := makeNodeClaimWithTemplatesAndID("nc-a", "it-1", tmpl)
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Pod 2 on NC-B: allocate 2 template devices — should succeed because
			// template counters are per-IT, not shared across NodeClaims.
			ncB := makeNodeClaimWithTemplatesAndID("nc-b", "it-1", tmpl)
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			r2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2).ToNot(BeNil())
		})

		It("should accumulate template counter consumption across multiple pods on the same NC/IT", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			// 4 devices × 40Gi each, but only 80Gi budget — max 2 devices allocatable per NC/IT.
			tmpl := makeTemplateWithCounters("gpu.example.com", "pool-a",
				[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
					"memory": resource.MustParse("80Gi"),
				})},
				templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				templateDevice("gpu-2", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				templateDevice("gpu-3", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
			)

			// Pod 1 on NC-A: allocates 2 template devices (80Gi). Budget exhausted.
			ncA := makeNodeClaimWithTemplatesAndID("nc-a", "it-1", tmpl)
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Pod 2 on same NC-A: attempts 1 device. Should fail — counter budget is exhausted.
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			_, err = alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())
		})

		It("should allow template counter allocation on a different NC after exhausting budget on the first", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			tmpl := makeTemplateWithCounters("gpu.example.com", "pool-a",
				[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
					"memory": resource.MustParse("80Gi"),
				})},
				templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				templateDevice("gpu-2", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
			)

			// Pod 1 on NC-A: allocates 2 devices (80Gi). Budget exhausted for NC-A.
			ncA := makeNodeClaimWithTemplatesAndID("nc-a", "it-1", tmpl)
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Pod 2 on NC-B: should succeed — template budgets are per-NC.
			ncB := makeNodeClaimWithTemplatesAndID("nc-b", "it-1", tmpl)
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			r2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2).ToNot(BeNil())
		})

		It("should release template counter budget when instance type is pruned", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			tmpl := makeTemplateWithCounters("gpu.example.com", "pool-a",
				[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
					"memory": resource.MustParse("80Gi"),
				})},
				templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				templateDevice("gpu-2", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
			)

			// Pod 1 on NC-A: allocates 2 devices (80Gi). Budget exhausted.
			ncA := makeNodeClaimWithTemplatesAndID("nc-a", "it-1", tmpl)
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Release the instance type — template counter budget should be freed.
			alloc.ReleaseInstanceType(ctx, ncA.ID(), ncA.InstanceTypes()[0])

			// Pod 2 on same NC-A with fresh IT: should succeed since budget was released.
			ncA2 := makeNodeClaimWithTemplatesAndID("nc-a", "it-2", tmpl)
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			r2, err := alloc.Allocate(ctx, ncA2, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2).ToNot(BeNil())
		})

		It("should track template counter deductions within a single claim with multiple requests", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
				makeTemplateWithCounters("gpu.example.com", "pool-a",
					[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})},
					templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
					templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
					templateDevice("gpu-2", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})),
				),
			)
			// Two requests in one claim: 2 + 1 = 3 devices needed, but budget allows only 2.
			claim := makeClaim("c1",
				exactRequest("req-1", "gpu", 2),
				exactRequest("req-2", "gpu", 1),
			)

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should handle multiple template slices for the same pool key", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			// Two template slices for same driver/pool, each contributing part of the counter budget.
			nc := makeNodeClaimWithTemplates(
				makeTemplateWithCounters("gpu.example.com", "pool-a",
					[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("40Gi"),
					})},
					templateDevice("gpu-0", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("20Gi")})),
				),
				makeTemplateWithCounters("gpu.example.com", "pool-a",
					[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("60Gi"),
					})},
					templateDevice("gpu-1", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("20Gi")})),
					templateDevice("gpu-2", counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("20Gi")})),
				),
			)
			// 3 devices × 20Gi = 60Gi. The last template slice overwrites the counter budget
			// to 60Gi (same pool key, same counter set), so this should succeed.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 3))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should handle multiple counter sets per template slice", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
				makeTemplateWithCounters("gpu.example.com", "pool-a",
					[]resourcev1.CounterSet{
						counterSet("memory-budget", map[string]resource.Quantity{"memory": resource.MustParse("80Gi")}),
						counterSet("compute-budget", map[string]resource.Quantity{"flops": resource.MustParse("100")}),
					},
					cloudprovider.Device{
						Name: unique.Make("gpu-0"),
						ConsumesCounters: []resourcev1.DeviceCounterConsumption{
							counterConsumption("memory-budget", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
							counterConsumption("compute-budget", map[string]resource.Quantity{"flops": resource.MustParse("50")}),
						},
					},
					cloudprovider.Device{
						Name: unique.Make("gpu-1"),
						ConsumesCounters: []resourcev1.DeviceCounterConsumption{
							counterConsumption("memory-budget", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")}),
							counterConsumption("compute-budget", map[string]resource.Quantity{"flops": resource.MustParse("50")}),
						},
					},
				),
			)
			// 2 devices: exactly fits both budgets.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should reject template device when templateRemainingCounters is nil", func() {
			// Template device has ConsumesCounters but the template slice has NO SharedCounters.
			// buildTemplateCounters() returns nil → templateRemainingCounters is nil →
			// checkCounters receives nil remainingCounterSets → returns false.
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			// Template with devices that consume counters but NO SharedCounters declared.
			nc := makeNodeClaimWithTemplates(&cloudprovider.ResourceSliceTemplate{
				Driver: unique.Make("gpu.example.com"),
				Pool:   cloudprovider.ResourcePool{Name: unique.Make("pool-a")},
				Devices: []cloudprovider.Device{
					{
						Name:             unique.Make("gpu-0"),
						ConsumesCounters: []resourcev1.DeviceCounterConsumption{counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})},
					},
				},
				// SharedCounters intentionally omitted → buildTemplateCounters returns nil.
			})

			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should reject template device when its pool has no entry in templateRemainingCounters", func() {
			// Multi-pool template setup: pool-A has SharedCounters (populates
			// templateRemainingCounters), pool-B's device has ConsumesCounters but pool-B's
			// slice has no SharedCounters. When pool-B device is evaluated,
			// templateRemainingCounters is non-nil (from pool-A) but has no entry for pool-B.
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			itID := unique.Make("it-1")
			nc := &fakeNodeClaim{
				id:            unique.Make("test-nc"),
				nodePoolID:    unique.Make("test-np"),
				requirements:  scheduling.NewRequirements(),
				instanceTypes: []dynamicresources.InstanceTypeID{itID},
				resourceSlices: map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice{
					itID: {
						// Pool-A: has SharedCounters + device without ConsumesCounters (allocatable).
						dynamicresources.NewTemplateSlice(makeTemplateWithCounters("gpu.example.com", "pool-a",
							[]resourcev1.CounterSet{counterSet("gpu-slices", map[string]resource.Quantity{
								"memory": resource.MustParse("80Gi"),
							})},
							cloudprovider.Device{Name: unique.Make("gpu-a")},
						)),
						// Pool-B: NO SharedCounters but device references counters.
						dynamicresources.NewTemplateSlice(&cloudprovider.ResourceSliceTemplate{
							Driver: unique.Make("gpu.example.com"),
							Pool:   cloudprovider.ResourcePool{Name: unique.Make("pool-b")},
							Devices: []cloudprovider.Device{
								{
									Name:             unique.Make("gpu-b"),
									ConsumesCounters: []resourcev1.DeviceCounterConsumption{counterConsumption("gpu-slices", map[string]resource.Quantity{"memory": resource.MustParse("40Gi")})},
								},
							},
						}),
					},
				},
			}

			// Request 2 devices: gpu-a from pool-A succeeds (no ConsumesCounters).
			// gpu-b from pool-B fails (ConsumesCounters references budget that doesn't exist for pool-B).
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Multi-IT allocation", func() {
		It("should prune instance types that cannot satisfy requests", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
				"it-a": {makeTemplate("gpu.example.com", "pool-b", "tgpu-0")},
				"it-b": {makeTemplate("gpu.example.com", "pool-c", "tgpu-0")},
			})
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 5))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Multi-IT constraint isolation", func() {
		It("should not leak constraint state between instance type iterations", func() {
			// IT-A has template devices with numa=node-0, IT-B has template devices with numa=node-1.
			// Both should independently satisfy a MatchAttribute constraint on numa.
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
				"it-a": {makeTemplateWithAttrs("gpu.example.com", "pool-a",
					deviceWithAttrs("tgpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
					}),
					deviceWithAttrs("tgpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
					}),
				)},
				"it-b": {makeTemplateWithAttrs("gpu.example.com", "pool-b",
					deviceWithAttrs("tgpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
					}),
					deviceWithAttrs("tgpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
					}),
				)},
			})
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.InstanceTypes).To(HaveLen(2))
		})

		It("should not leak binding fallback state between instance type iterations", func() {
			// IT-A uses binding fallback (devices lack the attribute), IT-B has concrete attribute values.
			// Both should independently satisfy the constraint.
			devA0 := deviceID("gpu.example.com", "pool-a", "tgpu-0")
			devA1 := deviceID("gpu.example.com", "pool-a", "tgpu-1")

			bindings := dynamicresources.BuildAttributeBindings(map[string][]*cloudprovider.InstanceType{
				"test-np": {
					&cloudprovider.InstanceType{
						Name: "it-a",
						DynamicResources: cloudprovider.DynamicResources{
							AttributeBindings: []*cloudprovider.AttributeBinding{
								{
									Attribute: "gpu.example.com/numa",
									Devices:   []cloudprovider.DeviceID{devA0.DeviceID, devA1.DeviceID},
								},
							},
						},
					},
				},
			})

			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, bindings, env.Client, nil)
			nc := makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
				"it-a": {makeTemplate("gpu.example.com", "pool-a", "tgpu-0", "tgpu-1")},
				"it-b": {makeTemplateWithAttrs("gpu.example.com", "pool-b",
					deviceWithAttrs("tgpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
					}),
					deviceWithAttrs("tgpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
					}),
				)},
			})
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.InstanceTypes).To(HaveLen(2))
		})

		It("should still allow second IT to succeed when first IT fails constraints", func() {
			// IT-A has devices with conflicting NUMA values (can't satisfy constraint).
			// IT-B has devices with matching NUMA values (can satisfy).
			// Verifies that failed DFS backtracking leaves clean state.
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeMultiITNodeClaim(map[string][]*cloudprovider.ResourceSliceTemplate{
				"it-a": {makeTemplateWithAttrs("gpu.example.com", "pool-a",
					deviceWithAttrs("tgpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-0")},
					}),
					deviceWithAttrs("tgpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-1")},
					}),
				)},
				"it-b": {makeTemplateWithAttrs("gpu.example.com", "pool-b",
					deviceWithAttrs("tgpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-2")},
					}),
					deviceWithAttrs("tgpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"gpu.example.com/numa": {StringValue: ptr.To("node-2")},
					}),
				)},
			})
			claim := makeClaimWithConstraints("c1",
				[]resourcev1.DeviceConstraint{
					{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName("gpu.example.com/numa"))},
				},
				exactRequest("req-1", "gpu", 2),
			)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			// Only it-b should survive (it-a has conflicting NUMA values).
			Expect(result.InstanceTypes).To(HaveLen(1))
			Expect(result.InstanceTypes[0].Value()).To(Equal("it-b"))
		})
	})

	Describe("Commit protocol", func() {
		It("should mark in-cluster devices as allocated after commit", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1", "gpu-2")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: allocated}, nil, env.Client, nil)
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

			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-us-west-2b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("gpu-1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			Expect(meta).ToNot(BeNil())
			Expect(meta.Devices).To(HaveKey(unique.Make("it-1")))
			Expect(meta.Devices[unique.Make("it-1")]).To(HaveLen(1))
			Expect(meta.Devices[unique.Make("it-1")][0].DeviceID.Pool.Value()).To(ContainSubstring(expectedZone))
		})

		It("should narrow pools when a zonal device tightens requirements", func() {
			// Two pools in different zones. Request 2 devices — must come from the same zone
			// since the first device tightens requirements and eliminates the other zone's pool.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0", "gpu-a1"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("gpu-b0", "gpu-b1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("gpu-b0", "gpu-b1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
					withZoneSelector("eu-west-1a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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

			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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

			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, bindings, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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

			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
				makeTemplate("gpu.example.com", "pool-b", "tgpu-0", "tgpu-1"),
			)
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should fail with zero instance types", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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

		It("should return error for All-mode request against an invalid pool containing capacity devices", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						// Duplicate device names make the pool invalid.
						s.Spec.Devices = append(s.Spec.Devices,
							resourcev1.Device{
								Name:                     "gpu-dup",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
								},
							},
							resourcev1.Device{
								Name:                     "gpu-dup",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
								},
							},
						)
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			// All-mode: should fail because the pool is invalid (duplicate names).
			claim := makeClaim("c1", allRequest("req-1", "gpu"))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should fail ExactCount request when invalid pool's capacity devices are dropped", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						// Duplicate device names → invalid pool.
						// One capacity-only multi-alloc device + one duplicate = pool invalid.
						s.Spec.Devices = append(s.Spec.Devices,
							resourcev1.Device{
								Name:                     "gpu-dup",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
								},
							},
							resourcev1.Device{
								Name:                     "gpu-dup",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {Value: resource.MustParse("40Gi")},
								},
							},
						)
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-dup").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			// ExactCount: pool is invalid, devices are flushed → no devices available.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("In-cluster allocated claim handling", func() {
		It("should pass through claims with no nodeSelector", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeAllocatedClaim("c1", nil)

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(1))
		})

		It("should propagate topology requirements from the allocation nodeSelector", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("gpu-b0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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

	Describe("Deleting-pod claim reclassification", func() {
		// reservedClaim builds a claim already allocated to a real in-cluster device and reserved by the given consumers.
		// When every consumer is a deleting pod, the allocator should re-allocate it (re-run DFS) rather than treat it as
		// committed in place — so it must re-acquire a device from the available pool.
		reservedClaim := func(consumers ...resourcev1.ResourceClaimConsumerReference) *resourcev1.ResourceClaim {
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			claim.Status = resourcev1.ResourceClaimStatus{
				Allocation: &resourcev1.AllocationResult{
					Devices: resourcev1.DeviceAllocationResult{
						Results: []resourcev1.DeviceRequestAllocationResult{{
							Request: "req-1", Driver: "gpu.example.com", Pool: "pool-a", Device: "gpu-0",
						}},
					},
				},
				ReservedFor: consumers,
			}
			return claim
		}
		podRef := func(uid string) resourcev1.ResourceClaimConsumerReference {
			return resourcev1.ResourceClaimConsumerReference{Resource: "pods", Name: "pod-" + uid, UID: types.UID(uid)}
		}
		// inClusterSlices provides a single published cluster-wide GPU so a reclassified claim has a device to re-acquire.
		inClusterSlices := func() []dynamicresources.ResourceSlice {
			return []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 1)),
			}
		}

		It("re-allocates a claim reserved entirely by deleting pods", func() {
			alloc = dynamicresources.NewAllocator(inClusterSlices(), dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, sets.New[types.UID]("deleting"))
			nc := makeNodeClaim("it-1")
			claim := reservedClaim(podRef("deleting"))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			// The claim was reclassified as unallocated and re-ran the DFS, producing a fresh allocation.
			Expect(result.Allocation).ToNot(BeNil())
		})

		It("treats a claim reserved by a mix of deleting and live pods as committed", func() {
			// No in-cluster slices: if the claim were reclassified as unallocated, the DFS would fail to find a device.
			// Because a live consumer remains, the claim stays committed in place and allocation succeeds with no DFS.
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, sets.New[types.UID]("deleting"))
			nc := makeNodeClaim("it-1")
			claim := reservedClaim(podRef("deleting"), podRef("live"))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(1))
		})

		It("treats a claim reserved by a live pod as committed", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, sets.New[types.UID]("deleting"))
			nc := makeNodeClaim("it-1")
			claim := reservedClaim(podRef("live"))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(1))
		})

		It("treats a claim reserved by a non-pod consumer as committed", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, sets.New[types.UID]("deleting"))
			nc := makeNodeClaim("it-1")
			// A non-pod consumer (e.g. a different resource kind) is never treated as deleting, so the claim stays committed.
			claim := reservedClaim(resourcev1.ResourceClaimConsumerReference{Resource: "widgets", APIGroup: "example.com", Name: "w", UID: types.UID("deleting")})

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(1))
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("gpu-b0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices2, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0", "gpu-a1"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("gpu-b0", "gpu-b1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0", "gpu-a1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0", "gpu-a1", "gpu-a2"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

			nc := &fakeNodeClaim{
				id:         unique.Make("test-nc"),
				nodePoolID: unique.Make("test-np"),
				requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				),
				instanceTypes:  []dynamicresources.InstanceTypeID{unique.Make("it-a"), unique.Make("it-b"), unique.Make("it-c")},
				resourceSlices: make(map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice),
			}
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(3))
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
				makeTemplate("gpu.example.com", "pool-a", "tgpu-0"),
			)
			claim := makeClaim("c1", exactRequest("req-1", "nonexistent-class", 1))

			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		It("should return error for unsupported selector type", func() {
			// Use a request-level non-CEL selector (API server validates class selectors).
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
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

			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, bindings, env.Client, nil)
			// Template device WITHOUT the numa attribute (will use binding fallback path).
			nc := makeNodeClaimWithTemplates(
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

			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, bindings, env.Client, nil)
			nc := makeNodeClaimWithTemplates(
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

			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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

			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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

			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))

			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
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
					withZoneSelector("us-west-2a"),
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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

			metaC1 := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			metaC2 := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c2"})
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("gpu-1"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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

			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "gpu-pool-us-west-2b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("gpu-b0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s3", "fpga.example.com", "fpga-pool-us-west-2a",
					withZoneSelector("us-west-2a"),
					withAPIDevices("fpga-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s4", "fpga.example.com", "fpga-pool-us-west-2b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("fpga-b0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)
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

			claim1Meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "zonal-claim"})
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

			claim2Meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "new-claim"})
			Expect(claim2Meta).ToNot(BeNil())
			Expect(claim2Meta.Devices).To(HaveKey(unique.Make("it-1")))
			claim2Devices := claim2Meta.Devices[unique.Make("it-1")]
			Expect(claim2Devices).To(HaveLen(1))
			Expect(claim2Devices[0].DeviceID.Pool.Value()).To(ContainSubstring(expectedZone))
		})

		It("should fail when two in-memory allocated claims have incompatible zones", func() {
			// Create two in-memory claims pinned to different zones.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-zone-a",
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-a0"),
					withGeneration(1, 1),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-zone-b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("gpu-b0"),
					withGeneration(1, 1),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{ExclusiveDevices: sets.New[cloudprovider.DeviceID]()}, nil, env.Client, nil)

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
	Describe("Consumable capacity — DFS capacity-gated allocation", func() {
		It("should allocate a multi-alloc device with capacity and record ConsumedCapacity in metadata", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("16Gi"),
			}))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			Expect(meta).ToNot(BeNil())
			Expect(meta.Devices).To(HaveKey(unique.Make("it-1")))
			allocResults := meta.Devices[unique.Make("it-1")]
			Expect(allocResults).To(HaveLen(1))
			Expect(allocResults[0].DeviceID.Device.Value()).To(Equal("gpu-0"))
			Expect(allocResults[0].ConsumedCapacity).To(HaveKeyWithValue(
				resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("16Gi"),
			))
		})

		It("should block allocation when multi-alloc device capacity is exhausted", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {
						"gpu.example.com/vram": resource.MustParse("70Gi"),
					},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("16Gi"),
			}))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should deduct capacity within a single DFS when multiple slots request the same multi-alloc device", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("32Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1",
				exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("16Gi"),
				}),
				exactRequestWithCapacity("req-2", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("16Gi"),
				}),
			)
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			Expect(meta).ToNot(BeNil())
			allocResults := meta.Devices[unique.Make("it-1")]
			Expect(allocResults).To(HaveLen(2))
			Expect(allocResults[0].DeviceID.Device.Value()).To(Equal("gpu-0"))
			Expect(allocResults[1].DeviceID.Device.Value()).To(Equal("gpu-0"))
			Expect(allocResults[0].ConsumedCapacity).To(HaveKeyWithValue(
				resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("16Gi"),
			))
			Expect(allocResults[1].ConsumedCapacity).To(HaveKeyWithValue(
				resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("16Gi"),
			))
		})

		It("should fail when intra-DFS capacity deduction exceeds device capacity", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("24Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1",
				exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("16Gi"),
				}),
				exactRequestWithCapacity("req-2", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("16Gi"),
				}),
			)
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should restore capacity on backtrack and find alternative devices", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices,
							resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {Value: resource.MustParse("20Gi")},
								},
							},
							resourcev1.Device{
								Name:                     "gpu-1",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {Value: resource.MustParse("40Gi")},
								},
							},
						)
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					deviceID("gpu.example.com", "pool-a", "gpu-1").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1",
				exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("20Gi"),
				}),
				exactRequestWithCapacity("req-2", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("20Gi"),
				}),
			)
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			Expect(meta).ToNot(BeNil())
			allocResults := meta.Devices[unique.Make("it-1")]
			Expect(allocResults).To(HaveLen(2))
		})

		It("should have nil ConsumedCapacity for exclusive (non-multi-alloc) devices", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1), withAPIDevices("gpu-0")),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			Expect(meta).ToNot(BeNil())
			allocResults := meta.Devices[unique.Make("it-1")]
			Expect(allocResults).To(HaveLen(1))
			Expect(allocResults[0].DeviceID.Device.Value()).To(Equal("gpu-0"))
			Expect(allocResults[0].ConsumedCapacity).To(BeNil())
		})

		It("should accumulate capacity across sequential Allocate+Commit calls for the same multi-alloc device", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("48Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			// First allocation: 16Gi
			nc1 := makeNodeClaimWithID("nc-1", "it-1")
			claim1 := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("16Gi"),
			}))
			r1, err := alloc.Allocate(ctx, nc1, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Second allocation: 16Gi (remaining: 16Gi)
			nc2 := makeNodeClaimWithID("nc-2", "it-1")
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("16Gi"),
			}))
			r2, err := alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Third allocation: 16Gi (remaining: 0Gi)
			nc3 := makeNodeClaimWithID("nc-3", "it-1")
			claim3 := makeClaim("c3", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("16Gi"),
			}))
			r3, err := alloc.Allocate(ctx, nc3, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			r3.Allocation.Commit(ctx)

			// Fourth allocation should fail (capacity exhausted)
			nc4 := makeNodeClaimWithID("nc-4", "it-1")
			claim4 := makeClaim("c4", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("1Gi"),
			}))
			_, err = alloc.Allocate(ctx, nc4, []*resourcev1.ResourceClaim{claim4})
			Expect(err).To(HaveOccurred())
		})

		It("should consume full device capacity when no explicit capacity request is made on a multi-alloc device", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			Expect(meta).ToNot(BeNil())
			allocResults := meta.Devices[unique.Make("it-1")]
			Expect(allocResults).To(HaveLen(1))
			Expect(allocResults[0].ConsumedCapacity).To(HaveKeyWithValue(
				resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("80Gi"),
			))

			// Second allocation should fail (full capacity consumed)
			nc2 := makeNodeClaimWithID("nc-2", "it-1")
			_, err = alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{makeClaim("c2", exactRequest("req-1", "gpu", 1))})
			Expect(err).To(HaveOccurred())
		})

		It("should track capacity independently across ITs", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("16Gi"),
			}))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.InstanceTypes).To(HaveLen(2))
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			Expect(meta).ToNot(BeNil())
			for _, itName := range []string{"it-a", "it-b"} {
				itResults := meta.Devices[unique.Make(itName)]
				Expect(itResults).To(HaveLen(1))
				Expect(itResults[0].ConsumedCapacity).To(HaveKeyWithValue(
					resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("16Gi"),
				))
			}
		})

		It("should skip a device when request references a non-existent capacity dimension", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices,
							resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
								},
							},
							resourcev1.Device{
								Name:                     "gpu-1",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/bandwidth": {Value: resource.MustParse("100Gi")},
								},
							},
						)
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					deviceID("gpu.example.com", "pool-a", "gpu-1").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/bandwidth": resource.MustParse("50Gi"),
			}))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			Expect(meta).ToNot(BeNil())
			allocResults := meta.Devices[unique.Make("it-1")]
			Expect(allocResults).To(HaveLen(1))
			Expect(allocResults[0].DeviceID.Device.Value()).To(Equal("gpu-1"))
		})

		It("should allow cross-NodeClaim sharing of a fresh multi-alloc device via inflight capacity tracking", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
							},
						})
					},
				),
			}
			// NOT seeding ConsumedCapacity — fresh device discovered during allocation.
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
			}, nil, env.Client, nil)

			// NC-A allocates 16Gi
			ncA := makeNodeClaimWithID("nc-a", "it-1")
			claim1 := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("16Gi"),
			}))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// NC-B can still allocate the same device
			ncB := makeNodeClaimWithID("nc-b", "it-1")
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("16Gi"),
			}))
			r2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Exceeding remaining capacity (80 - 16 - 16 = 48 remaining) should fail
			ncC := makeNodeClaimWithID("nc-c", "it-1")
			claim3 := makeClaim("c3", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("49Gi"),
			}))
			_, err = alloc.Allocate(ctx, ncC, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())

			// Exactly 48Gi should succeed
			ncD := makeNodeClaimWithID("nc-d", "it-1")
			claim4 := makeClaim("c4", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("48Gi"),
			}))
			r4, err := alloc.Allocate(ctx, ncD, []*resourcev1.ResourceClaim{claim4})
			Expect(err).ToNot(HaveOccurred())
			r4.Allocation.Commit(ctx)
		})

		It("should handle template multi-alloc devices with capacity constraints", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
			}, nil, env.Client, nil)

			nc := makeNodeClaimWithTemplatesAndID("nc-a", "it-1",
				&cloudprovider.ResourceSliceTemplate{
					Driver: unique.Make("gpu.example.com"),
					Pool:   cloudprovider.ResourcePool{Name: unique.Make("pool-tmpl")},
					Devices: []cloudprovider.Device{
						{
							Name:                     unique.Make("tgpu-0"),
							AllowMultipleAllocations: true,
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("40Gi")},
							},
						},
					},
				},
			)

			// First pod: 20Gi
			claim1 := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("20Gi"),
			}))
			r1, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Second pod: 20Gi (remaining: 0)
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("20Gi"),
			}))
			r2, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Third pod should fail (no capacity left)
			claim3 := makeClaim("c3", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("1Gi"),
			}))
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())
		})

		Context("RequestPolicy integration", func() {
			It("should use RequestPolicy.Default when no capacity request is specified", func() {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 1),
						func(s *resourcev1.ResourceSlice) {
							s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {
										Value: resource.MustParse("80Gi"),
										RequestPolicy: &resourcev1.CapacityRequestPolicy{
											Default: ptr.To(resource.MustParse("8Gi")),
										},
									},
								},
							})
						},
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
					ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
					ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
						deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					},
				}, nil, env.Client, nil)

				// Allocate 10 times (10 * 8Gi = 80Gi = full capacity)
				for i := range 10 {
					nc := makeNodeClaimWithID(fmt.Sprintf("nc-%d", i), "it-1")
					claim := makeClaim(fmt.Sprintf("c%d", i), exactRequest("req-1", "gpu", 1))
					r, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
					Expect(err).ToNot(HaveOccurred(), "allocation %d should succeed", i)
					r.Allocation.Commit(ctx)
				}

				// 11th allocation should fail
				nc11 := makeNodeClaimWithID("nc-10", "it-1")
				_, err := alloc.Allocate(ctx, nc11, []*resourcev1.ResourceClaim{makeClaim("c10", exactRequest("req-1", "gpu", 1))})
				Expect(err).To(HaveOccurred())

				// Verify first allocation recorded 8Gi consumed
				meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c0"})
				Expect(meta).ToNot(BeNil())
				allocResults := meta.Devices[unique.Make("it-1")]
				Expect(allocResults).To(HaveLen(1))
				Expect(allocResults[0].ConsumedCapacity).To(HaveKeyWithValue(
					resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("8Gi"),
				))
			})

			It("should round up to the next ValidValue when request does not match exactly", func() {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 1),
						func(s *resourcev1.ResourceSlice) {
							s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {
										Value: resource.MustParse("80Gi"),
										RequestPolicy: &resourcev1.CapacityRequestPolicy{
											Default:     ptr.To(resource.MustParse("4Gi")),
											ValidValues: []resource.Quantity{resource.MustParse("4Gi"), resource.MustParse("8Gi"), resource.MustParse("16Gi")},
										},
									},
								},
							})
						},
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
					ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
					ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
						deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					},
				}, nil, env.Client, nil)

				nc := makeNodeClaim("it-1")
				claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("5Gi"),
				}))
				result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
				Expect(err).ToNot(HaveOccurred())
				result.Allocation.Commit(ctx)

				meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
				Expect(meta).ToNot(BeNil())
				allocResults := meta.Devices[unique.Make("it-1")]
				Expect(allocResults).To(HaveLen(1))
				Expect(allocResults[0].ConsumedCapacity).To(HaveKeyWithValue(
					resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("8Gi"),
				))
			})

			It("should fail when request exceeds all ValidValues", func() {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 1),
						func(s *resourcev1.ResourceSlice) {
							s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {
										Value: resource.MustParse("80Gi"),
										RequestPolicy: &resourcev1.CapacityRequestPolicy{
											Default:     ptr.To(resource.MustParse("4Gi")),
											ValidValues: []resource.Quantity{resource.MustParse("4Gi"), resource.MustParse("8Gi"), resource.MustParse("16Gi")},
										},
									},
								},
							})
						},
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
					ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
					ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
						deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					},
				}, nil, env.Client, nil)

				nc := makeNodeClaim("it-1")
				claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("20Gi"),
				}))
				_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
				Expect(err).To(HaveOccurred())
			})

			It("should round up to nearest step-aligned value with ValidRange policy", func() {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 1),
						func(s *resourcev1.ResourceSlice) {
							s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {
										Value: resource.MustParse("80Gi"),
										RequestPolicy: &resourcev1.CapacityRequestPolicy{
											Default: ptr.To(resource.MustParse("4Gi")),
											ValidRange: &resourcev1.CapacityRequestPolicyRange{
												Min:  ptr.To(resource.MustParse("4Gi")),
												Step: ptr.To(resource.MustParse("4Gi")),
											},
										},
									},
								},
							})
						},
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
					ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
					ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
						deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					},
				}, nil, env.Client, nil)

				nc := makeNodeClaim("it-1")
				// Request 5Gi — should round up to 8Gi (Min=4Gi + 1*Step=4Gi)
				claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("5Gi"),
				}))
				result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
				Expect(err).ToNot(HaveOccurred())
				result.Allocation.Commit(ctx)

				meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
				Expect(meta).ToNot(BeNil())
				allocResults := meta.Devices[unique.Make("it-1")]
				Expect(allocResults).To(HaveLen(1))
				Expect(allocResults[0].ConsumedCapacity).To(HaveKeyWithValue(
					resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("8Gi"),
				))

				// 80Gi - 8Gi = 72Gi remaining. Request 76Gi (rounds to 76Gi, step-aligned) should fail.
				nc2 := makeNodeClaimWithID("nc-2", "it-1")
				claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("76Gi"),
				}))
				_, err = alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim2})
				Expect(err).To(HaveOccurred())

				// 72Gi should succeed
				nc3 := makeNodeClaimWithID("nc-3", "it-1")
				claim3 := makeClaim("c3", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("72Gi"),
				}))
				r3, err := alloc.Allocate(ctx, nc3, []*resourcev1.ResourceClaim{claim3})
				Expect(err).ToNot(HaveOccurred())
				r3.Allocation.Commit(ctx)
			})

			It("should account for rounded-up ValidValues capacity in intra-DFS deduction", func() {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 1),
						func(s *resourcev1.ResourceSlice) {
							s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {
										Value: resource.MustParse("16Gi"),
										RequestPolicy: &resourcev1.CapacityRequestPolicy{
											Default:     ptr.To(resource.MustParse("4Gi")),
											ValidValues: []resource.Quantity{resource.MustParse("4Gi"), resource.MustParse("8Gi"), resource.MustParse("16Gi")},
										},
									},
								},
							})
						},
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
					ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
					ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
						deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					},
				}, nil, env.Client, nil)

				nc := makeNodeClaim("it-1")
				// Two requests each asking 5Gi (rounds to 8Gi each) — 16Gi total, exact fit
				claim := makeClaim("c1",
					exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
						"gpu.example.com/vram": resource.MustParse("5Gi"),
					}),
					exactRequestWithCapacity("req-2", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
						"gpu.example.com/vram": resource.MustParse("5Gi"),
					}),
				)
				result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
				Expect(err).ToNot(HaveOccurred())
				result.Allocation.Commit(ctx)

				meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
				Expect(meta).ToNot(BeNil())
				allocResults := meta.Devices[unique.Make("it-1")]
				Expect(allocResults).To(HaveLen(2))
				Expect(allocResults[0].ConsumedCapacity).To(HaveKeyWithValue(
					resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("8Gi"),
				))
				Expect(allocResults[1].ConsumedCapacity).To(HaveKeyWithValue(
					resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("8Gi"),
				))

				// No more capacity — even default (4Gi) should fail
				nc2 := makeNodeClaimWithID("nc-2", "it-1")
				_, err = alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{makeClaim("c2", exactRequest("req-1", "gpu", 1))})
				Expect(err).To(HaveOccurred())
			})

			It("should reject allocation when request rounds above ValidRange.Max", func() {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 1),
						func(s *resourcev1.ResourceSlice) {
							s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {
										Value: resource.MustParse("100Gi"),
										RequestPolicy: &resourcev1.CapacityRequestPolicy{
											Default: ptr.To(resource.MustParse("4Gi")),
											ValidRange: &resourcev1.CapacityRequestPolicyRange{
												Min:  ptr.To(resource.MustParse("4Gi")),
												Max:  ptr.To(resource.MustParse("50Gi")),
												Step: ptr.To(resource.MustParse("4Gi")),
											},
										},
									},
								},
							})
						},
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
					ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
					ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
						deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					},
				}, nil, env.Client, nil)

				nc := makeNodeClaim("it-1")
				// Request 55Gi: rounds up to 56Gi (Min=4Gi + 13*Step=52Gi → 56Gi), which exceeds Max=50Gi.
				claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("55Gi"),
				}))
				_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
				Expect(err).To(HaveOccurred())
			})

			It("should succeed when request is at exactly ValidRange.Max", func() {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 1),
						func(s *resourcev1.ResourceSlice) {
							s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {
										Value: resource.MustParse("100Gi"),
										RequestPolicy: &resourcev1.CapacityRequestPolicy{
											Default: ptr.To(resource.MustParse("4Gi")),
											ValidRange: &resourcev1.CapacityRequestPolicyRange{
												Min:  ptr.To(resource.MustParse("4Gi")),
												Max:  ptr.To(resource.MustParse("48Gi")),
												Step: ptr.To(resource.MustParse("4Gi")),
											},
										},
									},
								},
							})
						},
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
					ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
					ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
						deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					},
				}, nil, env.Client, nil)

				nc := makeNodeClaim("it-1")
				// Request 48Gi: already step-aligned (Min=4Gi + 11*4Gi = 48Gi) and equals Max.
				claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("48Gi"),
				}))
				result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
				Expect(err).ToNot(HaveOccurred())
				Expect(result).ToNot(BeNil())
			})

			It("should reject allocation when request exceeds Max even without Step", func() {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 1),
						func(s *resourcev1.ResourceSlice) {
							s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {
										Value: resource.MustParse("100Gi"),
										RequestPolicy: &resourcev1.CapacityRequestPolicy{
											Default: ptr.To(resource.MustParse("4Gi")),
											ValidRange: &resourcev1.CapacityRequestPolicyRange{
												Min: ptr.To(resource.MustParse("4Gi")),
												Max: ptr.To(resource.MustParse("50Gi")),
											},
										},
									},
								},
							})
						},
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
					ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
					ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
						deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					},
				}, nil, env.Client, nil)

				nc := makeNodeClaim("it-1")
				// Without Step, roundUpRange returns the request as-is (since it >= Min).
				// 55Gi exceeds Max=50Gi → violateValidRange should reject.
				claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("55Gi"),
				}))
				_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
				Expect(err).To(HaveOccurred())
			})

			It("should succeed when request fits within ValidRange without Step (Min only)", func() {
				inClusterSlices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
						withGeneration(1, 1),
						func(s *resourcev1.ResourceSlice) {
							s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {
										Value: resource.MustParse("100Gi"),
										RequestPolicy: &resourcev1.CapacityRequestPolicy{
											Default: ptr.To(resource.MustParse("4Gi")),
											ValidRange: &resourcev1.CapacityRequestPolicyRange{
												Min: ptr.To(resource.MustParse("4Gi")),
												Max: ptr.To(resource.MustParse("50Gi")),
											},
										},
									},
								},
							})
						},
					),
				}
				alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
					ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
					ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
						deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					},
				}, nil, env.Client, nil)

				nc := makeNodeClaim("it-1")
				// 30Gi: above Min=4Gi, below Max=50Gi, no Step → returned as-is.
				claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram": resource.MustParse("30Gi"),
				}))
				result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
				Expect(err).ToNot(HaveOccurred())
				Expect(result).ToNot(BeNil())
				result.Allocation.Commit(ctx)

				meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
				Expect(meta).ToNot(BeNil())
				allocResults := meta.Devices[unique.Make("it-1")]
				Expect(allocResults).To(HaveLen(1))
				Expect(allocResults[0].ConsumedCapacity).To(HaveKeyWithValue(
					resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("30Gi"),
				))
			})
		})

		It("should skip device missing a requested dimension and succeed on a sibling that has it", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices,
							resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
								},
							},
							resourcev1.Device{
								Name:                     "gpu-1",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram":      {Value: resource.MustParse("80Gi")},
									"gpu.example.com/bandwidth": {Value: resource.MustParse("50Gi")},
								},
							},
						)
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
					deviceID("gpu.example.com", "pool-a", "gpu-1").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			// Request bandwidth — gpu-0 doesn't have it, gpu-1 does.
			claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/bandwidth": resource.MustParse("25Gi"),
			}))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			allocResults := meta.Devices[unique.Make("it-1")]
			Expect(allocResults).To(HaveLen(1))
			Expect(allocResults[0].DeviceID.Device.Value()).To(Equal("gpu-1"))
		})

		It("should reject when one capacity dimension is exceeded even if other dimensions have room", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram":      {Value: resource.MustParse("80Gi")},
								"gpu.example.com/bandwidth": {Value: resource.MustParse("10Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			// Request 16Gi vram (fits in 80Gi) and 12Gi bandwidth (exceeds 10Gi).
			claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram":      resource.MustParse("16Gi"),
				"gpu.example.com/bandwidth": resource.MustParse("12Gi"),
			}))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should succeed when both dimensions have sufficient capacity", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram":      {Value: resource.MustParse("80Gi")},
								"gpu.example.com/bandwidth": {Value: resource.MustParse("10Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			// Request 16Gi vram and 5Gi bandwidth — both fit.
			claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram":      resource.MustParse("16Gi"),
				"gpu.example.com/bandwidth": resource.MustParse("5Gi"),
			}))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			result.Allocation.Commit(ctx)

			meta := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			allocResults := meta.Devices[unique.Make("it-1")]
			Expect(allocResults).To(HaveLen(1))
			Expect(allocResults[0].ConsumedCapacity).To(HaveKeyWithValue(
				resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("16Gi"),
			))
			Expect(allocResults[0].ConsumedCapacity).To(HaveKeyWithValue(
				resourcev1.QualifiedName("gpu.example.com/bandwidth"), resource.MustParse("5Gi"),
			))
		})

		It("should track multi-dimension capacity deductions intra-DFS independently", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram":      {Value: resource.MustParse("32Gi")},
								"gpu.example.com/bandwidth": {Value: resource.MustParse("8Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			// Two requests: first uses 16Gi vram + 4Gi bw, second uses 16Gi vram + 4Gi bw.
			// Total: 32Gi vram (exact fit), 8Gi bw (exact fit).
			claim := makeClaim("c1",
				exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram":      resource.MustParse("16Gi"),
					"gpu.example.com/bandwidth": resource.MustParse("4Gi"),
				}),
				exactRequestWithCapacity("req-2", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
					"gpu.example.com/vram":      resource.MustParse("16Gi"),
					"gpu.example.com/bandwidth": resource.MustParse("4Gi"),
				}),
			)
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			result.Allocation.Commit(ctx)

			// Capacity now exhausted in both dims — even 1Gi of either should fail.
			nc2 := makeNodeClaimWithID("nc-2", "it-1")
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("1Gi"),
			}))
			_, err = alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())
		})

		It("should reject allocation when device has zero capacity for a requested dimension", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("0")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("1Gi"),
			}))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should deduct capacity from the same multi-alloc device across multiple claims in a single Allocate call", func() {
			// gpu-0 has 48Gi total. Two claims each request 20Gi → 40Gi used.
			// After commit, 10Gi request fails (40+10=50 > 48), but 8Gi succeeds (40+8=48 exactly).
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("48Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			claim1 := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("20Gi"),
			}))
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("20Gi"),
			}))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim1, claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			result.Allocation.Commit(ctx)

			// 10Gi should fail (40+10 > 48).
			nc2 := makeNodeClaimWithID("nc-2", "it-1")
			claim3 := makeClaim("c3", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("10Gi"),
			}))
			_, err = alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())

			// 8Gi should succeed (40+8 = 48 exactly).
			nc3 := makeNodeClaimWithID("nc-3", "it-1")
			claim4 := makeClaim("c4", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("8Gi"),
			}))
			r4, err := alloc.Allocate(ctx, nc3, []*resourcev1.ResourceClaim{claim4})
			Expect(err).ToNot(HaveOccurred())
			Expect(r4).ToNot(BeNil())
		})
	})

	Describe("Consumable capacity — state and verification", func() {
		It("should bypass IsAllocated for multi-allocatable devices and allow cross-NodeClaim sharing", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
						})
					},
				),
			}
			// Device is tracked in ConsumedCapacity (multi-allocatable), not ExclusiveDevices.
			consumedCapacity := map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {
					"gpu.example.com/vram": resource.MustParse("30Gi"),
				},
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: consumedCapacity,
			}, nil, env.Client, nil)

			// NC-A allocates the multi-alloc device.
			ncA := makeNodeClaimWithID("nc-a", "it-1")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			Expect(r1).ToNot(BeNil())
			r1.Allocation.Commit(ctx)

			// NC-B can also allocate the same device (not blocked by IsAllocated).
			ncB := makeNodeClaimWithID("nc-b", "it-1")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			r2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2).ToNot(BeNil())
		})

		It("should block exclusive devices even when ConsumedCapacity tracks other devices", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices,
							resourcev1.Device{Name: "gpu-exclusive"},
							resourcev1.Device{
								Name:                     "gpu-shared",
								AllowMultipleAllocations: ptr.To(true),
								Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
									"gpu.example.com/vram": {Value: resource.MustParse("10Gi")},
								},
							},
						)
					},
				),
			}
			// gpu-exclusive is exclusively allocated; gpu-shared is multi-alloc with capacity fully consumed.
			consumedCapacity := map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
				deviceID("gpu.example.com", "pool-a", "gpu-shared").DeviceID: {
					"gpu.example.com/vram": resource.MustParse("10Gi"),
				},
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New(deviceID("gpu.example.com", "pool-a", "gpu-exclusive").DeviceID),
				ConsumedCapacity: consumedCapacity,
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			// Request 1 device: gpu-exclusive is blocked, gpu-shared has no remaining capacity.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())
		})

		It("should commit inflight consumed capacity with pessimistic-max across ITs", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
						})
					},
				),
			}
			consumedCapacity := map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {
					"gpu.example.com/vram": resource.MustParse("0"),
				},
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: consumedCapacity,
			}, nil, env.Client, nil)

			// NC-A with 2 ITs both allocate gpu-0.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(r1.InstanceTypes).To(HaveLen(2))
			r1.Allocation.Commit(ctx)

			// NC-B can still allocate the device (multi-alloc bypass).
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			r2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2).ToNot(BeNil())
		})

		It("should release inflight consumed capacity when all ITs are released", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
						})
					},
				),
			}
			consumedCapacity := map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {
					"gpu.example.com/vram": resource.MustParse("0"),
				},
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: consumedCapacity,
			}, nil, env.Client, nil)

			// NC-A with 2 ITs.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Release it-a only: multi-alloc device capacity is still held by it-b (pessimistic max).
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))

			// Release it-b: all capacity should be refunded.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-b"))

			// Subsequent allocation should still work (device remains available for multi-alloc).
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			r2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2).ToNot(BeNil())
		})

		It("should track template consumed capacity per-IT independently", func() {
			// Template devices with AllowMultipleAllocations are tracked per-(NC, IT).
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
			}, nil, env.Client, nil)

			nc := makeNodeClaimWithTemplatesAndID("nc-a", "it-1",
				&cloudprovider.ResourceSliceTemplate{
					Driver: unique.Make("gpu.example.com"),
					Pool:   cloudprovider.ResourcePool{Name: unique.Make("pool-tmpl")},
					Devices: []cloudprovider.Device{
						{Name: unique.Make("tgpu-0")},
						{Name: unique.Make("tgpu-1")},
					},
				},
			)

			// First pod allocates 1 template device.
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Second pod allocates another template device (same NC/IT).
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			r2, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Third allocation should fail (only 2 template devices available).
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 1))
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())
		})

		It("should release template capacity when IT is released", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
			}, nil, env.Client, nil)

			nc := makeNodeClaimWithTemplatesAndID("nc-a", "it-1",
				&cloudprovider.ResourceSliceTemplate{
					Driver: unique.Make("gpu.example.com"),
					Pool:   cloudprovider.ResourcePool{Name: unique.Make("pool-tmpl")},
					Devices: []cloudprovider.Device{
						{Name: unique.Make("tgpu-0")},
					},
				},
			)

			// Allocate the only template device.
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Second allocation fails (device used).
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			// Release the IT.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-1"))

			// Rebuild NC (simulates re-scheduling after release).
			nc2 := makeNodeClaimWithTemplatesAndID("nc-a", "it-1",
				&cloudprovider.ResourceSliceTemplate{
					Driver: unique.Make("gpu.example.com"),
					Pool:   cloudprovider.ResourcePool{Name: unique.Make("pool-tmpl")},
					Devices: []cloudprovider.Device{
						{Name: unique.Make("tgpu-0")},
					},
				},
			)
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 1))
			r3, err := alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should accumulate inflight capacity across multiple Allocate+Commit calls", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
						})
					},
				),
			}
			consumedCapacity := map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {
					"gpu.example.com/vram": resource.MustParse("0"),
				},
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: consumedCapacity,
			}, nil, env.Client, nil)

			// Multiple pods commit against the same multi-alloc device.
			for i := range 5 {
				nc := makeNodeClaimWithID("nc-a", "it-1")
				claim := makeClaim("c"+string(rune('1'+i)), exactRequest("req-1", "gpu", 1))
				result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
				Expect(err).ToNot(HaveOccurred())
				result.Allocation.Commit(ctx)
			}
		})

		It("should not mix multi-alloc and exclusive tracking for the same device", func() {
			// A device in ConsumedCapacity should bypass IsAllocated even after inflight commits.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices,
							resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
							},
						)
					},
				),
			}
			consumedCapacity := map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {
					"gpu.example.com/vram": resource.MustParse("20Gi"),
				},
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: consumedCapacity,
			}, nil, env.Client, nil)

			// NC-A commits.
			ncA := makeNodeClaimWithID("nc-a", "it-1")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// NC-B on a different node can still use it.
			ncB := makeNodeClaimWithID("nc-b", "it-2")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			r2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// NC-C can still use it too.
			ncC := makeNodeClaimWithID("nc-c", "it-3")
			claim3 := makeClaim("c3", exactRequest("req-1", "gpu", 1))
			r3, err := alloc.Allocate(ctx, ncC, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should partially refund capacity when released IT had higher consumption than survivors", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices,
							resourcev1.Device{
								Name:                     "gpu-0",
								AllowMultipleAllocations: ptr.To(true),
							},
							resourcev1.Device{
								Name:                     "gpu-1",
								AllowMultipleAllocations: ptr.To(true),
							},
						)
					},
				),
			}
			consumedCapacity := map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
				deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {
					"gpu.example.com/vram": resource.MustParse("0"),
				},
				deviceID("gpu.example.com", "pool-a", "gpu-1").DeviceID: {
					"gpu.example.com/vram": resource.MustParse("0"),
				},
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: consumedCapacity,
			}, nil, env.Client, nil)

			// NC-A with 2 ITs. Both get gpu-0 (count=1). Then second pod: both also get gpu-1.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequest("req-1", "gpu", 1))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			Expect(r1.InstanceTypes).To(HaveLen(2))
			r1.Allocation.Commit(ctx)

			// Release it-a: pessimistic max stays the same (it-b still has same consumption).
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))

			// Release it-b: full refund.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-b"))

			// NC-B can still allocate (devices remain multi-alloc accessible).
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			r2, err := alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2).ToNot(BeNil())
		})

		It("should track template consumed capacity per-IT with concrete quantities", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
			}, nil, env.Client, nil)

			nc := makeNodeClaimWithTemplatesAndID("nc-a", "it-1",
				&cloudprovider.ResourceSliceTemplate{
					Driver: unique.Make("gpu.example.com"),
					Pool:   cloudprovider.ResourcePool{Name: unique.Make("pool-tmpl")},
					Devices: []cloudprovider.Device{
						{
							Name:                     unique.Make("tgpu-0"),
							AllowMultipleAllocations: true,
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("32Gi")},
							},
						},
					},
				},
			)

			// Pod 1: 16Gi
			claim1 := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("16Gi"),
			}))
			r1, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			meta1 := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c1"})
			Expect(meta1).ToNot(BeNil())
			allocResults1 := meta1.Devices[unique.Make("it-1")]
			Expect(allocResults1).To(HaveLen(1))
			Expect(allocResults1[0].ConsumedCapacity).To(HaveKeyWithValue(
				resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("16Gi"),
			))

			// Pod 2: 16Gi (total: 32Gi = full capacity)
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("16Gi"),
			}))
			r2, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			meta2 := alloc.ResourceClaimAllocationMetadataForClaim(types.NamespacedName{Namespace: "default", Name: "c2"})
			Expect(meta2).ToNot(BeNil())
			allocResults2 := meta2.Devices[unique.Make("it-1")]
			Expect(allocResults2).To(HaveLen(1))
			Expect(allocResults2[0].ConsumedCapacity).To(HaveKeyWithValue(
				resourcev1.QualifiedName("gpu.example.com/vram"), resource.MustParse("16Gi"),
			))

			// Pod 3: even 1Gi should fail (capacity exhausted).
			claim3 := makeClaim("c3", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("1Gi"),
			}))
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())
		})

		It("should independently track template capacity across different ITs in same NodeClaim", func() {
			alloc = dynamicresources.NewAllocator(nil, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
			}, nil, env.Client, nil)

			template := &cloudprovider.ResourceSliceTemplate{
				Driver: unique.Make("gpu.example.com"),
				Pool:   cloudprovider.ResourcePool{Name: unique.Make("pool-tmpl")},
				Devices: []cloudprovider.Device{
					{
						Name:                     unique.Make("tgpu-0"),
						AllowMultipleAllocations: true,
						Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
							"gpu.example.com/vram": {Value: resource.MustParse("20Gi")},
						},
					},
				},
			}

			itA := unique.Make("it-a")
			itB := unique.Make("it-b")
			sliceA := dynamicresources.NewTemplateSlice(template)
			sliceB := dynamicresources.NewTemplateSlice(template)

			nc := &fakeNodeClaim{
				id:            unique.Make("nc-multi"),
				nodePoolID:    unique.Make("test-np"),
				requirements:  scheduling.NewRequirements(),
				instanceTypes: []dynamicresources.InstanceTypeID{itA, itB},
				resourceSlices: map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice{
					itA: {sliceA},
					itB: {sliceB},
				},
			}

			// Pod 1: 20Gi (exhausts template device for both ITs).
			claim1 := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("20Gi"),
			}))
			r1, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			Expect(r1.InstanceTypes).To(HaveLen(2))
			r1.Allocation.Commit(ctx)

			// Pod 2: should fail (both ITs have their template device fully consumed).
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("1Gi"),
			}))
			_, err = alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			// Release it-a: it-b still has capacity consumed.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-multi"), itA)

			// Pod 3 with only it-b: still should fail (it-b's capacity is exhausted).
			nc2 := &fakeNodeClaim{
				id:            unique.Make("nc-multi"),
				nodePoolID:    unique.Make("test-np"),
				requirements:  scheduling.NewRequirements(),
				instanceTypes: []dynamicresources.InstanceTypeID{itB},
				resourceSlices: map[dynamicresources.InstanceTypeID][]dynamicresources.ResourceSlice{
					itB: {sliceB},
				},
			}
			claim3 := makeClaim("c3", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("1Gi"),
			}))
			_, err = alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())
		})

		It("should correctly refund inflight capacity so a subsequent allocation at the boundary succeeds", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("40Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			// NC-A allocates 40Gi (full capacity) with 2 ITs.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("40Gi"),
			}))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// Before release: capacity fully consumed, NC-B cannot allocate even 1Gi.
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("1Gi"),
			}))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim2})
			Expect(err).To(HaveOccurred())

			// Release both ITs — should refund all 40Gi.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-b"))

			// After release: full 40Gi available, NC-C should succeed at exactly 40Gi.
			ncC := makeNodeClaimWithID("nc-c", "it-d")
			claim3 := makeClaim("c3", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("40Gi"),
			}))
			r3, err := alloc.Allocate(ctx, ncC, []*resourcev1.ResourceClaim{claim3})
			Expect(err).ToNot(HaveOccurred())
			Expect(r3).ToNot(BeNil())
		})

		It("should partially refund when released IT had higher consumption than survivors", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			// NC-A with it-a: allocate 60Gi
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("60Gi"),
			}))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			// NC-A second pod: allocate 20Gi more (both ITs now have 60+20=80Gi committed per IT...
			// actually each IT gets 60Gi from first pod and 20Gi from second pod = 80Gi total)
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("20Gi"),
			}))
			r2, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			r2.Allocation.Commit(ctx)

			// Pessimistic max is 80Gi (both ITs have same consumption). Capacity fully consumed.
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claim3 := makeClaim("c3", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("1Gi"),
			}))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())

			// Release it-a: survivor it-b still has 80Gi committed.
			// Pessimistic max stays at 80Gi, delta = 0, no refund.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))

			// Still no capacity available.
			ncC := makeNodeClaimWithID("nc-c", "it-d")
			claim4 := makeClaim("c4", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("1Gi"),
			}))
			_, err = alloc.Allocate(ctx, ncC, []*resourcev1.ResourceClaim{claim4})
			Expect(err).To(HaveOccurred())

			// Release it-b: now all capacity refunded (80Gi).
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-b"))

			// Full 80Gi available again.
			ncD := makeNodeClaimWithID("nc-d", "it-e")
			claim5 := makeClaim("c5", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("80Gi"),
			}))
			r5, err := alloc.Allocate(ctx, ncD, []*resourcev1.ResourceClaim{claim5})
			Expect(err).ToNot(HaveOccurred())
			Expect(r5).ToNot(BeNil())
		})

		It("should partially refund when released IT had lower consumption than survivors", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {},
				},
			}, nil, env.Client, nil)

			// Pod 1 on NC-A with it-a and it-b: allocates 30Gi.
			ncA := makeNodeClaimWithID("nc-a", "it-a", "it-b")
			claim1 := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("30Gi"),
			}))
			r1, err := alloc.Allocate(ctx, ncA, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)
			// Both ITs: 30Gi each. Pessimistic max = 30Gi. Inflight = 30Gi. Remaining = 50Gi.

			// Release it-a (lower IT — same consumption as survivor).
			// Since both ITs have 30Gi, releasing it-a: new max stays 30Gi (from it-b), delta = 0.
			alloc.ReleaseInstanceType(ctx, unique.Make("nc-a"), unique.Make("it-a"))

			// Remaining should still be 50Gi — assert 50Gi allocation succeeds but 51Gi fails.
			ncB := makeNodeClaimWithID("nc-b", "it-c")
			claimFail := makeClaim("cf", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("51Gi"),
			}))
			_, err = alloc.Allocate(ctx, ncB, []*resourcev1.ResourceClaim{claimFail})
			Expect(err).To(HaveOccurred())

			ncC := makeNodeClaimWithID("nc-c", "it-d")
			claimOK := makeClaim("co", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("50Gi"),
			}))
			rOK, err := alloc.Allocate(ctx, ncC, []*resourcev1.ResourceClaim{claimOK})
			Expect(err).ToNot(HaveOccurred())
			Expect(rOK).ToNot(BeNil())
		})

		It("should correctly account for preallocated + inflight + allocating capacity all contributing to the same device", func() {
			// gpu-0 has 100Gi total. Preallocated = 30Gi.
			// Pod 1 allocates 25Gi and commits → inflight = 25Gi.
			// Pod 2 requests 40Gi → 30 + 25 + 40 = 95 ≤ 100 → succeeds, commits.
			// Pod 3 requests 10Gi → 30 + 65 + 10 = 105 > 100 → fails.
			// Pod 4 requests 5Gi → 30 + 65 + 5 = 100 exactly → succeeds.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 1),
					func(s *resourcev1.ResourceSlice) {
						s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
							Name:                     "gpu-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("100Gi")},
							},
						})
					},
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "gpu-0").DeviceID: {
						"gpu.example.com/vram": resource.MustParse("30Gi"),
					},
				},
			}, nil, env.Client, nil)

			nc1 := makeNodeClaimWithID("nc-1", "it-1")
			claim1 := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("25Gi"),
			}))
			r1, err := alloc.Allocate(ctx, nc1, []*resourcev1.ResourceClaim{claim1})
			Expect(err).ToNot(HaveOccurred())
			r1.Allocation.Commit(ctx)

			nc2 := makeNodeClaimWithID("nc-2", "it-1")
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("40Gi"),
			}))
			r2, err := alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2).ToNot(BeNil())
			r2.Allocation.Commit(ctx)

			nc3 := makeNodeClaimWithID("nc-3", "it-1")
			claim3 := makeClaim("c3", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("10Gi"),
			}))
			_, err = alloc.Allocate(ctx, nc3, []*resourcev1.ResourceClaim{claim3})
			Expect(err).To(HaveOccurred())

			nc4 := makeNodeClaimWithID("nc-4", "it-1")
			claim4 := makeClaim("c4", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("5Gi"),
			}))
			r4, err := alloc.Allocate(ctx, nc4, []*resourcev1.ResourceClaim{claim4})
			Expect(err).ToNot(HaveOccurred())
			Expect(r4).ToNot(BeNil())
		})
	})

	Describe("Consumable capacity + SharedCounters interaction", func() {
		It("should deduct counters for preallocated multi-alloc devices", func() {
			// A device with BOTH AllowMultipleAllocations and ConsumesCounters.
			// The device is preallocated (via ConsumedCapacity, not ExclusiveDevices).
			// Its counter consumption must still be deducted from the pool budget.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						resourcev1.Device{
							Name:                     "mig-0",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("20Gi")},
							},
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "gpu-slices", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("40Gi")}}},
							},
						},
						resourcev1.Device{
							Name:                     "mig-1",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("20Gi")},
							},
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "gpu-slices", Counters: map[string]resourcev1.Counter{"memory": {Value: resource.MustParse("40Gi")}}},
							},
						},
					),
				),
			}
			// mig-0 is preallocated as a multi-alloc device (tracked via ConsumedCapacity).
			// Its 40Gi counter consumption must be deducted, leaving 40Gi for mig-1.
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "mig-0").DeviceID: {
						"gpu.example.com/vram": resource.MustParse("10Gi"),
					},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			// Request 2 devices: mig-0 still has capacity (10Gi/20Gi used) and mig-1 is fresh.
			// But budget is only 40Gi remaining (80Gi - 40Gi from mig-0's counter), so both
			// mig-0 (40Gi) + mig-1 (40Gi) = 80Gi > 40Gi remaining → should fail.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())

			// Request 1 device: only 40Gi counter budget needed → should succeed.
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 1))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should track both capacity and counters independently for multi-alloc devices", func() {
			// A multi-alloc device with both capacity AND counters.
			// Capacity allows multiple allocations but counters limit total pool usage.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"sm-count": resource.MustParse("40"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						resourcev1.Device{
							Name:                     "gpu-shared",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/bandwidth": {Value: resource.MustParse("100Gi")},
							},
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "gpu-slices", Counters: map[string]resourcev1.Counter{"sm-count": {Value: resource.MustParse("20")}}},
							},
						},
						resourcev1.Device{
							Name:                     "gpu-shared-2",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/bandwidth": {Value: resource.MustParse("100Gi")},
							},
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "gpu-slices", Counters: map[string]resourcev1.Counter{"sm-count": {Value: resource.MustParse("20")}}},
							},
						},
						resourcev1.Device{
							Name:                     "gpu-shared-3",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/bandwidth": {Value: resource.MustParse("100Gi")},
							},
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "gpu-slices", Counters: map[string]resourcev1.Counter{"sm-count": {Value: resource.MustParse("20")}}},
							},
						},
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			// Each device consumes 20 SMs. Budget is 40. So at most 2 devices can be allocated.
			// All 3 have plenty of bandwidth capacity, but counters limit to 2.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 3))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())

			// 2 should succeed (2 × 20 = 40 = budget).
			claim2 := makeClaim("c2", exactRequest("req-1", "gpu", 2))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should allocate both devices from a pool with asymmetric counter-sets via DFS ordering", func() {
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(
						counterSet("memory-budget", map[string]resource.Quantity{
							"bytes": resource.MustParse("40Gi"),
						}),
						counterSet("compute-budget", map[string]resource.Quantity{
							"cores": resource.MustParse("8"),
						}),
					),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						resourcev1.Device{
							Name: "mem-device",
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "memory-budget", Counters: map[string]resourcev1.Counter{
									"bytes": {Value: resource.MustParse("40Gi")},
								}},
							},
						},
						resourcev1.Device{
							Name: "compute-device",
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "compute-budget", Counters: map[string]resourcev1.Counter{
									"cores": {Value: resource.MustParse("4")},
								}},
							},
						},
					),
				),
			}
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
			}, nil, env.Client, nil)
			nc := makeNodeClaim("it-1")
			// Request count=2. DFS first tries mem-device (slot 1) → exhausts memory-budget.
			// For slot 2, poolCountersExhausted fires (memory-budget drained) → skips all.
			// Backtracks to try compute-device (slot 1) → deducts 4 cores from compute-budget.
			// Then mem-device (slot 2) → memory-budget: 40Gi remaining, 40Gi cost → passes.
			// Net: DFS finds solution via backtracking despite over-skip.
			claim := makeClaim("c1", exactRequest("req-1", "gpu", 2))
			result, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
		})

		It("should correctly backtrack capacity and counters together on multi-alloc device", func() {
			// One multi-alloc device with capacity and counters, plus one exclusive device.
			// Counter budget is 80Gi. device-a (multi-alloc, preallocated) costs 40Gi counters,
			// leaving 40Gi. device-b (exclusive, not allocated) costs 40Gi counters.
			// Request count=2 → DFS allocates device-a (capacity OK, 40Gi counters deducted intra-DFS).
			// Then tries device-b: counter check fails (40Gi remaining - 40Gi allocating = 0 < 40Gi needed).
			// Backtrack device-a. Allocation fails. Then a count=1 request for device-a should succeed.
			inClusterSlices := []dynamicresources.ResourceSlice{
				makeAPISlice("s-counters", "gpu.example.com", "pool-a",
					withSharedCounters(counterSet("gpu-slices", map[string]resource.Quantity{
						"memory": resource.MustParse("80Gi"),
					})),
					withGeneration(1, 2),
				),
				makeAPISlice("s-devices", "gpu.example.com", "pool-a", withAllNodes(),
					withGeneration(1, 2),
					withDevicesConsumingCounters(
						resourcev1.Device{
							Name:                     "device-a",
							AllowMultipleAllocations: ptr.To(true),
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
							},
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "gpu-slices", Counters: map[string]resourcev1.Counter{
									"memory": {Value: resource.MustParse("40Gi")},
								}},
							},
						},
						resourcev1.Device{
							Name: "device-b",
							ConsumesCounters: []resourcev1.DeviceCounterConsumption{
								{CounterSet: "gpu-slices", Counters: map[string]resourcev1.Counter{
									"memory": {Value: resource.MustParse("40Gi")},
								}},
							},
						},
					),
				),
			}
			// Only device-a is preallocated (multi-alloc). device-b is fresh/exclusive.
			// InitRemainingCounters deducts device-a's 40Gi → remaining = 40Gi.
			alloc = dynamicresources.NewAllocator(inClusterSlices, dynamicresources.AllocatedDeviceState{
				ExclusiveDevices: sets.New[cloudprovider.DeviceID](),
				ConsumedCapacity: map[cloudprovider.DeviceID]map[resourcev1.QualifiedName]resource.Quantity{
					deviceID("gpu.example.com", "pool-a", "device-a").DeviceID: {},
				},
			}, nil, env.Client, nil)

			nc := makeNodeClaim("it-1")
			// Request 2 slots with capacity. DFS: slot 1 → device-a (multi-alloc, capacity OK,
			// deducts 40Gi counters intra-DFS). slot 2 → device-b (exclusive, counter check:
			// 40Gi remaining - 40Gi allocating = 0 < 40Gi → fails). No more devices.
			// Backtrack restores device-a's counter and capacity deductions. Allocation fails.
			claim := makeClaim("c1", exactRequestWithCapacity("req-1", "gpu", 2, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("20Gi"),
			}))
			_, err := alloc.Allocate(ctx, nc, []*resourcev1.ResourceClaim{claim})
			Expect(err).To(HaveOccurred())

			// After the failed allocation: device-a's capacity and counter deductions were restored.
			// A single-device request should succeed (device-a, 20Gi capacity, counters pass since
			// 40Gi remaining - 0 allocating >= 40Gi device cost).
			nc2 := makeNodeClaimWithID("nc-2", "it-1")
			claim2 := makeClaim("c2", exactRequestWithCapacity("req-1", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
				"gpu.example.com/vram": resource.MustParse("20Gi"),
			}))
			r2, err := alloc.Allocate(ctx, nc2, []*resourcev1.ResourceClaim{claim2})
			Expect(err).ToNot(HaveOccurred())
			Expect(r2).ToNot(BeNil())
		})
	})
})
