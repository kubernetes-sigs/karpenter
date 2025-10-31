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

package state_test

import (
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("ClusterCost", func() {
	var clusterCost *state.ClusterCost
	var testNodePool *v1.NodePool
	var testNodePool2 *v1.NodePool
	var testInstanceType *cloudprovider.InstanceType
	var spotOffering *cloudprovider.Offering
	var onDemandOffering *cloudprovider.Offering
	BeforeEach(func() {
		testNodePool = test.NodePool(v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodepool"},
		})
		testNodePool2 = test.NodePool(v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodepool-2"},
		})

		spotOffering = &cloudprovider.Offering{
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:  v1.CapacityTypeSpot,
				corev1.LabelTopologyZone: "test-zone-1",
			}),
			Price:     1.50,
			Available: true,
		}

		onDemandOffering = &cloudprovider.Offering{
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
				corev1.LabelTopologyZone: "test-zone-1",
			}),
			Price:     3.00,
			Available: true,
		}

		testInstanceType = &cloudprovider.InstanceType{
			Name: "test-instance",
			Offerings: []*cloudprovider.Offering{
				spotOffering,
				onDemandOffering,
			},
		}
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{testInstanceType}
		// Initialize ClusterCost with required dependencies
		clusterCost = state.NewClusterCost(ctx, cloudProvider, env.Client)
		nodeOverlayController = nodeoverlay.NewController(env.Client, cloudProvider, nodeoverlay.NewInstanceTypeStore(), cluster, clusterCost)

		ExpectApplied(ctx, env.Client, testNodePool, testNodePool2)
	})

	Context("UpdateNodeClaim", func() {
		It("should update costs correctly for single spot offering to empty nodepool", func() {
			initialClusterCost := clusterCost.GetClusterCost()
			Expect(initialClusterCost).To(BeNumerically("~", 0.0, 0.001),
				"Cluster cost should start at 0")
			RunUpdateNodeClaimTest(
				clusterCost,
				testNodePool,
				testInstanceType.Name,
				spotOffering.CapacityType(),
				spotOffering.Zone(),
				1.50, // expectedNodePoolCost
				1.50, // expectedClusterCost
			)
		})
		It("should update costs correctly for single on-demand offering to empty nodepool", func() {
			// Setup: Empty cluster cost (no existing offerings)
			// No setup needed - starting with clean state
			RunUpdateNodeClaimTest(
				clusterCost,
				testNodePool,
				testInstanceType.Name,
				onDemandOffering.CapacityType(),
				onDemandOffering.Zone(),
				3.00, // expectedNodePoolCost
				3.00, // expectedClusterCost
			)
		})

		It("should update costs correctly for duplicate spot offering to increment count", func() {
			// Setup: Add initial spot offering to create baseline
			nodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			clusterCost.UpdateNodeClaim(ctx, nodeClaim)

			RunUpdateNodeClaimTest(
				clusterCost,
				testNodePool,
				testInstanceType.Name,
				spotOffering.CapacityType(),
				spotOffering.Zone(),
				3.00, // expectedNodePoolCost (2 * 1.50)
				3.00, // expectedClusterCost
			)
		})

		It("should update costs correctly for different offering types to same nodepool", func() {
			// Setup: Add spot offering first, then test will add on-demand offering
			nodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			clusterCost.UpdateNodeClaim(ctx, nodeClaim)

			RunUpdateNodeClaimTest(
				clusterCost,
				testNodePool,
				testInstanceType.Name,
				onDemandOffering.CapacityType(),
				onDemandOffering.Zone(),
				4.50, // expectedNodePoolCost (1.50 + 3.00)
				4.50, // expectedClusterCost
			)
		})

		It("should update costs correctly for offering to different nodepool", func() {
			// Setup: Add spot offering to first nodepool, then test will add on-demand to second nodepool
			nodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			clusterCost.UpdateNodeClaim(ctx, nodeClaim)

			RunUpdateNodeClaimTest(
				clusterCost,
				testNodePool2,
				testInstanceType.Name,
				onDemandOffering.CapacityType(),
				onDemandOffering.Zone(),
				3.00, // expectedNodePoolCost (second nodepool cost)
				4.50, // expectedClusterCost (1.50 + 3.00 total)
			)
		})
	})

	Context("DeleteNodeClaim", func() {
		It("should update costs correctly for single spot offering from nodepool with one offering", func() {
			// Setup: Add one spot offering to remove
			nodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			clusterCost.UpdateNodeClaim(ctx, nodeClaim)

			RunDeleteNodeClaimTest(
				clusterCost,
				nodeClaim,
				testNodePool,
				0.0, // expectedNodePoolCost
				0.0, // expectedClusterCost
			)
		})

		It("should update costs correctly for one instance of duplicate spot offering", func() {
			// Setup: Add same spot offering twice so removing one leaves one remaining
			nodeClaim1 := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			nodeClaim2 := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			nodeClaim2.Name = "test-nodeclaim-2"
			clusterCost.UpdateNodeClaim(ctx, nodeClaim1)
			clusterCost.UpdateNodeClaim(ctx, nodeClaim2)

			RunDeleteNodeClaimTest(
				clusterCost,
				nodeClaim1,
				testNodePool,
				1.50, // expectedNodePoolCost (one offering remains)
				1.50, // expectedClusterCost
			)
		})

		It("should update costs correctly for spot offering from nodepool with mixed offerings", func() {
			// Setup: Add both spot and on-demand offerings, test will remove spot
			spotNodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			onDemandNodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, onDemandOffering.CapacityType(), onDemandOffering.Zone())
			onDemandNodeClaim.Name = "test-nodeclaim-ondemand"
			clusterCost.UpdateNodeClaim(ctx, spotNodeClaim)
			clusterCost.UpdateNodeClaim(ctx, onDemandNodeClaim)

			RunDeleteNodeClaimTest(
				clusterCost,
				spotNodeClaim,
				testNodePool,
				3.00, // expectedNodePoolCost (only on-demand remains)
				3.00, // expectedClusterCost
			)
		})

		It("should update costs correctly for offering from one of multiple nodepools", func() {
			// Setup: Add spot offering to first nodepool and on-demand to second nodepool
			spotNodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			onDemandNodeClaim := createTestNodeClaim(testNodePool2, testInstanceType.Name, onDemandOffering.CapacityType(), onDemandOffering.Zone())
			onDemandNodeClaim.Name = "test-nodeclaim-ondemand"
			clusterCost.UpdateNodeClaim(ctx, spotNodeClaim)
			clusterCost.UpdateNodeClaim(ctx, onDemandNodeClaim)

			RunDeleteNodeClaimTest(
				clusterCost,
				spotNodeClaim,
				testNodePool,
				0.0, // expectedNodePoolCost (first nodepool now empty)
				3.0, // expectedClusterCost (only second nodepool remains)
			)
		})

		It("should update costs correctly for last offering completely removes nodepool entry", func() {
			// Setup: Add single spot offering to remove completely
			nodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			clusterCost.UpdateNodeClaim(ctx, nodeClaim)

			RunDeleteNodeClaimTest(
				clusterCost,
				nodeClaim,
				testNodePool,
				0.0, // expectedNodePoolCost
				0.0, // expectedClusterCost
			)
		})
	})

	Context("UpdateOfferings", func() {
		It("should recalculate cluster cost appropriately when nodeoverlays change", func() {
			GinkgoHelper()
			// Setup: Add initial offering to establish baseline cost
			nodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			clusterCost.UpdateNodeClaim(ctx, nodeClaim)

			// Verify initial cost
			initialCost := clusterCost.GetNodepoolCost(testNodePool)
			Expect(initialCost).To(BeNumerically("~", 1.50, 0.001))
			overlay := test.NodeOverlay(v1alpha1.NodeOverlay{
				Spec: v1alpha1.NodeOverlaySpec{
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      v1.NodePoolLabelKey,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{testNodePool.Name},
						},
					},
					PriceAdjustment: lo.ToPtr("+2.5"),
					Weight:          lo.ToPtr(int32(10000)),
				},
			})

			ExpectApplied(ctx, env.Client, overlay)
			ExpectReconcileSucceeded(ctx, nodeOverlayController, client.ObjectKeyFromObject(overlay))

			// Verify cost has been updated to reflect new price
			updatedCost := clusterCost.GetNodepoolCost(testNodePool)
			Expect(updatedCost).To(BeNumerically("~", 4.0, 0.001),
				"NodePool cost should be updated to reflect new offering price from nodeoverlay")

			// Verify cluster cost is also updated
			clusterCost := clusterCost.GetClusterCost()
			Expect(clusterCost).To(BeNumerically("~", 4.0, 0.001),
				"Cluster cost should be updated to reflect new offering price")
		})

		It("should recalculate cluster cost appropriately when cloud provider updates the price of offerings", func() {
			GinkgoHelper()
			// Setup: Add multiple nodeclaims across different nodepools to test comprehensive recalculation
			spotNodeClaim1 := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			onDemandNodeClaim1 := createTestNodeClaim(testNodePool, testInstanceType.Name, onDemandOffering.CapacityType(), onDemandOffering.Zone())
			onDemandNodeClaim1.Name = "test-nodeclaim-ondemand1"
			spotNodeClaim2 := createTestNodeClaim(testNodePool2, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
			spotNodeClaim2.Name = "test-nodeclaim-spot2"

			clusterCost.UpdateNodeClaim(ctx, spotNodeClaim1)
			clusterCost.UpdateNodeClaim(ctx, onDemandNodeClaim1)
			clusterCost.UpdateNodeClaim(ctx, spotNodeClaim2)

			// Verify initial costs
			initialNodePool1Cost := clusterCost.GetNodepoolCost(testNodePool)
			initialNodePool2Cost := clusterCost.GetNodepoolCost(testNodePool2)
			initialClusterCost := clusterCost.GetClusterCost()

			Expect(initialNodePool1Cost).To(BeNumerically("~", 4.50, 0.001)) // 1.50 + 3.00
			Expect(initialNodePool2Cost).To(BeNumerically("~", 1.50, 0.001)) // 1.50
			Expect(initialClusterCost).To(BeNumerically("~", 6.00, 0.001))   // 4.50 + 1.50

			// Simulate cloud provider price changes
			updatedSpotOffering := &cloudprovider.Offering{
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  v1.CapacityTypeSpot,
					corev1.LabelTopologyZone: "test-zone-1",
				}),
				Price:     2.50, // Changed from 1.50 to 2.50
				Available: true,
			}

			updatedInstanceType := &cloudprovider.InstanceType{
				Name: "test-instance",
				Offerings: []*cloudprovider.Offering{
					updatedSpotOffering,
					onDemandOffering,
				},
			}

			// Update cloudProvider with new prices
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{updatedInstanceType}

			// Call UpdateOfferings to trigger cost recalculation
			_ = clusterCost.UpdateOfferings(ctx, testNodePool, cloudProvider.InstanceTypes)
			_ = clusterCost.UpdateOfferings(ctx, testNodePool2, cloudProvider.InstanceTypes)

			// Verify costs have been updated across all nodepools
			finalNodePool1Cost := clusterCost.GetNodepoolCost(testNodePool)
			finalNodePool2Cost := clusterCost.GetNodepoolCost(testNodePool2)
			finalClusterCost := clusterCost.GetClusterCost()

			Expect(finalNodePool1Cost).To(BeNumerically("~", 5.50, 0.001), // 2.50 + 3.00
				"NodePool1 cost should reflect updated prices for both offerings")
			Expect(finalNodePool2Cost).To(BeNumerically("~", 2.50, 0.001), // 2.50
				"NodePool2 cost should reflect updated spot offering price")
			Expect(finalClusterCost).To(BeNumerically("~", 8.00, 0.001), // 5.50 + 2.50
				"Cluster cost should be sum of all updated nodepool costs")
		})
	})

	Context("Concurrency", func() {
		It("should be thread-safe with concurrent UpdateNodeClaim calls", func() {
			GinkgoHelper()
			numGoroutines := 5
			numOperationsPerGoroutine := 10
			var nodeClaims []*v1.NodeClaim

			// Pre-create nodeclaims for concurrent operations
			for i := 0; i < numGoroutines*numOperationsPerGoroutine; i++ {
				nodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
				nodeClaim.Name = fmt.Sprintf("test-nodeclaim-%d", i)
				nodeClaims = append(nodeClaims, nodeClaim)
			}

			var wg sync.WaitGroup
			wg.Add(numGoroutines)

			// Launch multiple goroutines that concurrently add nodeclaims
			for i := 0; i < numGoroutines; i++ {
				go func(goroutineIndex int) {
					defer wg.Done()
					for j := 0; j < numOperationsPerGoroutine; j++ {
						nodeClaimIndex := goroutineIndex*numOperationsPerGoroutine + j
						clusterCost.UpdateNodeClaim(ctx, nodeClaims[nodeClaimIndex])
					}
				}(i)
			}

			wg.Wait()

			// Verify final cost is correct (should be numGoroutines * numOperationsPerGoroutine * spotOffering.Price)
			expectedCost := float64(numGoroutines*numOperationsPerGoroutine) * spotOffering.Price
			finalCost := clusterCost.GetNodepoolCost(testNodePool)
			Expect(finalCost).To(BeNumerically("~", expectedCost, 0.001),
				"Final cost should be sum of all concurrent operations")
		})

		It("should be thread-safe with concurrent DeleteNodeClaim calls", func() {
			GinkgoHelper()
			numGoroutines := 25
			numOperationsPerGoroutine := 10
			totalOperations := numGoroutines * numOperationsPerGoroutine
			var nodeClaims []*v1.NodeClaim

			// Setup: Pre-populate with nodeclaims to remove
			for i := 0; i < totalOperations; i++ {
				nodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
				nodeClaim.Name = fmt.Sprintf("test-nodeclaim-%d", i)
				nodeClaims = append(nodeClaims, nodeClaim)
				clusterCost.UpdateNodeClaim(ctx, nodeClaim)
			}

			// Verify setup
			initialCost := clusterCost.GetNodepoolCost(testNodePool)
			expectedInitialCost := float64(totalOperations) * spotOffering.Price
			Expect(initialCost).To(BeNumerically("~", expectedInitialCost, 0.001))

			var wg sync.WaitGroup
			wg.Add(numGoroutines)

			// Launch multiple goroutines that concurrently remove nodeclaims
			for i := 0; i < numGoroutines; i++ {
				go func(goroutineIndex int) {
					defer wg.Done()
					for j := 0; j < numOperationsPerGoroutine; j++ {
						nodeClaimIndex := goroutineIndex*numOperationsPerGoroutine + j
						clusterCost.DeleteNodeClaim(ctx, nodeClaims[nodeClaimIndex])
					}
				}(i)
			}

			wg.Wait()

			// Verify final cost is zero after all removals
			finalCost := clusterCost.GetNodepoolCost(testNodePool)
			Expect(finalCost).To(BeNumerically("==", 0.0),
				"Final cost should be zero after removing all nodeclaims")
		})

		It("should handle concurrent updates while reading costs", func() {
			GinkgoHelper()
			numReaders := 2
			numWriters := 1
			operationsPerWriter := 10
			duration := time.Second * 2

			var wg sync.WaitGroup
			stopChan := make(chan struct{})
			var nodeClaims []*v1.NodeClaim

			// Pre-create nodeclaims for concurrent operations
			for i := 0; i < operationsPerWriter; i++ {
				nodeClaim := createTestNodeClaim(testNodePool, testInstanceType.Name, spotOffering.CapacityType(), spotOffering.Zone())
				nodeClaim.Name = fmt.Sprintf("test-nodeclaim-%d", i)
				nodeClaims = append(nodeClaims, nodeClaim)
			}

			// Launch reader goroutines that continuously read costs
			wg.Add(numReaders)
			for i := 0; i < numReaders; i++ {
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					for {
						select {
						case <-stopChan:
							return
						default:
							// Read costs continuously - these should never panic or return invalid values
							nodepoolCost := clusterCost.GetNodepoolCost(testNodePool)
							clusterCost := clusterCost.GetClusterCost()

							// Costs should always be non-negative
							Expect(nodepoolCost).To(BeNumerically(">=", 0.0))
							Expect(clusterCost).To(BeNumerically(">=", 0.0))
						}
					}
				}()
			}

			// Launch writer goroutines that add and remove nodeclaims
			wg.Add(numWriters)
			for i := 0; i < numWriters; i++ {
				go func(_ int) {
					defer GinkgoRecover()
					defer wg.Done()
					for j := 0; j < operationsPerWriter; j++ {
						// Randomly add or remove nodeclaims
						if j%2 == 0 {
							clusterCost.UpdateNodeClaim(ctx, nodeClaims[j])
						} else {
							// Only remove if we've added some nodeclaims
							if j > 0 {
								clusterCost.DeleteNodeClaim(ctx, nodeClaims[j-1])
							}
						}

						// Small delay to allow readers to interleave
						time.Sleep(time.Millisecond * 10)
					}
				}(i)
			}

			// Stop readers after duration
			go func() {
				time.Sleep(duration)
				close(stopChan)
			}()

			wg.Wait()

			// Verify that operations completed successfully without race conditions
			// The exact final cost depends on the timing of operations, but it should be valid
			finalCost := clusterCost.GetNodepoolCost(testNodePool)
			Expect(finalCost).To(BeNumerically(">=", 0.0),
				"Final cost should be non-negative after concurrent operations")
		})
	})
})

