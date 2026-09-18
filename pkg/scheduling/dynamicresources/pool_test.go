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

func withZoneSelector(values ...string) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.Spec.NodeSelector = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: values},
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

//nolint:unparam
func withNodeName(nodeName string) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.Spec.NodeName = &nodeName
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
	name             string
	attrs            map[resourcev1.QualifiedName]resourcev1.DeviceAttribute
	consumesCounters []resourcev1.DeviceCounterConsumption
}

func deviceWithAttrs(name string, attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute) apiDeviceSpec {
	return apiDeviceSpec{name: name, attrs: attrs}
}

func deviceWithAttrsAndCounters(name string, attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute, consumesCounters []resourcev1.DeviceCounterConsumption) apiDeviceSpec {
	return apiDeviceSpec{name: name, attrs: attrs, consumesCounters: consumesCounters}
}

func withAPIDevicesWithAttrs(specs ...apiDeviceSpec) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		for _, spec := range specs {
			s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{
				Name:             spec.name,
				Attributes:       spec.attrs,
				ConsumesCounters: spec.consumesCounters,
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

func withSharedCounters(counterSets ...resourcev1.CounterSet) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.Spec.SharedCounters = counterSets
	}
}

func withDevicesConsumingCounters(devices ...resourcev1.Device) func(*resourcev1.ResourceSlice) {
	return func(s *resourcev1.ResourceSlice) {
		s.Spec.Devices = append(s.Spec.Devices, devices...)
	}
}

func counterSet(name string, counters map[string]resource.Quantity) resourcev1.CounterSet {
	cs := resourcev1.CounterSet{
		Name:     name,
		Counters: make(map[string]resourcev1.Counter, len(counters)),
	}
	for k, v := range counters {
		cs.Counters[k] = resourcev1.Counter{Value: v}
	}
	return cs
}

