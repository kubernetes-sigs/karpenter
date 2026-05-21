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
	"k8s.io/utils/ptr"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
)

func makeAPISlice(name, driver, pool string, opts ...func(*resourcev1.ResourceSlice)) dynamicresources.ResourceSlice {
	s := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver: driver,
			Pool:   resourcev1.ResourcePool{Name: pool},
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return dynamicresources.NewAPIServerSlice(s)
}

func withAllNodes() func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.Spec.AllNodes = ptr.To(true)
	}
}

func withNodeSelector(key string, values ...string) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.Spec.NodeSelector = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: key, Operator: corev1.NodeSelectorOpIn, Values: values},
				}},
			},
		}
	}
}

func withRawNodeSelector(ns *corev1.NodeSelector) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.Spec.NodeSelector = ns
	}
}

func withAPIDevices(names ...string) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		for _, name := range names {
			s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{Name: name})
		}
	}
}

type apiDeviceSpec struct {
	name  string
	attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute
}

func deviceWithAttrs(name string, attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute) apiDeviceSpec {
	return apiDeviceSpec{name: name, attrs: attrs}
}

func withAPIDevicesWithAttrs(specs ...apiDeviceSpec) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		for _, spec := range specs {
			s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
				Name:       spec.name,
				Attributes: spec.attrs,
			})
		}
	}
}

func withGeneration(gen int64, sliceCount int64) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.Spec.Pool.Generation = gen
		s.Spec.Pool.ResourceSliceCount = sliceCount
	}
}

func makePotentialSlice(driver, pool string) dynamicresources.ResourceSlice {
	return dynamicresources.NewTemplateSlice(&cloudprovider.ResourceSliceTemplate{
		Driver:  unique.Make(driver),
		Pool:    cloudprovider.ResourcePool{Name: unique.Make(pool)},
		Devices: []cloudprovider.Device{{Name: unique.Make("dev-0")}},
	})
}