func createTestNodeClaim(np *v1.NodePool, instanceName, capacityType, zone string) *v1.NodeClaim {
	return test.NodeClaim(v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: test.RandomName(),
			Labels: map[string]string{
				v1.NodePoolLabelKey:            np.Name,
				corev1.LabelInstanceTypeStable: instanceName,
				v1.CapacityTypeLabelKey:        capacityType,
				corev1.LabelTopologyZone:       zone,
			},
		},
	})
}

func RunUpdateNodeClaimTest(clusterCost *state.ClusterCost, np *v1.NodePool, instanceName, capacityType, zone string, expectedNodePoolCost float64, expectedClusterCost float64) {
	GinkgoHelper()
	// Record initial costs before adding nodeclaim
	initialNodePoolCost := clusterCost.GetNodepoolCost(np)
	initialClusterCost := clusterCost.GetClusterCost()

	// Create and add the nodeclaim
	nodeClaim := createTestNodeClaim(np, instanceName, capacityType, zone)
	clusterCost.UpdateNodeClaim(ctx, nodeClaim)

	// Verify that nodepool cost has been updated correctly
	finalNodePoolCost := clusterCost.GetNodepoolCost(np)
	Expect(finalNodePoolCost).To(BeNumerically("~", expectedNodePoolCost, 0.001),
		"NodePool cost should match expected value after adding nodeclaim")

	// Verify that cluster cost has been updated correctly
	finalClusterCost := clusterCost.GetClusterCost()
	Expect(finalClusterCost).To(BeNumerically("~", expectedClusterCost, 0.001),
		"Cluster cost should match expected value after adding nodeclaim")

	// Verify that costs have increased (or stayed same if starting from expected value)
	Expect(finalNodePoolCost).To(BeNumerically(">=", initialNodePoolCost),
		"NodePool cost should not decrease when adding nodeclaims")
	Expect(finalClusterCost).To(BeNumerically(">=", initialClusterCost),
		"Cluster cost should not decrease when adding nodeclaims")
}