func deviceConsumingCounter(name, counterSetName string, counters map[string]resource.Quantity) resourcev1.Device {
	rc := make(map[string]resourcev1.Counter, len(counters))
	for k, v := range counters {
		rc[k] = resourcev1.Counter{Value: v}
	}
	return resourcev1.Device{
		Name: name,
		ConsumesCounters: []resourcev1.DeviceCounterConsumption{
			{
				CounterSet: counterSetName,
				Counters:   rc,
			},
		},
	}
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
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(HaveLen(1))
			Expect(pools[0].Key.Driver).To(Equal(unique.Make("gpu.example.com")))
			Expect(pools[0].Key.Pool).To(Equal(unique.Make("pool-a")))
			Expect(pools[0].Devices).To(HaveLen(2))
		})

		It("should build a pool from a nodeSelector slice matching requirements", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(HaveLen(1))
		})

		It("should exclude pools with only incompatible nodeSelector slices", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withZoneSelector("eu-west-1a"),
					withAPIDevices("gpu-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(BeEmpty())
		})

		It("should exclude slices with no node affinity fields", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAPIDevices("gpu-0")),
			}
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(BeEmpty())
		})

		Context("node-name-pinned slices", func() {
			It("should include a NodeName slice when evaluating the existing node it is pinned to", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withNodeName("node-a"), withAPIDevices("gpu-0")),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "node-a")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(1))
			})

			It("should exclude a NodeName slice when evaluating a different node", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withNodeName("node-a"), withAPIDevices("gpu-0")),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "node-b")
				Expect(pools).To(BeEmpty())
			})

			It("should exclude a NodeName slice when evaluating an in-flight NodeClaim (no node name)", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withNodeName("node-a"), withAPIDevices("gpu-0")),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(BeEmpty())
			})

			It("should not apply a node name to AllNodes slices", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				}
				// AllNodes slices are accessible regardless of the candidate node name, including in-flight NodeClaims.
				Expect(dynamicresources.GatherPools(slices, reqs, "")).To(HaveLen(1))
				Expect(dynamicresources.GatherPools(slices, reqs, "node-a")).To(HaveLen(1))
			})
		})

		It("should return an empty list when no slices are provided", func() {
			pools := dynamicresources.GatherPools(nil, reqs, "")
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
			pools := dynamicresources.GatherPools(slices, reqs, "")
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
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(BeEmpty())
		})

		It("should group multiple slices into the same pool by driver+pool", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1")),
			}
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(HaveLen(1))
			Expect(pools[0].Devices).To(HaveLen(2))
			Expect(pools[0].Slices).To(HaveLen(2))
		})

		It("should create separate pools for different pool names", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				makeAPISlice("s2", "gpu.example.com", "pool-b", withAllNodes(), withAPIDevices("gpu-1")),
			}
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(HaveLen(2))
		})

		It("should create separate pools for different drivers", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				makeAPISlice("s2", "nic.example.com", "pool-a", withAllNodes(), withAPIDevices("nic-0")),
			}
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(HaveLen(2))
		})

		Context("validation", func() {
			It("should mark a pool as invalid when it has duplicate device names", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
					makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")), // duplicate
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeTrue())
			})

			It("should panic when a potential slice is provided", func() {
				slices := []dynamicresources.ResourceSlice{
					makePotentialSlice("gpu.example.com", "pool-a"),
				}
				Expect(func() {
					dynamicresources.GatherPools(slices, reqs, "")
				}).To(PanicWith("potential slices must not be passed to pool gathering or filtering"))
			})

			It("should mark a pool as invalid when a single slice has duplicate device names", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0", "gpu-0")),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeTrue())
			})

			It("should not mark a pool as invalid when device names are unique", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
					makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1")),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeFalse())
			})

			DescribeTable("should mark pool invalid when counter set names are duplicated",
				func(slices []dynamicresources.ResourceSlice) {
					pools := dynamicresources.GatherPools(slices, reqs, "")
					Expect(pools).To(HaveLen(1))
					Expect(pools[0].Invalid).To(BeTrue())
				},
				Entry("across slices", []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice-1", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("40Gi"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("counter-slice-2", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("40Gi"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withAPIDevices("gpu-0"),
						withGeneration(1, 3),
					),
				}),
				Entry("within a single slice", []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(
							counterSet("budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
							counterSet("budget", map[string]resource.Quantity{
								"compute": resource.MustParse("100"),
							}),
						),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withAPIDevices("gpu-0"),
						withGeneration(1, 2),
					),
				}),
			)

			DescribeTable("should mark pool invalid when counter references are invalid",
				func(slices []dynamicresources.ResourceSlice) {
					pools := dynamicresources.GatherPools(slices, reqs, "")
					Expect(pools).To(HaveLen(1))
					Expect(pools[0].Invalid).To(BeTrue())
				},
				Entry("device references non-existent counter set", []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "nonexistent-budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 2),
					),
				}),
				Entry("device references non-existent counter within set", []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"nonexistent-counter": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 2),
					),
				}),
				Entry("non-targeting device references non-existent counter set", []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("matching-device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 3),
					),
					makeAPISlice("non-matching-device-slice", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-1", "wrong-budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 3),
					),
				}),
				Entry("non-targeting device references non-existent counter within set", []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("matching-device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 3),
					),
					makeAPISlice("non-matching-device-slice", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-1", "budget", map[string]resource.Quantity{
								"nonexistent-counter": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 3),
					),
				}),
				Entry("device declares ConsumesCounters but pool has no counter sets", []dynamicresources.ResourceSlice{
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "nonexistent-budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 1),
					),
				}),
				Entry("device references counter that exists in a different counter set", []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(
							counterSet("memory-budget", map[string]resource.Quantity{
								"memory": resource.MustParse("80Gi"),
							}),
							counterSet("compute-budget", map[string]resource.Quantity{
								"flops": resource.MustParse("1000"),
							}),
						),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "memory-budget", map[string]resource.Quantity{
								"flops": resource.MustParse("500"),
							}),
						),
						withGeneration(1, 2),
					),
				}),
			)

			It("should not mark pool invalid when device counter references are valid", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
							"flops":  resource.MustParse("1000"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeFalse())
			})
		})

		Context("device IDs", func() {
			It("should construct correct DeviceIDs", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices[0].ID.Driver).To(Equal(unique.Make("gpu.example.com")))
				Expect(pools[0].Devices[0].ID.Pool).To(Equal(unique.Make("pool-a")))
				Expect(pools[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-0")))
			})

			It("should construct correct DeviceIDs for multiple devices in a single slice", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0", "gpu-1", "gpu-2")),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
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
				pools := dynamicresources.GatherPools(slices, reqs, "")
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
				pools := dynamicresources.GatherPools(slices, reqs, "")
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
				pools := dynamicresources.GatherPools(slices, reqs, "")
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
				pools := dynamicresources.GatherPools(slices, reqs, "")
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
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeTrue())
				Expect(pools[0].Devices).To(BeEmpty())
			})

			It("should return new generation as valid when previous generation was incomplete", func() {
				slices := []dynamicresources.ResourceSlice{
					// Gen 1: incomplete (1 of 2)
					makeAPISlice("s-old", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-old"), withGeneration(1, 2)),
					// Gen 2: complete (1 of 1)
					makeAPISlice("s-new", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-new"), withGeneration(2, 1)),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
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
				pools := dynamicresources.GatherPools(slices, reqs, "")
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
				pools := dynamicresources.GatherPools(slices, reqs, "")
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
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeTrue())
			})

			It("should not mark a pool as incomplete when slice count matches", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 2)),
					makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1"), withGeneration(1, 2)),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeFalse())
			})

			It("should mark a pool as incomplete when more slices than ResourceSliceCount", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0"), withGeneration(1, 1)),
					makeAPISlice("s2", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-1"), withGeneration(1, 1)),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeTrue())
			})

			It("should mark a pool as incomplete when ResourceSliceCount is 0", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeTrue())
			})

			It("should not mark a pool as incomplete when non-matching slices complete the count", func() {
				// Pool has 2 slices: one matches our node, one targets a different node.
				// The pool should be considered complete because all slices exist globally.
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 2),
					),
					makeAPISlice("s2", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"), // non-matching
						withAPIDevices("gpu-1"),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
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
						withZoneSelector("us-west-2a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 1),
					),
					makeAPISlice("s-new", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"), // non-matching
						withAPIDevices("gpu-new"),
						withGeneration(2, 1),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				// Gen 2 supersedes gen 1. Gen 2 has no matching slices, so pool is nil.
				Expect(pools).To(BeEmpty())
			})

			It("should include only matching slices when pool has mixed node selectors", func() {
				// Pool has 3 slices: 2 match, 1 doesn't. All same generation.
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 3),
					),
					makeAPISlice("s2", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2b"),
						withAPIDevices("gpu-1"),
						withGeneration(1, 3),
					),
					makeAPISlice("s3", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withAPIDevices("gpu-2"),
						withGeneration(1, 3),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeFalse())
				Expect(pools[0].Devices).To(HaveLen(2))
				Expect(pools[0].Slices).To(HaveLen(2))
			})
		})

		Context("Partitionable Devices", func() {
			It("should gather counter sets from a SharedCounters slice", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("memory-budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "memory-budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].CounterSets).To(HaveKey("memory-budget"))
				Expect(pools[0].CounterSets["memory-budget"]).To(HaveKey("memory"))
				Expect(pools[0].CounterSets["memory-budget"]["memory"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())
			})

			It("should not include counter-set slices in pool.Slices", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"compute": resource.MustParse("100"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"compute": resource.MustParse("50"),
							}),
						),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Slices).To(HaveLen(1))
				Expect(pools[0].CounterSets).To(HaveKey("budget"))
				Expect(pools[0].CounterSets["budget"]).To(HaveKey("compute"))
			})

			It("should count counter-set slices toward completeness", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("device-slice-1", "gpu.example.com", "pool-a",
						withAllNodes(),
						withAPIDevices("gpu-0"),
						withGeneration(1, 3),
					),
					makeAPISlice("device-slice-2", "gpu.example.com", "pool-a",
						withAllNodes(),
						withAPIDevices("gpu-1"),
						withGeneration(1, 3),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeFalse())
			})

			It("should mark pool incomplete when counter-set slice is missing", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withAPIDevices("gpu-0"),
						withGeneration(1, 2),
					),
					// counter-set slice is missing (only 1 of 2 slices present)
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeTrue())
			})

			It("should gather multiple counter sets from a single slice", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(
							counterSet("memory-budget", map[string]resource.Quantity{
								"memory": resource.MustParse("80Gi"),
							}),
							counterSet("compute-budget", map[string]resource.Quantity{
								"flops": resource.MustParse("1000"),
							}),
						),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withAPIDevices("gpu-0"),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].CounterSets).To(HaveLen(2))
				Expect(pools[0].CounterSets).To(HaveKey("memory-budget"))
				Expect(pools[0].CounterSets).To(HaveKey("compute-budget"))
			})

			It("should gather counter sets from multiple SharedCounters slices in the same pool", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice-1", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("memory-budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("counter-slice-2", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("compute-budget", map[string]resource.Quantity{
							"flops": resource.MustParse("1000"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withAPIDevices("gpu-0"),
						withGeneration(1, 3),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeFalse())
				Expect(pools[0].CounterSets).To(HaveLen(2))
				Expect(pools[0].CounterSets).To(HaveKey("memory-budget"))
				Expect(pools[0].CounterSets).To(HaveKey("compute-budget"))
			})

			It("should gather counter sets correctly regardless of slice ordering", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 2),
					),
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeFalse())
				Expect(pools[0].CounterSets).To(HaveKey("budget"))
				Expect(pools[0].Devices).To(HaveLen(1))
			})

			It("should ignore devices on a slice that also declares SharedCounters", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("both-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withAPIDevices("ghost-device"),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withAPIDevices("gpu-0"),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-0")))
				Expect(pools[0].CounterSets).To(HaveKey("budget"))
			})

			It("should treat SharedCounters slice as always matching requirements", func() {
				narrowReqs := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "ap-southeast-1a"),
				)
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withZoneSelector("ap-southeast-1a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, narrowReqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].CounterSets).To(HaveKey("budget"))
			})

			It("should handle pool with SharedCounters slice and no ConsumesCounters on devices", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withAPIDevices("gpu-0", "gpu-1"),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].CounterSets).To(HaveKey("budget"))
				Expect(pools[0].Devices).To(HaveLen(2))
				Expect(pools[0].Invalid).To(BeFalse())
			})

			It("should handle devices consuming from multiple counter sets", func() {
				dev := resourcev1.Device{
					Name: "gpu-0",
					ConsumesCounters: []resourcev1.DeviceCounterConsumption{
						{
							CounterSet: "memory-budget",
							Counters: map[string]resourcev1.Counter{
								"memory": {Value: resource.MustParse("40Gi")},
							},
						},
						{
							CounterSet: "compute-budget",
							Counters: map[string]resourcev1.Counter{
								"flops": {Value: resource.MustParse("500")},
							},
						},
					},
				}
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(
							counterSet("memory-budget", map[string]resource.Quantity{
								"memory": resource.MustParse("80Gi"),
							}),
							counterSet("compute-budget", map[string]resource.Quantity{
								"flops": resource.MustParse("1000"),
							}),
						),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(dev),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeFalse())
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].Devices[0].ConsumesCounters).To(HaveLen(2))
			})

			It("should return nil pool when only SharedCounters slices exist with no device slices matching", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("non-matching-device-slice", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(BeEmpty())
			})

			It("should retain non-matching devices with ConsumesCounters as NonTargetingDevices", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("matching-device-slice", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 3),
					),
					makeAPISlice("non-matching-device-slice", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-1", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 3),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-0")))
				Expect(pools[0].NonTargetingDevices).To(HaveLen(1))
				Expect(pools[0].NonTargetingDevices[0].ID.Device).To(Equal(unique.Make("gpu-1")))
			})

			It("should not retain non-matching devices without ConsumesCounters as NonTargetingDevices", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("matching-slice", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 2),
					),
					makeAPISlice("non-matching-slice", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withAPIDevices("gpu-1"),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].NonTargetingDevices).To(BeEmpty())
			})

			It("should retain only devices with ConsumesCounters from a non-matching slice with mixed devices", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("matching-slice", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 3),
					),
					makeAPISlice("non-matching-mixed-slice", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-1", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("20Gi"),
							}),
							deviceConsumingCounter("gpu-2", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("20Gi"),
							}),
						),
						withGeneration(1, 3),
						func(s *resourcev1.ResourceSlice) {
							s.Spec.Devices = append(s.Spec.Devices, resourcev1.Device{Name: "gpu-plain"})
						},
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(1))
				Expect(pools[0].NonTargetingDevices).To(HaveLen(2))
				nonTargetingNames := []string{
					pools[0].NonTargetingDevices[0].ID.Device.Value(),
					pools[0].NonTargetingDevices[1].ID.Device.Value(),
				}
				Expect(nonTargetingNames).To(ConsistOf("gpu-1", "gpu-2"))
			})

			It("should preserve ConsumesCounters data on NonTargetingDevices", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("matching-slice", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 3),
					),
					makeAPISlice("non-matching-slice", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-1", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 3),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].NonTargetingDevices).To(HaveLen(1))
				ntd := pools[0].NonTargetingDevices[0]
				Expect(ntd.ID.Driver).To(Equal(unique.Make("gpu.example.com")))
				Expect(ntd.ID.Pool).To(Equal(unique.Make("pool-a")))
				Expect(ntd.ID.Device).To(Equal(unique.Make("gpu-1")))
				Expect(ntd.TopologyRequirements).To(BeNil())
				Expect(ntd.ConsumesCounters).To(HaveLen(1))
				Expect(ntd.ConsumesCounters[0].CounterSet).To(Equal("budget"))
				Expect(ntd.ConsumesCounters[0].Counters).To(HaveKey("memory"))
				Expect(ntd.ConsumesCounters[0].Counters["memory"].Value.Equal(resource.MustParse("40Gi"))).To(BeTrue())
			})

			It("should retain pool during build when only NonTargetingDevices exist", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("non-matching-slice", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Slices).To(BeEmpty())
				Expect(pools[0].Devices).To(BeEmpty())
				Expect(pools[0].NonTargetingDevices).To(HaveLen(1))
				Expect(pools[0].NonTargetingDevices[0].ID.Device).To(Equal(unique.Make("gpu-0")))
				Expect(pools[0].CounterSets).To(HaveKey("budget"))
			})
		})
	})

	Describe("FilterPools", func() {
		It("should return an empty list when no pools are provided", func() {
			filtered := dynamicresources.FilterPools(nil, reqs, "")
			Expect(filtered).To(BeEmpty())
		})

		It("should retain pools with compatible slices", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(HaveLen(1))

			tightened := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
			)
			filtered := dynamicresources.FilterPools(pools, tightened, "")
			Expect(filtered).To(HaveLen(1))
		})

		It("should exclude pools with incompatible slices after requirement tightening", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(HaveLen(1))

			tightened := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2b"),
			)
			filtered := dynamicresources.FilterPools(pools, tightened, "")
			Expect(filtered).To(BeEmpty())
		})

		It("should panic when a pool contains potential slices", func() {
			pool := &dynamicresources.Pool{
				Slices: []dynamicresources.ResourceSlice{
					makePotentialSlice("gpu.example.com", "pool-a"),
				},
			}
			Expect(func() {
				dynamicresources.FilterPools([]*dynamicresources.Pool{pool}, reqs, "")
			}).To(PanicWith("potential slices must not be passed to pool gathering or filtering"))
		})

		It("should always retain pools with AllNodes slices", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a", withAllNodes(), withAPIDevices("gpu-0")),
			}
			pools := dynamicresources.GatherPools(slices, reqs, "")

			tightened := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "ap-southeast-1a"),
			)
			filtered := dynamicresources.FilterPools(pools, tightened, "")
			Expect(filtered).To(HaveLen(1))
		})

		It("should retain some pools and exclude others", func() {
			slices := []dynamicresources.ResourceSlice{
				makeAPISlice("s1", "gpu.example.com", "pool-a",
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-0"),
				),
				makeAPISlice("s2", "nic.example.com", "pool-b",
					withZoneSelector("us-west-2b"),
					withAPIDevices("nic-0"),
				),
			}
			pools := dynamicresources.GatherPools(slices, reqs, "")
			Expect(pools).To(HaveLen(2))

			tightened := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
			)
			filtered := dynamicresources.FilterPools(pools, tightened, "")
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
					withZoneSelector("us-west-2a"),
					withAPIDevices("gpu-0"),
				),
				makeAPISlice("s2", "gpu.example.com", "pool-a",
					withZoneSelector("us-west-2b"),
					withAPIDevices("gpu-1"),
				),
			}
			pools := dynamicresources.GatherPools(slices, broad, "")
			Expect(pools).To(HaveLen(1))
			Expect(pools[0].Slices).To(HaveLen(2))
			Expect(pools[0].Devices).To(HaveLen(2))

			// Tighten to only us-west-2a — the us-west-2b slice should be stripped.
			tightened := scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
			)
			filtered := dynamicresources.FilterPools(pools, tightened, "")
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
			filtered := dynamicresources.FilterPools([]*dynamicresources.Pool{pool}, reqs, "")
			Expect(filtered).To(BeEmpty())
		})

		Context("Partitionable Devices", func() {
			It("should preserve CounterSets through filtering", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))

				filtered := dynamicresources.FilterPools(pools, reqs, "")
				Expect(filtered).To(HaveLen(1))
				Expect(filtered[0].CounterSets).To(HaveKey("budget"))
				Expect(filtered[0].CounterSets["budget"]["memory"].Value.Equal(resource.MustParse("80Gi"))).To(BeTrue())
			})

			It("should move devices with ConsumesCounters to NonTargetingDevices when requirements tighten", func() {
				broad := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				)
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 3),
					),
					makeAPISlice("device-slice-a", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 3),
					),
					makeAPISlice("device-slice-b", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2b"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-1", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 3),
					),
				}
				pools := dynamicresources.GatherPools(slices, broad, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(2))
				Expect(pools[0].NonTargetingDevices).To(BeEmpty())

				// Tighten to only us-west-2a — gpu-1 should move to NonTargetingDevices
				tightened := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
				)
				filtered := dynamicresources.FilterPools(pools, tightened, "")
				Expect(filtered).To(HaveLen(1))
				Expect(filtered[0].Devices).To(HaveLen(1))
				Expect(filtered[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-0")))
				Expect(filtered[0].NonTargetingDevices).To(HaveLen(1))
				Expect(filtered[0].NonTargetingDevices[0].ID.Device).To(Equal(unique.Make("gpu-1")))
			})

			It("should not move devices without ConsumesCounters to NonTargetingDevices on tightening", func() {
				broad := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				)
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("device-slice-a", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withAPIDevices("gpu-0"),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice-b", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2b"),
						withAPIDevices("gpu-1"),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, broad, "")
				Expect(pools).To(HaveLen(1))

				tightened := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
				)
				filtered := dynamicresources.FilterPools(pools, tightened, "")
				Expect(filtered).To(HaveLen(1))
				Expect(filtered[0].Devices).To(HaveLen(1))
				Expect(filtered[0].NonTargetingDevices).To(BeEmpty())
			})

			It("should preserve existing NonTargetingDevices through filtering", func() {
				broad := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				)
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 4),
					),
					makeAPISlice("device-slice-a", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("20Gi"),
							}),
						),
						withGeneration(1, 4),
					),
					makeAPISlice("device-slice-b", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2b"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-1", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("20Gi"),
							}),
						),
						withGeneration(1, 4),
					),
					makeAPISlice("device-slice-eu", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-2", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("20Gi"),
							}),
						),
						withGeneration(1, 4),
					),
				}
				// Gather with broad requirements — eu-west-1a is non-matching, gpu-2 goes to NonTargeting
				pools := dynamicresources.GatherPools(slices, broad, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].NonTargetingDevices).To(HaveLen(1))
				Expect(pools[0].NonTargetingDevices[0].ID.Device).To(Equal(unique.Make("gpu-2")))

				// Now tighten to us-west-2a only — gpu-1 should also move to NonTargeting
				tightened := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
				)
				filtered := dynamicresources.FilterPools(pools, tightened, "")
				Expect(filtered).To(HaveLen(1))
				Expect(filtered[0].Devices).To(HaveLen(1))
				Expect(filtered[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-0")))
				// Both gpu-1 (newly non-targeting) and gpu-2 (previously non-targeting) should be present
				Expect(filtered[0].NonTargetingDevices).To(HaveLen(2))
				nonTargetingNames := []string{
					filtered[0].NonTargetingDevices[0].ID.Device.Value(),
					filtered[0].NonTargetingDevices[1].ID.Device.Value(),
				}
				Expect(nonTargetingNames).To(ConsistOf("gpu-1", "gpu-2"))
			})

			It("should always retain pools with SharedCounters slices through filtering", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))

				// Even with different zone requirements, the pool with AllNodes devices is retained
				tightened := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "ap-southeast-1a"),
				)
				filtered := dynamicresources.FilterPools(pools, tightened, "")
				Expect(filtered).To(HaveLen(1))
				Expect(filtered[0].CounterSets).To(HaveKey("budget"))
			})

			It("should preserve Incomplete and Invalid flags through filtering", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 3), // only 2 slices present for count=3
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withAllNodes(),
						withAPIDevices("gpu-0", "gpu-0"), // duplicate device name -> invalid
						withGeneration(1, 3),
					),
				}
				pools := dynamicresources.GatherPools(slices, reqs, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Incomplete).To(BeTrue())
				Expect(pools[0].Invalid).To(BeTrue())

				filtered := dynamicresources.FilterPools(pools, reqs, "")
				Expect(filtered).To(HaveLen(1))
				Expect(filtered[0].Incomplete).To(BeTrue())
				Expect(filtered[0].Invalid).To(BeTrue())
			})

			It("should preserve invalid pool through filtering even when all slices become incompatible", func() {
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("s1", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withAPIDevices("gpu-0", "gpu-0"), // duplicate -> invalid
					),
				}
				broad := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				)
				pools := dynamicresources.GatherPools(slices, broad, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Invalid).To(BeTrue())

				// Tighten requirements to a different zone — the slice no longer matches.
				// The invalid pool must still be preserved so All-mode can detect and error on it.
				tightened := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2b"),
				)
				filtered := dynamicresources.FilterPools(pools, tightened, "")
				Expect(filtered).To(HaveLen(1))
				Expect(filtered[0].Invalid).To(BeTrue())
				Expect(filtered[0].Devices).To(BeEmpty())
			})

			It("should not mutate the original pool's NonTargetingDevices when filtering multiple times", func() {
				// Start with a pool that already has NonTargetingDevices populated
				// (eu-west-1a is non-matching). This exercises the aliasing risk where
				// filterPool copies the NonTargetingDevices slice header and then appends.
				broad := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				)
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 4),
					),
					makeAPISlice("device-slice-a", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("20Gi"),
							}),
						),
						withGeneration(1, 4),
					),
					makeAPISlice("device-slice-b", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2b"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-1", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("20Gi"),
							}),
						),
						withGeneration(1, 4),
					),
					makeAPISlice("device-slice-eu", "gpu.example.com", "pool-a",
						withZoneSelector("eu-west-1a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-eu", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("20Gi"),
							}),
						),
						withGeneration(1, 4),
					),
				}
				pools := dynamicresources.GatherPools(slices, broad, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(2))
				Expect(pools[0].NonTargetingDevices).To(HaveLen(1))
				Expect(pools[0].NonTargetingDevices[0].ID.Device).To(Equal(unique.Make("gpu-eu")))

				// First filter: tighten to us-west-2a — gpu-1 moves to NonTargeting
				filter1 := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a"),
				)
				filtered1 := dynamicresources.FilterPools(pools, filter1, "")
				Expect(filtered1).To(HaveLen(1))
				Expect(filtered1[0].Devices).To(HaveLen(1))
				Expect(filtered1[0].NonTargetingDevices).To(HaveLen(2))

				// Second filter on the SAME original pool with different requirements.
				// If the first filter mutated the backing array, this would produce wrong results.
				filter2 := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2b"),
				)
				filtered2 := dynamicresources.FilterPools(pools, filter2, "")
				Expect(filtered2).To(HaveLen(1))
				Expect(filtered2[0].Devices).To(HaveLen(1))
				Expect(filtered2[0].Devices[0].ID.Device).To(Equal(unique.Make("gpu-1")))
				Expect(filtered2[0].NonTargetingDevices).To(HaveLen(2))

				// Original pool must be unchanged
				Expect(pools[0].Devices).To(HaveLen(2))
				Expect(pools[0].NonTargetingDevices).To(HaveLen(1))
				Expect(pools[0].NonTargetingDevices[0].ID.Device).To(Equal(unique.Make("gpu-eu")))
			})

			It("should retain pool with zero Slices when NonTargetingDevices exist after tightening", func() {
				broad := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2a", "us-west-2b"),
				)
				slices := []dynamicresources.ResourceSlice{
					makeAPISlice("counter-slice", "gpu.example.com", "pool-a",
						withSharedCounters(counterSet("budget", map[string]resource.Quantity{
							"memory": resource.MustParse("80Gi"),
						})),
						withGeneration(1, 2),
					),
					makeAPISlice("device-slice", "gpu.example.com", "pool-a",
						withZoneSelector("us-west-2a"),
						withDevicesConsumingCounters(
							deviceConsumingCounter("gpu-0", "budget", map[string]resource.Quantity{
								"memory": resource.MustParse("40Gi"),
							}),
						),
						withGeneration(1, 2),
					),
				}
				pools := dynamicresources.GatherPools(slices, broad, "")
				Expect(pools).To(HaveLen(1))
				Expect(pools[0].Devices).To(HaveLen(1))

				// Tighten to a zone that matches NO device slices — but gpu-0 has
				// ConsumesCounters so it should become NonTargeting and keep the pool alive
				tightened := scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-west-2b"),
				)
				filtered := dynamicresources.FilterPools(pools, tightened, "")
				Expect(filtered).To(HaveLen(1))
				Expect(filtered[0].Slices).To(BeEmpty())
				Expect(filtered[0].Devices).To(BeEmpty())
				Expect(filtered[0].NonTargetingDevices).To(HaveLen(1))
				Expect(filtered[0].NonTargetingDevices[0].ID.Device).To(Equal(unique.Make("gpu-0")))
			})
		})
	})
})
