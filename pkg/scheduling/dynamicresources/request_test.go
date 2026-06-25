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
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	dracel "k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Request Validation", func() {
	var (
		celCache *dracel.Cache
		pools    []*dynamicresources.Pool
	)

	BeforeEach(func() {
		celCache = dracel.NewCache(100, dracel.Features{EnableConsumableCapacity: true})

		// Create DeviceClasses in the API server.
		ExpectApplied(ctx, env.Client,
			&resourcev1.DeviceClass{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu"},
				Spec: resourcev1.DeviceClassSpec{
					Selectors: []resourcev1.DeviceSelector{
						{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].model == "H100"`}},
					},
				},
			},
			&resourcev1.DeviceClass{
				ObjectMeta: metav1.ObjectMeta{Name: "empty-class"},
				Spec:       resourcev1.DeviceClassSpec{},
			},
		)

		// Build pools with some devices.
		reqs := scheduling.NewRequirements()
		slices := []dynamicresources.ResourceSlice{
			makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(),
				withGeneration(1, 1), withAPIDevices("gpu-0", "gpu-1", "gpu-2")),
		}
		pools = dynamicresources.GatherPools(slices, reqs, "")
	})

	// makeTemplateDevices builds a map of instance type ID to template devices for use
	// in All-mode tests. Each entry contains the specified number of devices.
	makeTemplateDevices := func(entries map[string]int) map[dynamicresources.InstanceTypeID][]dynamicresources.DeviceWithID {
		result := make(map[dynamicresources.InstanceTypeID][]dynamicresources.DeviceWithID, len(entries))
		for itName, count := range entries {
			itID := unique.Make(itName)
			devices := make([]dynamicresources.DeviceWithID, count)
			for i := range count {
				name := fmt.Sprintf("%s-dev-%d", itName, i)
				devices[i] = dynamicresources.DeviceWithID{
					Device: cloudprovider.Device{Name: unique.Make(name)},
					ID:     deviceID("gpu.example.com", "pool-tmpl", name),
				}
			}
			result[itID] = devices
		}
		return result
	}

	Describe("ValidateClaimRequest", func() {
		It("should validate a simple ExactCount request", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "gpu-req",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           2,
								},
							},
						},
					},
				},
			}

			data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(data.Requests).To(HaveLen(1))
			Expect(data.Requests[0].Name).To(Equal("gpu-req"))
			Expect(data.Requests[0].NumDevices).To(Equal(2))
			Expect(data.Requests[0].AllocationMode).To(Equal(resourcev1.DeviceAllocationModeExactCount))
			Expect(data.Requests[0].CapacityRequests).To(BeNil())
		})

		It("should populate CapacityRequests when Capacity is set", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "gpu-req",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           1,
									Capacity: &resourcev1.CapacityRequirements{
										Requests: map[resourcev1.QualifiedName]resource.Quantity{
											"gpu.example.com/memory": resource.MustParse("16Gi"),
										},
									},
								},
							},
						},
					},
				},
			}

			data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(data.Requests).To(HaveLen(1))
			Expect(data.Requests[0].CapacityRequests).To(HaveLen(1))
			Expect(data.Requests[0].CapacityRequests["gpu.example.com/memory"]).To(Equal(resource.MustParse("16Gi")))
		})

		It("should validate multiple requests in a single claim", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req-a",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           2,
								},
							},
							{
								Name: "req-b",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "gpu",
									Count:           1,
								},
							},
						},
					},
				},
			}

			data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(data.Requests).To(HaveLen(2))
			Expect(data.Requests[0].Name).To(Equal("req-a"))
			Expect(data.Requests[0].NumDevices).To(Equal(2))
			Expect(data.Requests[1].Name).To(Equal("req-b"))
			Expect(data.Requests[1].NumDevices).To(Equal(1))
			Expect(data.Requests[1].Selectors).To(HaveLen(1))
		})

		It("should validate an empty claim with no requests", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{},
				},
			}

			data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(data.Requests).To(BeEmpty())
			Expect(data.Constraints).To(BeEmpty())
		})

		It("should fail when DeviceClass is not found", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "nonexistent",
									Count:           1,
								},
							},
						},
					},
				},
			}

			_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		It("should fail when a request uses FirstAvailable", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{Name: "req"},
						},
					},
				},
			}

			_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("only Exactly requests"))
		})

		It("should fail when an All mode request encounters an invalid pool", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									AllocationMode:  resourcev1.DeviceAllocationModeAll,
								},
							},
						},
					},
				},
			}
			invalidPools := []*dynamicresources.Pool{{
				Key:     dynamicresources.PoolKey{Driver: pools[0].Key.Driver, Pool: pools[0].Key.Pool},
				Invalid: true,
			}}

			_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, invalidPools, nil, celCache, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid"))
		})

		It("should fail when an All mode request encounters an incomplete pool", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									AllocationMode:  resourcev1.DeviceAllocationModeAll,
								},
							},
						},
					},
				},
			}
			incompletePools := []*dynamicresources.Pool{{
				Key:        dynamicresources.PoolKey{Driver: pools[0].Key.Driver, Pool: pools[0].Key.Pool},
				Incomplete: true,
			}}

			_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, incompletePools, nil, celCache, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("incomplete"))
		})

		It("should combine selectors from class and request", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "gpu-req",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "gpu",
									Count:           1,
									Selectors: []resourcev1.DeviceSelector{
										{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].memory > 40`}},
									},
								},
							},
						},
					},
				},
			}

			data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).ToNot(HaveOccurred())
			// 1 from class + 1 from request
			Expect(data.Requests[0].Selectors).To(HaveLen(2))
		})

		It("should fail when a request has a non-CEL selector", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           1,
									Selectors: []resourcev1.DeviceSelector{
										{}, // non-CEL
									},
								},
							},
						},
					},
				},
			}

			_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported selector type"))
		})

		It("should build MatchAttribute constraints from the claim", func() {
			attr := "topology.kubernetes.io/zone"
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req-a",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           1,
								},
							},
						},
						Constraints: []resourcev1.DeviceConstraint{
							{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName(attr))},
						},
					},
				},
			}

			data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(data.Constraints).To(HaveLen(1))
		})

		It("should build MatchAttribute constraints scoped to specific requests", func() {
			attr := "topology.kubernetes.io/zone"
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req-a",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           1,
								},
							},
							{
								Name: "req-b",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           1,
								},
							},
						},
						Constraints: []resourcev1.DeviceConstraint{
							{
								Requests:       []string{"req-a"},
								MatchAttribute: ptr.To(resourcev1.FullyQualifiedName(attr)),
							},
						},
					},
				},
			}

			data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(data.Constraints).To(HaveLen(1))
			mac, ok := data.Constraints[0].(*dynamicresources.MatchAttributeConstraint)
			Expect(ok).To(BeTrue())
			Expect(mac.RequestNames.Has("req-a")).To(BeTrue())
			Expect(mac.RequestNames.Has("req-b")).To(BeFalse())
		})

		It("should build multiple MatchAttribute constraints", func() {
			attrZone := "topology.kubernetes.io/zone"
			attrRack := "topology.kubernetes.io/rack"
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req-a",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           1,
								},
							},
						},
						Constraints: []resourcev1.DeviceConstraint{
							{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName(attrZone))},
							{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName(attrRack))},
						},
					},
				},
			}

			data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(data.Constraints).To(HaveLen(2))
			attrs := make([]resourcev1.QualifiedName, len(data.Constraints))
			for i, c := range data.Constraints {
				mac, ok := c.(*dynamicresources.MatchAttributeConstraint)
				Expect(ok).To(BeTrue())
				attrs[i] = mac.AttributeName
			}
			Expect(attrs).To(ConsistOf(
				resourcev1.QualifiedName(attrZone),
				resourcev1.QualifiedName(attrRack),
			))
		})

		It("should pass bindingFallback to MatchAttribute constraints", func() {
			attr := "topology.kubernetes.io/zone"
			fallback := &dynamicresources.AttributeBindingFallback{
				NodePool: "default",
			}
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req-a",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           1,
								},
							},
						},
						Constraints: []resourcev1.DeviceConstraint{
							{MatchAttribute: ptr.To(resourcev1.FullyQualifiedName(attr))},
						},
					},
				},
			}

			data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, fallback)
			Expect(err).ToNot(HaveOccurred())
			Expect(data.Constraints).To(HaveLen(1))
			mac, ok := data.Constraints[0].(*dynamicresources.MatchAttributeConstraint)
			Expect(ok).To(BeTrue())
			Expect(mac.AttributeBindingFallback).ToNot(BeNil())
			Expect(mac.AttributeBindingFallback.NodePool).To(Equal("default"))
		})

		It("should fail on DistinctAttribute constraints (not yet supported)", func() {
			attr := resourcev1.FullyQualifiedName("gpu.example.com/numa-node")
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Constraints: []resourcev1.DeviceConstraint{
							{DistinctAttribute: &attr},
						},
					},
				},
			}

			_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("DistinctAttribute"))
			Expect(err.Error()).To(ContainSubstring("not done yet"))
		})

		It("should fail on unsupported constraint types", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Constraints: []resourcev1.DeviceConstraint{
							{}, // no MatchAttribute set — unsupported
						},
					},
				},
			}

			_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported constraint type"))
		})

		It("should succeed when total devices across multiple requests equal AllocationResultsMaxSize", func() {
			half := int64(resourcev1.AllocationResultsMaxSize) / 2
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req-a",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           half,
								},
							},
							{
								Name: "req-b",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           int64(resourcev1.AllocationResultsMaxSize) - half,
								},
							},
						},
					},
				},
			}

			_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should fail when total devices across multiple requests exceed AllocationResultsMaxSize", func() {
			half := int64(resourcev1.AllocationResultsMaxSize) / 2
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req-a",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           half,
								},
							},
							{
								Name: "req-b",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           int64(resourcev1.AllocationResultsMaxSize) - half + 1,
								},
							},
						},
					},
				},
			}

			_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exceeding maximum"))
		})

		It("should fail when a CEL expression in a request does not compile", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "req",
								Exactly: &resourcev1.ExactDeviceRequest{
									DeviceClassName: "empty-class",
									Count:           1,
									Selectors: []resourcev1.DeviceSelector{
										{CEL: &resourcev1.CELDeviceSelector{Expression: `this is not valid CEL`}},
									},
								},
							},
						},
					},
				},
			}

			_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to compile"))
		})

		Context("All mode", func() {
			It("should validate an All mode request with valid pools", func() {
				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(data.Requests).To(HaveLen(1))
				Expect(data.Requests[0].AllocationMode).To(Equal(resourcev1.DeviceAllocationModeAll))
				Expect(data.Requests[0].NumDevices).To(Equal(0))
				// 3 devices in the pool (gpu-0, gpu-1, gpu-2) match empty-class (no selectors)
				Expect(data.Requests[0].AllDevices).To(HaveLen(3))
			})

			It("should populate AllTemplateDevicesByIT for All mode with template devices", func() {
				templateDevices := makeTemplateDevices(map[string]int{
					"c5.large":  5,
					"c5.xlarge": 10,
				})
				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				// Use empty pools so in-cluster devices don't interfere.
				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, nil, templateDevices, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(data.Requests[0].AllTemplateDevicesByIT).To(HaveLen(2))
			})

			It("should filter All mode devices by selectors", func() {
				// Build a pool with devices that have attributes — only some will match.
				reqs := scheduling.NewRequirements()
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-gpu", "gpu.example.com", "pool-gpu", withAllNodes(), withGeneration(1, 1),
						withAPIDevicesWithAttrs(
							deviceWithAttrs("h100-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								"gpu.example.com/model": {StringValue: ptr.To("H100")},
							}),
							deviceWithAttrs("a100-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								"gpu.example.com/model": {StringValue: ptr.To("A100")},
							}),
						)),
				}
				gpuPools := dynamicresources.GatherPools(slices, reqs, "")

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "gpu", // selects model == "H100"
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, gpuPools, nil, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				// Only the H100 should match.
				Expect(data.Requests[0].AllDevices).To(HaveLen(1))
			})

			It("should exclude instance types with no matching template devices from AllTemplateDevicesByIT", func() {
				// c5.large has H100s (matches), c5.xlarge has A100s (does not match)
				c5Large := unique.Make("c5.large")
				c5XLarge := unique.Make("c5.xlarge")
				templateDevices := map[dynamicresources.InstanceTypeID][]dynamicresources.DeviceWithID{
					c5Large: {
						{
							Device: cloudprovider.Device{
								Name: unique.Make("h100-0"),
								Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
									"gpu.example.com/model": {StringValue: ptr.To("H100")},
								},
							},
							ID: deviceID("gpu.example.com", "pool-tmpl", "h100-0"),
						},
					},
					c5XLarge: {
						{
							Device: cloudprovider.Device{
								Name: unique.Make("a100-0"),
								Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
									"gpu.example.com/model": {StringValue: ptr.To("A100")},
								},
							},
							ID: deviceID("gpu.example.com", "pool-tmpl", "a100-0"),
						},
					},
				}

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "gpu", // selects model == "H100"
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, nil, templateDevices, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(data.Requests[0].AllTemplateDevicesByIT).To(HaveKey(c5Large))
				Expect(data.Requests[0].AllTemplateDevicesByIT).ToNot(HaveKey(c5XLarge))
			})

			It("should keep instance types with no matching template devices when in-cluster devices exist", func() {
				c5Large := unique.Make("c5.large")
				c5XLarge := unique.Make("c5.xlarge")
				templateDevices := map[dynamicresources.InstanceTypeID][]dynamicresources.DeviceWithID{
					c5Large: {
						{
							Device: cloudprovider.Device{
								Name: unique.Make("h100-0"),
								Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
									"gpu.example.com/model": {StringValue: ptr.To("H100")},
								},
							},
							ID: deviceID("gpu.example.com", "pool-tmpl", "h100-0"),
						},
					},
					c5XLarge: {
						{
							Device: cloudprovider.Device{
								Name: unique.Make("a100-0"),
								Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
									"gpu.example.com/model": {StringValue: ptr.To("A100")},
								},
							},
							ID: deviceID("gpu.example.com", "pool-tmpl", "a100-0"),
						},
					},
				}

				// Build in-cluster pool with an H100 device.
				reqs := scheduling.NewRequirements()
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-h100", "gpu.example.com", "pool-h100", withAllNodes(), withGeneration(1, 1),
						withAPIDevicesWithAttrs(
							deviceWithAttrs("h100-cluster", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								"gpu.example.com/model": {StringValue: ptr.To("H100")},
							}),
						)),
				}
				inClusterPools := dynamicresources.GatherPools(slices, reqs, "")

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "gpu", // selects model == "H100"
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, inClusterPools, templateDevices, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				// c5.large has a matching H100 template device.
				Expect(data.Requests[0].AllTemplateDevicesByIT).To(HaveKey(c5Large))
				Expect(data.Requests[0].AllTemplateDevicesByIT[c5Large]).To(HaveLen(1))
				// c5.xlarge has no matching template devices, but in-cluster devices exist
				// so the IT should be kept with an empty device list.
				Expect(data.Requests[0].AllTemplateDevicesByIT).To(HaveKey(c5XLarge))
				Expect(data.Requests[0].AllTemplateDevicesByIT[c5XLarge]).To(BeEmpty())
			})

			It("should keep all instance types with empty device lists when in-cluster devices match but no template devices match", func() {
				c5Large := unique.Make("c5.large")
				c5XLarge := unique.Make("c5.xlarge")
				templateDevices := map[dynamicresources.InstanceTypeID][]dynamicresources.DeviceWithID{
					c5Large: {
						{
							Device: cloudprovider.Device{
								Name: unique.Make("a100-0"),
								Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
									"gpu.example.com/model": {StringValue: ptr.To("A100")},
								},
							},
							ID: deviceID("gpu.example.com", "pool-tmpl", "a100-0"),
						},
					},
					c5XLarge: {
						{
							Device: cloudprovider.Device{
								Name: unique.Make("a100-1"),
								Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
									"gpu.example.com/model": {StringValue: ptr.To("A100")},
								},
							},
							ID: deviceID("gpu.example.com", "pool-tmpl", "a100-1"),
						},
					},
				}

				// Build in-cluster pool with an H100 device that matches the "gpu" class selector.
				reqs := scheduling.NewRequirements()
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-h100", "gpu.example.com", "pool-h100", withAllNodes(), withGeneration(1, 1),
						withAPIDevicesWithAttrs(
							deviceWithAttrs("h100-cluster", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								"gpu.example.com/model": {StringValue: ptr.To("H100")},
							}),
						)),
				}
				inClusterPools := dynamicresources.GatherPools(slices, reqs, "")

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "gpu", // selects model == "H100"
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, inClusterPools, templateDevices, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				// In-cluster H100 should match.
				Expect(data.Requests[0].AllDevices).To(HaveLen(1))
				// No template devices match (all are A100s), but in-cluster devices exist
				// so all instance types should be kept with empty device lists.
				Expect(data.Requests[0].AllTemplateDevicesByIT).To(HaveLen(2))
				Expect(data.Requests[0].AllTemplateDevicesByIT).To(HaveKey(c5Large))
				Expect(data.Requests[0].AllTemplateDevicesByIT[c5Large]).To(BeEmpty())
				Expect(data.Requests[0].AllTemplateDevicesByIT).To(HaveKey(c5XLarge))
				Expect(data.Requests[0].AllTemplateDevicesByIT[c5XLarge]).To(BeEmpty())
			})

			It("should aggregate devices across multiple pools in All mode", func() {
				reqs := scheduling.NewRequirements()
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withGeneration(1, 1),
						withAPIDevices("gpu-0", "gpu-1")),
					makeAPISlice("s2", "nic.example.com", "pool-b", withAllNodes(), withGeneration(1, 1),
						withAPIDevices("nic-0")),
				}
				multiPools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(multiPools).To(HaveLen(2))

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, multiPools, nil, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				// 2 from pool-a + 1 from pool-b = 3
				Expect(data.Requests[0].AllDevices).To(HaveLen(3))
			})

			It("should fail when any pool is invalid in All mode even if others are valid", func() {
				mixedPools := []*dynamicresources.Pool{
					pools[0], // valid
					{
						Key:     dynamicresources.PoolKey{Driver: unique.Make("nic.example.com"), Pool: unique.Make("pool-b")},
						Invalid: true,
					},
				}
				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, mixedPools, nil, celCache, nil)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("invalid"))
			})

			It("should return empty AllDevices when no pool devices match selectors in All mode", func() {
				reqs := scheduling.NewRequirements()
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-a100", "gpu.example.com", "pool-a100", withAllNodes(), withGeneration(1, 1),
						withAPIDevicesWithAttrs(
							deviceWithAttrs("a100-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								"gpu.example.com/model": {StringValue: ptr.To("A100")},
							}),
							deviceWithAttrs("a100-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								"gpu.example.com/model": {StringValue: ptr.To("A100")},
							}),
						)),
				}
				a100Pools := dynamicresources.GatherPools(slices, reqs, "")

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "gpu", // selects model == "H100"
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, a100Pools, nil, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(data.Requests[0].AllDevices).To(BeEmpty())
			})

			It("should validate All mode with empty pools", func() {
				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, nil, nil, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(data.Requests[0].AllDevices).To(BeEmpty())
			})

			It("should fail when All mode in-cluster devices exceed AllocationResultsMaxSize", func() {
				// Build a pool with more than AllocationResultsMaxSize devices.
				deviceNames := make([]string, int(resourcev1.AllocationResultsMaxSize)+1)
				for i := range deviceNames {
					deviceNames[i] = fmt.Sprintf("dev-%d", i)
				}
				reqs := scheduling.NewRequirements()
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-big", "gpu.example.com", "pool-big", withAllNodes(), withGeneration(1, 1),
						withAPIDevices(deviceNames...)),
				}
				bigPools := dynamicresources.GatherPools(slices, reqs, "")

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, bigPools, nil, celCache, nil)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("exceeding maximum"))
			})

			It("should fail when All mode in-cluster devices plus ExactCount exceed AllocationResultsMaxSize", func() {
				// 3 in-cluster devices from the default pool + ExactCount of 30 = 33 > 32.
				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "all-req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
								{
									Name: "exact-req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										Count:           30,
									},
								},
							},
						},
					},
				}

				_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, nil, celCache, nil)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("exceeding maximum"))
			})

			It("should prune instance types whose template devices would exceed AllocationResultsMaxSize", func() {
				// 20 in-cluster devices + c5.large(10)=30 ok, c5.xlarge(20)=40 pruned.
				deviceNames := make([]string, 20)
				for i := range deviceNames {
					deviceNames[i] = fmt.Sprintf("dev-%d", i)
				}
				reqs := scheduling.NewRequirements()
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-20", "gpu.example.com", "pool-20", withAllNodes(), withGeneration(1, 1),
						withAPIDevices(deviceNames...)),
				}
				largePools := dynamicresources.GatherPools(slices, reqs, "")

				templateDevices := makeTemplateDevices(map[string]int{
					"c5.large":  10, // 20 + 10 = 30 <= 32
					"c5.xlarge": 20, // 20 + 20 = 40 > 32
				})

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, largePools, templateDevices, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(data.Requests[0].AllDevices).To(HaveLen(20))
				// c5.large should remain, c5.xlarge should be pruned.
				Expect(data.Requests[0].AllTemplateDevicesByIT).To(HaveKey(unique.Make("c5.large")))
				Expect(data.Requests[0].AllTemplateDevicesByIT).ToNot(HaveKey(unique.Make("c5.xlarge")))
			})

			It("should account for ExactCount devices in base total when pruning template devices", func() {
				// 10 ExactCount + 15 All in-cluster = 25 base; template IT with 10 = 35 > 32, should be pruned.
				// Template IT with 5 = 30 <= 32, should remain.
				deviceNames := make([]string, 15)
				for i := range deviceNames {
					deviceNames[i] = fmt.Sprintf("dev-%d", i)
				}
				reqs := scheduling.NewRequirements()
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-15", "gpu.example.com", "pool-15", withAllNodes(), withGeneration(1, 1),
						withAPIDevices(deviceNames...)),
				}
				mixedPools := dynamicresources.GatherPools(slices, reqs, "")

				templateDevices := makeTemplateDevices(map[string]int{
					"c5.large":  5,  // 25 + 5 = 30 <= 32
					"c5.xlarge": 10, // 25 + 10 = 35 > 32
				})

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "all-req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
								{
									Name: "exact-req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										Count:           10,
									},
								},
							},
						},
					},
				}

				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, mixedPools, templateDevices, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(data.Requests[0].AllDevices).To(HaveLen(15))
				Expect(data.Requests[0].AllTemplateDevicesByIT).To(HaveKey(unique.Make("c5.large")))
				Expect(data.Requests[0].AllTemplateDevicesByIT).ToNot(HaveKey(unique.Make("c5.xlarge")))
			})

			It("should fail when all instance types are pruned", func() {
				// 30 in-cluster devices + any template devices > 32.
				deviceNames := make([]string, 30)
				for i := range deviceNames {
					deviceNames[i] = fmt.Sprintf("dev-%d", i)
				}
				reqs := scheduling.NewRequirements()
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-30", "gpu.example.com", "pool-30", withAllNodes(), withGeneration(1, 1),
						withAPIDevices(deviceNames...)),
				}
				largePools := dynamicresources.GatherPools(slices, reqs, "")

				templateDevices := makeTemplateDevices(map[string]int{
					"c5.large":  5, // 30 + 5 = 35 > 32
					"c5.xlarge": 5, // 30 + 5 = 35 > 32
				})

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										AllocationMode:  resourcev1.DeviceAllocationModeAll,
									},
								},
							},
						},
					},
				}

				_, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, largePools, templateDevices, celCache, nil)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("all instance types pruned"))
			})

			It("should not fail when template devices exist but no All mode requests reference them", func() {
				templateDevices := makeTemplateDevices(map[string]int{
					"c5.large": 5,
				})

				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "test-claim"},
					Spec: resourcev1.ResourceClaimSpec{
						Devices: resourcev1.DeviceClaim{
							Requests: []resourcev1.DeviceRequest{
								{
									Name: "req",
									Exactly: &resourcev1.ExactDeviceRequest{
										DeviceClassName: "empty-class",
										Count:           1,
									},
								},
							},
						},
					},
				}

				// ExactCount only — templateDevices are present but allITs is empty, no pruning occurs.
				data, err := dynamicresources.ValidateClaimRequest(ctx, env.Client, claim, pools, templateDevices, celCache, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(data.Requests[0].AllocationMode).To(Equal(resourcev1.DeviceAllocationModeExactCount))
			})
		})
	})

	Describe("DeviceMatchesSelectors", func() {
		It("should match a device with matching attributes", func() {
			d := cloudprovider.Device{
				Name: unique.Make("gpu-0"),
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					"gpu.example.com/model": {StringValue: ptr.To("H100")},
				},
			}
			selectors := []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].model == "H100"`}},
			}

			match, err := dynamicresources.DeviceMatchesSelectors(ctx, d,
				deviceID("gpu.example.com", "pool", "gpu-0"), selectors, celCache)
			Expect(err).ToNot(HaveOccurred())
			Expect(match).To(BeTrue())
		})

		It("should not match a device with non-matching attributes", func() {
			d := cloudprovider.Device{
				Name: unique.Make("gpu-0"),
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					"gpu.example.com/model": {StringValue: ptr.To("A100")},
				},
			}
			selectors := []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].model == "H100"`}},
			}

			match, err := dynamicresources.DeviceMatchesSelectors(ctx, d,
				deviceID("gpu.example.com", "pool", "gpu-0"), selectors, celCache)
			Expect(err).ToNot(HaveOccurred())
			Expect(match).To(BeFalse())
		})

		It("should match when all selectors pass (AND semantics)", func() {
			d := cloudprovider.Device{
				Name: unique.Make("gpu-0"),
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					"gpu.example.com/model":  {StringValue: ptr.To("H100")},
					"gpu.example.com/memory": {IntValue: ptr.To(int64(80))},
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"gpu.example.com/vram": {Value: resource.MustParse("80Gi")},
				},
				AllowMultipleAllocations: true,
			}
			selectors := []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].model == "H100"`}},
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].memory > 40`}},
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.capacity["gpu.example.com"].vram.isGreaterThan(quantity("40Gi"))`}},
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.allowMultipleAllocations == true`}},
			}

			match, err := dynamicresources.DeviceMatchesSelectors(ctx, d,
				deviceID("gpu.example.com", "pool", "gpu-0"), selectors, celCache)
			Expect(err).ToNot(HaveOccurred())
			Expect(match).To(BeTrue())
		})

		It("should not match when any selector fails (AND semantics)", func() {
			d := cloudprovider.Device{
				Name: unique.Make("gpu-0"),
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					"gpu.example.com/model":  {StringValue: ptr.To("H100")},
					"gpu.example.com/memory": {IntValue: ptr.To(int64(20))},
				},
			}
			selectors := []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].model == "H100"`}},
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.attributes["gpu.example.com"].memory > 40`}},
			}

			match, err := dynamicresources.DeviceMatchesSelectors(ctx, d,
				deviceID("gpu.example.com", "pool", "gpu-0"), selectors, celCache)
			Expect(err).ToNot(HaveOccurred())
			Expect(match).To(BeFalse())
		})

		It("should match with empty selectors", func() {
			d := cloudprovider.Device{Name: unique.Make("gpu-0")}
			match, err := dynamicresources.DeviceMatchesSelectors(ctx, d,
				deviceID("gpu.example.com", "pool", "gpu-0"), nil, celCache)
			Expect(err).ToNot(HaveOccurred())
			Expect(match).To(BeTrue())
		})

		It("should skip non-CEL selectors", func() {
			d := cloudprovider.Device{Name: unique.Make("gpu-0")}
			selectors := []resourcev1.DeviceSelector{
				{}, // non-CEL selector, should be skipped
			}

			match, err := dynamicresources.DeviceMatchesSelectors(ctx, d,
				deviceID("gpu.example.com", "pool", "gpu-0"), selectors, celCache)
			Expect(err).ToNot(HaveOccurred())
			Expect(match).To(BeTrue())
		})

		It("should return error when CEL expression fails to compile in DeviceMatchesSelectors", func() {
			d := cloudprovider.Device{Name: unique.Make("gpu-0")}
			selectors := []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: `this is not valid CEL`}},
			}

			_, err := dynamicresources.DeviceMatchesSelectors(ctx, d,
				deviceID("gpu.example.com", "pool", "gpu-0"), selectors, celCache)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to compile"))
		})

		It("should not match when capacity selector fails", func() {
			d := cloudprovider.Device{
				Name: unique.Make("gpu-0"),
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"gpu.example.com/memory": {Value: resource.MustParse("20Gi")},
				},
			}
			selectors := []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.capacity["gpu.example.com"].memory.isGreaterThan(quantity("40Gi"))`}},
			}

			match, err := dynamicresources.DeviceMatchesSelectors(ctx, d,
				deviceID("gpu.example.com", "pool", "gpu-0"), selectors, celCache)
			Expect(err).ToNot(HaveOccurred())
			Expect(match).To(BeFalse())
		})

		It("should not match when AllowMultipleAllocations is false", func() {
			d := cloudprovider.Device{
				Name:                     unique.Make("exclusive-gpu"),
				AllowMultipleAllocations: false,
			}
			selectors := []resourcev1.DeviceSelector{
				{CEL: &resourcev1.CELDeviceSelector{Expression: `device.allowMultipleAllocations == true`}},
			}

			match, err := dynamicresources.DeviceMatchesSelectors(ctx, d,
				deviceID("gpu.example.com", "pool", "exclusive-gpu"), selectors, celCache)
			Expect(err).ToNot(HaveOccurred())
			Expect(match).To(BeFalse())
		})
	})
})