var _ = Describe("Pool Gathering", func() {
	var reqs scheduling.Requirements

	BeforeEach(func() {
		reqs = scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
		)
	})

	Describe("GatherPools", func() {
		It("should build a pool from an AllNodes slice", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0", "gpu-1")),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(HaveLen(1))
			Expect(pools[0].Key.Driver).To(Equal(unique.Make("gpu.example.com")))
			Expect(pools[0].Key.Pool).To(Equal(unique.Make("pool-a")))
			Expect(pools[0].Devices).To(HaveLen(2))
		})

		It("should build a pool from a nodeSelector slice matching requirements", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(HaveLen(1))
		})

		It("should exclude pools with only incompatible nodeSelector slices", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "eu-west-1a"),
					withAPIDevices("gpu-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(BeEmpty())
		})

		It("should exclude slices with no node affinity fields", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAPIDevices("gpu-0")),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(BeEmpty())
		})

		It("should return an empty list when no slices are provided", func() {
			pools := dynamicresources.GatherPools(nil, reqs)
			Expect(pools).To(BeEmpty())
		})

		It("should match a slice when any NodeSelectorTerm is compatible", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withRawNodeSelector(&corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							// First term: incompatible zone
							{MatchExpressions: []corev1.NodeSelectorRequirement{
								{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"eu-west-1a"}},
							}},
							// Second term: compatible zone
							{MatchExpressions: []corev1.NodeSelectorRequirement{
								{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2a"}},
							}},
						},
					}),
					withAPIDevices("gpu-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(HaveLen(1))
		})

		It("should reject a slice when one match expression within a term is incompatible", func() {
			// Requirements constrain zone to {us-west-2a, us-west-2b}. The term ANDs
			// two expressions on the same key with non-overlapping values, so the
			// intersection is empty and the term should fail.
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withRawNodeSelector(&corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{MatchExpressions: []corev1.NodeSelectorRequirement{
								{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"us-west-2a"}},
								{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"us-west-2a"}},
							}},
						},
					}),
					withAPIDevices("gpu-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(BeEmpty())
		})

		It("should group multiple slices into the same pool by driver+pool", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1")),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(HaveLen(1))
			Expect(pools[0].Devices).To(HaveLen(2))
			Expect(pools[0].Slices).To(HaveLen(2))
		})

		It("should create separate pools for different pool names", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				makeAPISlice("s2", "gpu.example.com", "pool-b", withAllNodes(), withAPIDevices("gpu-1")),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(HaveLen(2))
		})

		It("should create separate pools for different drivers", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				makeAPISlice("s2", "nic.example.com", "pool-a", withAllNodes(), withAPIDevices("nic-0")),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(HaveLen(2))
		})

		Context("validation", func() {
			It("should mark a pool as invalid when it has duplicate device names", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
					makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")), // duplicate
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeTrue())
			})

			It("should panic when a potential slice is provided", func() {
				slices := []dynamicresources.ResourceSlice{
					makePotentialSlice("gpu.example.com", "pool-a"),
				}
				Expect(func() {
					dynamicresources.GatherPools(slices, reqs)
				}).To(PanicWith("potential slices must not be passed to pool gathering or filtering"))
			})

			It("should mark a pool as invalid when a single slice has duplicate device names", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0", "gpu-0")),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeTrue())
			})

			It("should not mark a pool as invalid when device names are unique", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
					makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1")),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeFalse())
			})
		})

		Context("device IDs", func() {
			It("should construct correct DeviceIDs", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices[0].ID.Driver).To(Equal(unique.Make("gpu.example.com")))
				Expect(pools[0].Devices[0].ID.Pool).To(Equal(unique.Make("pool-a")))
				Expect(pools[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-0")))
			})

			It("should construct correct DeviceIDs for multiple devices in a single slice", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0", "gpu-1", "gpu-2")),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(3))
				for i, name := range []string{"gpu-0", "gpu-1", "gpu-2"} {
					Expect(pools[0].Devices[i].ID.Driver).To(Equal(unique.Make("gpu.example.com")))
					Expect(pools[0].Devices[i].ID.Pool).To(Equal(unique.Make("pool-a")))
					Expect(pools[0].Devices[i].ID.Device).To(Equal(unique.Make(name)))
				}
			})

			It("should construct correct DeviceIDs across multiple slices and pools", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 2)),
					makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1"), withGeneration(1, 2)),
					makeAPISlice("s3", "nic.example.com", "pool-b", withAllNodes(), withAPIDevices("nic-0"), withGeneration(1, 1)),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(2))

				// Build a map by pool key for order-independent assertions.
				byPool := map[string]*dynamicresources.Pool{}
				for _, p := range pools {
					byPool[p.Key.Pool.Value()] = p
				}

				// pool-a: 2 slices, 2 devices from gpu.example.com
				poolA := byPool["pool-a"]
				Expect(poolA).ToNot(BeNil())
				Expect(poolA.Devices).To(HaveLen(2))
				for _, d := range poolA.Devices {
					Expect(d.ID.Driver).To(Equal(unique.Make("gpu.example.com")))
					Expect(d.ID.Pool).To(Equal(unique.Make("pool-a")))
				}
				deviceNames := []string{poolA.Devices[0].ID.Device.Value(), poolA.Devices[1].ID.Device.Value()}
				Expect(deviceNames).To(ConsistOf("gpu-0", "gpu-1"))

				// pool-b: 1 slice, 1 device from nic.example.com
				poolB := byPool["pool-b"]
				Expect(poolB).ToNot(BeNil())
				Expect(poolB.Devices).To(HaveLen(1))
				Expect(poolB.Devices[0].ID.Driver).To(Equal(unique.Make("nic.example.com")))
				Expect(poolB.Devices[0].ID.Pool).To(Equal(unique.Make("pool-b")))
				Expect(poolB.Devices[0].ID.Device).To(Equal(unique.Make("nic-0")))
			})
		})

		Context("generation handling", func() {
			It("should discard slices from an older generation", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-old", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-old"), withGeneration(1, 1)),
					makeAPISlice("s-new", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-new"), withGeneration(2, 1)),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-new")))
			})

			It("should replace all old-generation slices when a newer generation arrives", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-old-a", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 2)),
					makeAPISlice("s-old-b", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1"), withGeneration(1, 2)),
					makeAPISlice("s-new", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-new"), withGeneration(2, 1)),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-new")))
			})

			It("should return new generation as incomplete when previous generation was complete", func() {
				slices := []dynamicresources.ResourceSlice{
					// Gen 1: complete (1 of 1)
					makeAPISlice("s-old", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 1)),
					// Gen 2: incomplete (1 of 2)
					makeAPISlice("s-new", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-new"), withGeneration(2, 2)),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeTrue())
				// Should only contain gen 2 devices
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-new")))
			})

			It("should return new generation as invalid when previous generation was complete", func() {
				slices := []dynamicresources.ResourceSlice{
					// Gen 1: complete (1 of 1)
					makeAPISlice("s-old", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 1)),
					// Gen 2: has duplicate device names (invalid)
					makeAPISlice("s-new-a", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-dup"), withGeneration(2, 2)),
					makeAPISlice("s-new-b", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-dup"), withGeneration(2, 2)),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeTrue())
				// Should only contain gen 2 devices
				Expect(pools[0].Devices).To(HaveLen(2))
			})

			It("should return new generation as valid when previous generation was incomplete", func() {
				slices := []dynamicresources.ResourceSlice{
					// Gen 1: incomplete (1 of 2)
					makeAPISlice("s-old", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-old"), withGeneration(1, 2)),
					// Gen 2: complete (1 of 1)
					makeAPISlice("s-new", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-new"), withGeneration(2, 1)),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeFalse())
				Expect(pools[0].Invalid).To(BeFalse())
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-new")))
			})

			It("should return new generation as valid when previous generation was invalid", func() {
				slices := []dynamicresources.ResourceSlice{
					// Gen 1: invalid (duplicate names)
					makeAPISlice("s-old-a", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-dup"), withGeneration(1, 2)),
					makeAPISlice("s-old-b", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-dup"), withGeneration(1, 2)),
					// Gen 2: valid and complete
					makeAPISlice("s-new", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-new"), withGeneration(2, 1)),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeFalse())
				Expect(pools[0].Invalid).To(BeFalse())
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-new")))
			})

			It("should accumulate slices from the same generation", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 2)),
					makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1"), withGeneration(1, 2)),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(2))
			})
		})

		Context("completeness", func() {
			It("should mark a pool as incomplete when fewer slices than ResourceSliceCount", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 2)),
					// Only 1 slice but ResourceSliceCount=2
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeTrue())
			})

			It("should not mark a pool as incomplete when slice count matches", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 2)),
					makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1"), withGeneration(1, 2)),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeFalse())
			})

			It("should mark a pool as incomplete when more slices than ResourceSliceCount", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 1)),
					makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1"), withGeneration(1, 1)),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeTrue())
			})

			It("should mark a pool as incomplete when ResourceSliceCount is 0", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeTrue())
			})

			It("should not mark a pool as incomplete when non-matching slices complete the count", func() {
				// Pool has 2 slices: one matches our node, one targets a different node.
				// The pool should be considered complete because all slices exist globally.
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a",
						withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 2),
					),
					makeAPISlice("s2", "gpu.example.com", "pool-a",
						withNodeSelector(corev1.LabelTopologyZone, "eu-west-1a"), // non-matching
						withAPIDevices("gpu-1"),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeFalse())
				// Only the matching slice's devices should be visible.
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-0")))
				Expect(pools[0].Slices).To(HaveLen(1))
			})

			It("should drop a pool when a newer generation exists only on non-matching slices", func() {
				// Gen 1 matches our node, but gen 2 exists on a different node.
				// The gen 1 data is obsolete — the pool should be dropped.
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s-old", "gpu.example.com", "pool-a",
						withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 1),
					),
					makeAPISlice("s-new", "gpu.example.com", "pool-a",
						withNodeSelector(corev1.LabelTopologyZone, "eu-west-1a"), // non-matching
						withAPIDevices("gpu-new"),
						withGeneration(2, 1),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				// Gen 2 supersedes gen 1. Gen 2 has no matching slices, so pool is nil.
				Expect(pools).To(BeEmpty())
			})

			It("should include only matching slices when pool has mixed node selectors", func() {
				// Pool has 3 slices: 2 match, 1 doesn't. All same generation.
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a",
						withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 3),
					),
					makeAPISlice("s2", "gpu.example.com", "pool-a",
						withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
						withAPIDevices("gpu-1"),
						withGeneration(1, 3),
					),
					makeAPISlice("s3", "gpu.example.com", "pool-a",
						withNodeSelector(corev1.LabelTopologyZone, "eu-west-1a"),
						withAPIDevices("gpu-2"),
						withGeneration(1, 3),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs)
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeFalse())
				Expect(pools[0].Devices).To(HaveLen(2))
				Expect(pools[0].Slices).To(HaveLen(2))
			})
		})
	})

	Describe("FilterPools", func() {
		It("should return an empty list when no pools are provided", func() {
			filtered := dynamicresources.FilterPools(nil, reqs)
			Expect(filtered).To(BeEmpty())
		})

		It("should retain pools with compatible slices", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(HaveLen(1))

			tightened := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
			)
			filtered := dynamicresources.FilterPools(pools, tightened)
			Expect(filtered).To(HaveLen(1))
		})

		It("should exclude pools with incompatible slices after requirement tightening", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(HaveLen(1))

			tightened := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2b"),
			)
			filtered := dynamicresources.FilterPools(pools, tightened)
			Expect(filtered).To(BeEmpty())
		})

		It("should panic when a pool contains potential slices", func() {
			pool := &dynamicresources.Pool{
				Slices: []dynamicresources.ResourceSlice{
					makePotentialSlice("gpu.example.com", "pool-a"),
				},
			}
			Expect(func() {
				dynamicresources.FilterPools([]*dynamicresources.Pool{pool}, reqs)
			}).To(PanicWith("potential slices must not be passed to pool gathering or filtering"))
		})

		It("should always retain pools with AllNodes slices", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
			}
			pools := dynamicresources.GatherPools(slices, reqs)

			tightened := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "ap-southeast-1a"),
			)
			filtered := dynamicresources.FilterPools(pools, tightened)
			Expect(filtered).To(HaveLen(1))
		})

		It("should retain some pools and exclude others", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-0"),
				),
				makeAPISlice("s2", "nic.example.com", "pool-b",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("nic-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs)
			Expect(pools).To(HaveLen(2))

			tightened := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
			)
			filtered := dynamicresources.FilterPools(pools, tightened)
			Expect(filtered).To(HaveLen(1))
			Expect(filtered[0].Key.Pool).To(Equal(unique.Make("pool-a")))
		})

		It("should retain a pool but strip non-matching slices and their devices", func() {
			// Use broad initial requirements so both slices pass gathering.
			broad := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
			)
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2a"),
					withAPIDevices("gpu-0"),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-a",
					withNodeSelector(corev1.LabelTopologyZone, "us-west-2b"),
					withAPIDevices("gpu-1"),
				),
			}
			pools := dynamicresources.GatherPools(slices, broad)
			Expect(pools).To(HaveLen(1))
			Expect(pools[0].Slices).To(HaveLen(2))
			Expect(pools[0].Devices).To(HaveLen(2))

			// Tighten to only us-west-2a — the us-west-2b slice should be stripped.
			tightened := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
			)
			filtered := dynamicresources.FilterPools(pools, tightened)
			Expect(filtered).To(HaveLen(1))
			Expect(filtered[0].Slices).To(HaveLen(1))
			Expect(filtered[0].Devices).To(HaveLen(1))
			Expect(filtered[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-0")))
		})

		It("should exclude a pool when its slices have no node affinity fields", func() {
			pool := &dynamicresources.Pool{
				Slices: []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAPIDevices("gpu-0")),
				},
			}
			filtered := dynamicresources.FilterPools([]*dynamicresources.Pool{pool}, reqs)
			Expect(filtered).To(BeEmpty())
		})
	})
})