func RunDeleteNodeClaimTest(clusterCost *state.ClusterCost, nodeClaim *v1.NodeClaim, np *v1.NodePool, expectedNodePoolCost float64, expectedClusterCost float64) {
	GinkgoHelper()
	// Record initial costs before removing nodeclaim
	initialNodePoolCost := clusterCost.GetNodepoolCost(np)
	initialClusterCost := clusterCost.GetClusterCost()

	// Remove the nodeclaim
	clusterCost.DeleteNodeClaim(ctx, nodeClaim)

	// Verify that nodepool cost has been updated correctly
	finalNodePoolCost := clusterCost.GetNodepoolCost(np)
	Expect(finalNodePoolCost).To(BeNumerically("~", expectedNodePoolCost, 0.001),
		"NodePool cost should match expected value after removing nodeclaim")

	// Verify that cluster cost has been updated correctly
	finalClusterCost := clusterCost.GetClusterCost()
	Expect(finalClusterCost).To(BeNumerically("~", expectedClusterCost, 0.001),
		"Cluster cost should match expected value after removing nodeclaim")

	// Verify that costs have decreased (or stayed same if at zero)
	Expect(finalNodePoolCost).To(BeNumerically("<=", initialNodePoolCost),
		"NodePool cost should not increase when removing nodeclaims")
	Expect(finalClusterCost).To(BeNumerically("<=", initialClusterCost),
		"Cluster cost should not increase when removing nodeclaims")
}
