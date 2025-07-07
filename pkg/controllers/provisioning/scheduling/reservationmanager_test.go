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

package scheduling_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	pscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
)

var _ = Describe("ReservationManager", func() {
	var rm *scheduling.ReservationManager
	var offerings []cloudprovider.Offerings
	var instanceTypes []*cloudprovider.InstanceType

	BeforeEach(func() {
		// Create hardcoded instance types with reserved offerings
		instanceTypes = []*cloudprovider.InstanceType{
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "small-reserved",
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
					corev1.ResourcePods:   resource.MustParse("10"),
				},
				Offerings: []*cloudprovider.Offering{
					{
						Available:           true,
						ReservationCapacity: 3,
						Requirements: pscheduling.NewLabelRequirements(map[string]string{
							v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
							corev1.LabelTopologyZone:         "test-zone-1",
							cloudprovider.ReservationIDLabel: "small-reserved",
						}),
					},
				},
			}),
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "medium-reserved",
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("20"),
				},
				Offerings: []*cloudprovider.Offering{
					{
						Available:           true,
						ReservationCapacity: 2,
						Requirements: pscheduling.NewLabelRequirements(map[string]string{
							v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
							corev1.LabelTopologyZone:         "test-zone-2",
							cloudprovider.ReservationIDLabel: "medium-reserved",
						}),
					},
				},
			}),
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "large-reserved",
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("8"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
					corev1.ResourcePods:   resource.MustParse("40"),
				},
				Offerings: []*cloudprovider.Offering{
					{
						Available:           true,
						ReservationCapacity: 1,
						Requirements: pscheduling.NewLabelRequirements(map[string]string{
							v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
							corev1.LabelTopologyZone:         "test-zone-3",
							cloudprovider.ReservationIDLabel: "large-reserved",
						}),
					},
				},
			}),
		}

		// Extract offerings from instance types
		offerings = nil // Reset offerings slice
		for _, it := range instanceTypes {
			offerings = append(offerings, it.Offerings)
		}

		rm = scheduling.NewReservationManager(map[string][]*cloudprovider.InstanceType{"": instanceTypes})
	})

	Describe("NewReservationManager", func() {
		It("should handle empty instance types", func() {
			emptyInstanceTypes := map[string][]*cloudprovider.InstanceType{}
			emptyRM := scheduling.NewReservationManager(emptyInstanceTypes)
			Expect(emptyRM).ToNot(BeNil())
		})

		It("should handle instance types without reserved offerings", func() {
			nonReservedTypes := map[string][]*cloudprovider.InstanceType{
				"nodepool-1": {
					fake.NewInstanceType(fake.InstanceTypeOptions{
						Name: "on-demand-only",
						Offerings: []*cloudprovider.Offering{
							{
								Available: true,
								Requirements: pscheduling.NewLabelRequirements(map[string]string{
									v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
								}),
							},
						},
					}),
				},
			}
			nonReservedRM := scheduling.NewReservationManager(nonReservedTypes)
			Expect(nonReservedRM).ToNot(BeNil())
		})

		It("should track the minimum capacity when multiple offerings have the same reservation ID", func() {
			duplicateReservationTypes := map[string][]*cloudprovider.InstanceType{
				"nodepool-1": {
					fake.NewInstanceType(fake.InstanceTypeOptions{
						Name: "instance-1",
						Offerings: []*cloudprovider.Offering{
							{
								Available:           true,
								ReservationCapacity: 10,
								Requirements: pscheduling.NewLabelRequirements(map[string]string{
									v1.CapacityTypeLabelKey: v1.CapacityTypeReserved,
								}),
							},
						},
					}),
					fake.NewInstanceType(fake.InstanceTypeOptions{
						Name: "instance-2",
						Offerings: []*cloudprovider.Offering{
							{
								Available:           true,
								ReservationCapacity: 5, // Lower capacity should be tracked
								Requirements: pscheduling.NewLabelRequirements(map[string]string{
									v1.CapacityTypeLabelKey: v1.CapacityTypeReserved,
								}),
							},
						},
					}),
				},
			}
			duplicateRM := scheduling.NewReservationManager(duplicateReservationTypes)
			Expect(duplicateRM).ToNot(BeNil())
		})
	})

	Describe("CanReserve", func() {
		var testOffering *cloudprovider.Offering

		BeforeEach(func() {
			testOffering = offerings[0][0] // Use first offering from small-reserved (capacity 3)
		})

		Context("With Available Capacity", func() {
			It("should return true when capacity is available", func() {
				canReserve := rm.CanReserve("hostname-1", testOffering)
				Expect(canReserve).To(BeTrue())
			})

			It("should return true when hostname already has the reservation", func() {
				// First reserve
				rm.Reserve("hostname-1", testOffering)
				// Should still be able to "reserve" the same reservation for the same hostname
				canReserve := rm.CanReserve("hostname-1", testOffering)
				Expect(canReserve).To(BeTrue())
			})
		})

		Context("With No Capacity", func() {
			It("should return false when capacity is exhausted", func() {
				// Exhaust all capacity
				for i := 0; i < 3; i++ {
					rm.Reserve(fmt.Sprintf("hostname-%d", i), testOffering)
				}
				// Should not be able to reserve more
				canReserve := rm.CanReserve("hostname-new", testOffering)
				Expect(canReserve).To(BeFalse())
			})

			It("should return true for existing hostname even when capacity is exhausted", func() {
				// Reserve for hostname-1
				rm.Reserve("hostname-1", testOffering)
				// Exhaust remaining capacity with other hostnames
				for i := 0; i < 2; i++ {
					rm.Reserve(fmt.Sprintf("hostname-other-%d", i), testOffering)
				}
				// Should still return true for hostname-1
				canReserve := rm.CanReserve("hostname-1", testOffering)
				Expect(canReserve).To(BeTrue())
			})
		})

		Context("Error Cases", func() {
			It("should panic with non-existent reservation ID", func() {
				nonExistentOffering := &cloudprovider.Offering{
					Available:           true,
					ReservationCapacity: 1,
					Requirements: pscheduling.NewLabelRequirements(map[string]string{
						v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
						cloudprovider.ReservationIDLabel: "i-dont-exist",
					}),
				}
				Expect(func() {
					rm.CanReserve("hostname-1", nonExistentOffering)
				}).To(Panic())
			})
		})
	})

	Describe("Reserve", func() {
		var testOffering1, testOffering2 *cloudprovider.Offering

		BeforeEach(func() {
			testOffering1 = offerings[0][0] // Use first offering from small-reserved (capacity 3)
			testOffering2 = offerings[1][0] // Use first offering from medium-reserved (capacity 2)
		})

		Context("Single Reservations", func() {
			It("should successfully reserve capacity", func() {
				rm.Reserve("hostname-1", testOffering1)
				// Verify reservation was made by checking CanReserve behavior
				canReserve := rm.CanReserve("hostname-1", testOffering1)
				Expect(canReserve).To(BeTrue())
			})

			It("should not double-reserve for the same hostname and reservation", func() {
				// Reserve twice for the same hostname and reservation
				rm.Reserve("hostname-1", testOffering1)
				rm.Reserve("hostname-1", testOffering1)

				// Should still have capacity for other hostnames
				canReserve := rm.CanReserve("hostname-2", testOffering1)
				Expect(canReserve).To(BeTrue())
			})

			It("should decrement available capacity", func() {
				// Reserve capacity and verify it's decremented
				rm.Reserve("hostname-1", testOffering1)
				rm.Reserve("hostname-2", testOffering1)
				rm.Reserve("hostname-3", testOffering1)

				// Should have no capacity left
				canReserve := rm.CanReserve("hostname-4", testOffering1)
				Expect(canReserve).To(BeFalse())
			})
		})

		Context("Multiple Reservations", func() {
			It("should handle multiple offerings in a single call", func() {
				rm.Reserve("hostname-1", testOffering1, testOffering2)

				// Verify both reservations were made
				canReserve1 := rm.CanReserve("hostname-1", testOffering1)
				canReserve2 := rm.CanReserve("hostname-1", testOffering2)
				Expect(canReserve1).To(BeTrue())
				Expect(canReserve2).To(BeTrue())
			})

			It("should handle mixed new and existing reservations", func() {
				// First reserve one offering
				rm.Reserve("hostname-1", testOffering1)

				// Then reserve both (one existing, one new)
				rm.Reserve("hostname-1", testOffering1, testOffering2)

				// Verify both are reserved and capacity is correctly tracked
				canReserve1 := rm.CanReserve("hostname-1", testOffering1)
				canReserve2 := rm.CanReserve("hostname-1", testOffering2)
				Expect(canReserve1).To(BeTrue())
				Expect(canReserve2).To(BeTrue())

				// Verify capacity was only decremented once for testOffering1
				canReserveOther1 := rm.CanReserve("hostname-2", testOffering1)
				Expect(canReserveOther1).To(BeTrue()) // Should still have capacity
			})
		})

		Context("Error Cases", func() {
			It("should panic when trying to over-reserve", func() {
				// Exhaust capacity
				for i := 0; i < 3; i++ {
					rm.Reserve(fmt.Sprintf("hostname-%d", i), testOffering1)
				}

				// Attempting to reserve more should panic
				Expect(func() {
					rm.Reserve("hostname-new", testOffering1)
				}).To(Panic())
			})

			It("should panic with non-existent reservation ID", func() {
				nonExistentOffering := &cloudprovider.Offering{
					Available:           true,
					ReservationCapacity: 1,
					Requirements: pscheduling.NewLabelRequirements(map[string]string{
						v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
						cloudprovider.ReservationIDLabel: "i-dont-exist",
					}),
				}
				Expect(func() {
					rm.Reserve("hostname-1", nonExistentOffering)
				}).To(Panic())
			})
		})
	})

	Describe("Release", func() {
		var testOffering1, testOffering2 *cloudprovider.Offering

		BeforeEach(func() {
			testOffering1 = offerings[0][0] // Use first offering from small-reserved (capacity 3)
			testOffering2 = offerings[1][0] // Use first offering from medium-reserved (capacity 2)
		})

		Context("Valid Releases", func() {
			It("should release a single reservation", func() {
				// Reserve and then release
				rm.Reserve("hostname-1", testOffering1)
				rm.Release("hostname-1", testOffering1)

				// Verify the reservation is no longer tracked for this hostname
				// but capacity should be restored
				canReserve := rm.CanReserve("hostname-2", testOffering1)
				Expect(canReserve).To(BeTrue())
			})

			It("should handle releasing non-existent reservations gracefully", func() {
				// Should not panic when releasing a reservation that doesn't exist
				Expect(func() {
					rm.Release("hostname-1", testOffering1)
				}).ToNot(Panic())
			})

			It("should handle releasing from non-existent hostname gracefully", func() {
				rm.Reserve("hostname-1", testOffering1)
				// Should not panic when releasing from a different hostname
				Expect(func() {
					rm.Release("hostname-2", testOffering1)
				}).ToNot(Panic())
			})
		})

		Context("Multiple Releases", func() {
			It("should handle multiple offerings in a single call", func() {
				// Reserve both offerings
				rm.Reserve("hostname-1", testOffering1, testOffering2)

				// Release both
				rm.Release("hostname-1", testOffering1, testOffering2)

				// Verify capacity is restored for both
				canReserve1 := rm.CanReserve("hostname-2", testOffering1)
				canReserve2 := rm.CanReserve("hostname-2", testOffering2)
				Expect(canReserve1).To(BeTrue())
				Expect(canReserve2).To(BeTrue())
			})

			It("should handle partial releases", func() {
				// Reserve both offerings
				rm.Reserve("hostname-1", testOffering1, testOffering2)

				// Release only one
				rm.Release("hostname-1", testOffering1)

				// Verify only the released one has restored capacity
				canReserve1 := rm.CanReserve("hostname-1", testOffering1)
				canReserve2 := rm.CanReserve("hostname-1", testOffering2)
				Expect(canReserve1).To(BeTrue()) // Should still be reserved for hostname-1
				Expect(canReserve2).To(BeTrue()) // Should still be reserved for hostname-1
			})
		})

		Context("Capacity Restoration", func() {
			It("should restore capacity when releasing reservations", func() {
				// Exhaust capacity
				rm.Reserve("hostname-1", testOffering1)
				rm.Reserve("hostname-2", testOffering1)
				rm.Reserve("hostname-3", testOffering1)

				// Verify no capacity left
				canReserve := rm.CanReserve("hostname-4", testOffering1)
				Expect(canReserve).To(BeFalse())

				// Release one reservation
				rm.Release("hostname-1", testOffering1)

				// Verify capacity is restored
				canReserve = rm.CanReserve("hostname-4", testOffering1)
				Expect(canReserve).To(BeTrue())
			})

			It("should correctly track capacity after multiple reserve/release cycles", func() {
				// Reserve, release, reserve again
				rm.Reserve("hostname-1", testOffering1)
				rm.Release("hostname-1", testOffering1)
				rm.Reserve("hostname-2", testOffering1)

				// Should still have capacity available
				canReserve := rm.CanReserve("hostname-3", testOffering1)
				Expect(canReserve).To(BeTrue())
			})
		})
	})

	Describe("Integration Scenarios", func() {
		var offering1, offering2, offering3 *cloudprovider.Offering

		BeforeEach(func() {
			offering1 = offerings[1][0] // Use medium-reserved offering (capacity 2)
			offering2 = offerings[0][0] // Use small-reserved offering (capacity 3)
			offering3 = offerings[2][0] // Use large-reserved offering (capacity 1)
		})

		It("should handle complex reservation patterns", func() {
			// Multiple hostnames with multiple reservations
			rm.Reserve("host-1", offering1, offering2)
			rm.Reserve("host-2", offering1, offering3)
			rm.Reserve("host-3", offering2)

			// Verify all reservations are tracked
			Expect(rm.CanReserve("host-1", offering1)).To(BeTrue())
			Expect(rm.CanReserve("host-1", offering2)).To(BeTrue())
			Expect(rm.CanReserve("host-2", offering1)).To(BeTrue())
			Expect(rm.CanReserve("host-2", offering3)).To(BeTrue())
			Expect(rm.CanReserve("host-3", offering2)).To(BeTrue())

			// Verify capacity limits
			Expect(rm.CanReserve("host-4", offering1)).To(BeFalse()) // Exhausted (2 capacity, 2 used)
			Expect(rm.CanReserve("host-4", offering2)).To(BeTrue())  // Still available (3 capacity, 2 used)
			Expect(rm.CanReserve("host-4", offering3)).To(BeFalse()) // Exhausted (1 capacity, 1 used)
		})

		It("should handle reservation conflicts correctly", func() {
			// Reserve same offering for different hostnames
			rm.Reserve("host-1", offering1)
			rm.Reserve("host-2", offering1)

			// Both should be able to "reserve" their existing reservations
			Expect(rm.CanReserve("host-1", offering1)).To(BeTrue())
			Expect(rm.CanReserve("host-2", offering1)).To(BeTrue())

			// But no new reservations should be possible
			Expect(rm.CanReserve("host-3", offering1)).To(BeFalse())
		})

		It("should maintain consistency during mixed operations", func() {
			// Complex sequence of operations
			rm.Reserve("host-1", offering1, offering2)
			rm.Reserve("host-2", offering2, offering3)
			rm.Release("host-1", offering1)
			rm.Reserve("host-3", offering1)
			rm.Release("host-2", offering3)

			// Verify final state
			Expect(rm.CanReserve("host-1", offering1)).To(BeTrue()) // Released, so not reserved for host-1
			Expect(rm.CanReserve("host-1", offering2)).To(BeTrue()) // Still reserved for host-1
			Expect(rm.CanReserve("host-2", offering2)).To(BeTrue()) // Still reserved for host-2
			Expect(rm.CanReserve("host-2", offering3)).To(BeTrue()) // Released, so not reserved for host-2
			Expect(rm.CanReserve("host-3", offering1)).To(BeTrue()) // Reserved for host-3

			// Check capacity availability
			Expect(rm.CanReserve("host-4", offering1)).To(BeTrue()) // Should have 1 available (2 total, 1 used by host-3)
			Expect(rm.CanReserve("host-4", offering2)).To(BeTrue()) // Should have 1 available (3 total, 2 used)
			Expect(rm.CanReserve("host-4", offering3)).To(BeTrue()) // Should have 1 available (1 total, 0 used after release)
		})

		It("should handle hostname cleanup correctly", func() {
			// Reserve multiple offerings for a hostname
			rm.Reserve("host-1", offering1, offering2, offering3)

			// Release all offerings for the hostname
			rm.Release("host-1", offering1, offering2, offering3)

			// Verify capacity is fully restored
			Expect(rm.CanReserve("host-2", offering1)).To(BeTrue())
			Expect(rm.CanReserve("host-3", offering2)).To(BeTrue())
			Expect(rm.CanReserve("host-4", offering3)).To(BeTrue())

			// Verify we can use full capacity again
			rm.Reserve("host-2", offering1)
			rm.Reserve("host-3", offering1)
			Expect(rm.CanReserve("host-4", offering1)).To(BeFalse()) // Should be exhausted again
		})

		It("should handle the same hostname reserving multiple times", func() {
			rm.Reserve("host-1", offering1)
			rm.Reserve("host-1", offering1)
			rm.Reserve("host-1", offering1)
			rm.Reserve("host-1", offering1)
			rm.Reserve("host-1", offering1)
		})
	})
})
