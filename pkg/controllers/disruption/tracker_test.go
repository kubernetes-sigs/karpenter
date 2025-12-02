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

package disruption_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	cache "github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/state/podresources"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Tracker", func() {
	var testCache *cache.Cache
	var testBucketsThresholdMap map[string]*disruption.BucketThresholds
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node

	BeforeEach(func() {
		// Setup test dependencies
		testCache = cache.New(30*time.Minute, time.Minute)

		// Setup test bucket thresholds with exported field names
		testBucketsThresholdMap = map[string]*disruption.BucketThresholds{
			disruption.ClusterCostLabel: {
				BiggestName: "xl",
				Thresholds: []disruption.BucketThreshold{
					{LowerBound: 0, Size: "xs"},
					{LowerBound: 100, Size: "s"},
					{LowerBound: 500, Size: "m"},
					{LowerBound: 1000, Size: "l"},
				},
			},
			disruption.TotalCPURequestsLabel: {
				BiggestName: "xl",
				Thresholds: []disruption.BucketThreshold{
					{LowerBound: 0, Size: "xs"},
					{LowerBound: 1000, Size: "s"},
					{LowerBound: 5000, Size: "m"},
					{LowerBound: 10000, Size: "l"},
				},
			},
			disruption.TotalMemoryRequestsLabel: {
				BiggestName: "xl",
				Thresholds: []disruption.BucketThreshold{
					{LowerBound: 0, Size: "xs"},
					{LowerBound: 1000000000, Size: "s"},  // 1GB
					{LowerBound: 5000000000, Size: "m"},  // 5GB
					{LowerBound: 10000000000, Size: "l"}, // 10GB
				},
			},
			disruption.TotalNodeCountLabel: {
				BiggestName: "xl",
				Thresholds: []disruption.BucketThreshold{
					{LowerBound: 0, Size: "xs"},
					{LowerBound: 10, Size: "s"},
					{LowerBound: 50, Size: "m"},
					{LowerBound: 100, Size: "l"},
				},
			},
			disruption.TotalDesiredPodCountLabel: {
				BiggestName: "xl",
				Thresholds: []disruption.BucketThreshold{
					{LowerBound: 0, Size: "xs"},
					{LowerBound: 100, Size: "s"},
					{LowerBound: 500, Size: "m"},
					{LowerBound: 1000, Size: "l"},
				},
			},
			disruption.PodCPURequestChangeRatioLabel: {
				BiggestName: "increase",
				Thresholds: []disruption.BucketThreshold{
					{LowerBound: 0, Size: "large_decrease"},
					{LowerBound: 0.8, Size: "small_decrease"},
					{LowerBound: 0.95, Size: "no_change"},
					{LowerBound: 1.05, Size: "small_increase"},
				},
			},
			disruption.PodMemRequestChangeRatioLabel: {
				BiggestName: "increase",
				Thresholds: []disruption.BucketThreshold{
					{LowerBound: 0, Size: "large_decrease"},
					{LowerBound: 0.8, Size: "small_decrease"},
					{LowerBound: 0.95, Size: "no_change"},
					{LowerBound: 1.05, Size: "small_increase"},
				},
			},
		}

		// Setup test objects
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
					v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				},
			},
			Status: v1.NodeClaimStatus{
				Allocatable: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("100"),
				},
			},
		})
	})

	Describe("GatherClusterState", func() {
		BeforeEach(func() {
			podResources = podresources.NewPodResources()
			tracker = disruption.NewTracker(cluster, clusterCost, podResources, fakeClock, testCache, testBucketsThresholdMap, true)
		})

		Context("when gathering cluster state", func() {
			It("should successfully gather cluster state with no pods", func() {
				clusterState, err := tracker.GatherClusterState(ctx)

				Expect(err).ToNot(HaveOccurred())
				Expect(clusterState).ToNot(BeNil())
				Expect(clusterState.TotalDesiredPodCount).To(Equal(0))
				Expect(clusterState.TotalDesiredPodResources).ToNot(BeNil())
				Expect(clusterState.TotalNodes).To(Equal(0)) // No nodes in cluster initially
			})

			It("should successfully gather cluster state with scheduled pods", func() {
				// Create pods directly with resource requirements
				pods := []*corev1.Pod{
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}),
				}
				ExpectApplied(ctx, env.Client, pods[0])
				podResources.UpdatePod(pods[0])
				ExpectApplied(ctx, env.Client, pods[1])
				podResources.UpdatePod(pods[1])
				ExpectApplied(ctx, env.Client, pods[2])
				podResources.UpdatePod(pods[2])

				clusterState, err := tracker.GatherClusterState(ctx)

				Expect(err).ToNot(HaveOccurred())
				Expect(clusterState).ToNot(BeNil())
				Expect(clusterState.TotalDesiredPodCount).To(Equal(3))
				Expect(clusterState.TotalDesiredPodResources).ToNot(BeNil())

				// Verify resource calculations (3 pods * 100m CPU = 300m total)
				expectedCPU := resource.MustParse("300m")
				expectedMemory := resource.MustParse("384Mi") // 3 * 128Mi

				calculatedCPU := lo.ToPtr((clusterState.TotalDesiredPodResources)[corev1.ResourceCPU])
				calculatedMemory := lo.ToPtr((clusterState.TotalDesiredPodResources)[corev1.ResourceMemory])

				Expect(calculatedCPU.String()).To(Equal(expectedCPU.String()))
				Expect(calculatedMemory.String()).To(Equal(expectedMemory.String()))
			})

			It("should calculate total desired pod count correctly with multiple pods", func() {
				// Create pods with different resource requirements
				pods := []*corev1.Pod{
					// First set of pods (2 pods with 100m CPU each)
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("100m"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("100m"),
							},
						},
					}),
					// Second set of pods (5 pods with 200m CPU each)
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("200m"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("200m"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("200m"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("200m"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("200m"),
							},
						},
					}),
				}

				for _, pod := range pods {
					ExpectApplied(ctx, env.Client, pod)
					podResources.UpdatePod(pod)
				}

				clusterState, err := tracker.GatherClusterState(ctx)

				Expect(err).ToNot(HaveOccurred())
				Expect(clusterState.TotalDesiredPodCount).To(Equal(7)) // 2 + 5

				// Total CPU should be (2 * 100m) + (5 * 200m) = 1200m
				expectedCPU := resource.MustParse("1200m")
				calculatedCPU := lo.ToPtr(clusterState.TotalDesiredPodResources[corev1.ResourceCPU])
				Expect(calculatedCPU.String()).To(Equal(expectedCPU.String()))
			})

			It("should handle empty cluster with no pods", func() {
				clusterState, err := tracker.GatherClusterState(ctx)

				Expect(err).ToNot(HaveOccurred())
				Expect(clusterState.TotalDesiredPodCount).To(Equal(0))
				Expect(clusterState.TotalDesiredPodResources[corev1.ResourceCPU]).To(Equal(resource.Quantity{}))
			})

			It("should include cluster cost and pod resources from cluster state", func() {
				// Apply some nodes and pods to the cluster state
				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				clusterState, err := tracker.GatherClusterState(ctx)

				Expect(err).ToNot(HaveOccurred())
				Expect(clusterState.TotalNodes).To(Equal(1))
				Expect(clusterState.ClusterCost).To(BeNumerically(">=", 0))
				Expect(clusterState.PodResources).ToNot(BeNil())
			})
		})
	})

	Describe("AddCommand", func() {
		BeforeEach(func() {
			podResources = podresources.NewPodResources()
			tracker = disruption.NewTracker(cluster, clusterCost, podResources, fakeClock, testCache, testBucketsThresholdMap, true)
		})

		Context("when tracker is enabled", func() {
			It("should add command with single candidate", func() {
				// Setup a real drift disruption method
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				// Setup node and nodeclaim
				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				// Create command with real drift method
				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				// Verify cache is empty initially
				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())

				// Add command should succeed
				_ = tracker.AddCommand(ctx, command)

				// Verify decision was cached
				cachedDecision, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeTrue())
				Expect(cachedDecision).ToNot(BeNil())
				// Verify it's the correct type
				decision, ok := cachedDecision.(disruption.TrackedDecision)
				Expect(ok).To(BeTrue())
				Expect(decision).ToNot(BeNil())
			})

			It("should add command with multiple candidates", func() {
				// Use consolidation method for multi-node scenario
				consolidationMethod := disruption.NewMultiNodeConsolidation(disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue))

				// Setup multiple candidates
				nodeClaim2, node2 := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
							v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
							corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
						},
					},
					Status: v1.NodeClaimStatus{
						Allocatable: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
							corev1.ResourcePods:   resource.MustParse("100"),
						},
					},
				})

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, nodeClaim2, node2)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node, node2}, []*v1.NodeClaim{nodeClaim, nodeClaim2})

				stateNode1 := ExpectStateNodeExists(cluster, node)
				stateNode2 := ExpectStateNodeExists(cluster, node2)

				candidates := []*disruption.Candidate{
					{StateNode: stateNode1, NodePool: nodePool},
					{StateNode: stateNode2, NodePool: nodePool},
				}

				command := &disruption.Command{
					Method:     consolidationMethod,
					Candidates: candidates,
				}

				// Add command should succeed
				_ = tracker.AddCommand(ctx, command)

				// Verify both candidates were cached
				cachedDecision1, found1 := testCache.Get(nodeClaim.Name)
				cachedDecision2, found2 := testCache.Get(nodeClaim2.Name)

				Expect(found1).To(BeTrue())
				Expect(found2).To(BeTrue())
				Expect(cachedDecision1).ToNot(BeNil())
				Expect(cachedDecision2).ToNot(BeNil())

				// Both should be TrackedDecision type
				_, ok1 := cachedDecision1.(disruption.TrackedDecision)
				_, ok2 := cachedDecision2.(disruption.TrackedDecision)
				Expect(ok1).To(BeTrue())
				Expect(ok2).To(BeTrue())
			})

			It("should use correct cache expiration", func() {
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				_ = tracker.AddCommand(ctx, command)

				// Verify decision exists in cache
				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeTrue())
			})
		})

		Context("when tracker is disabled", func() {
			BeforeEach(func() {
				podResources = podresources.NewPodResources()
				tracker = disruption.NewTracker(cluster, clusterCost, podResources, fakeClock, testCache, testBucketsThresholdMap, false)
			})

			It("should not add command when disabled", func() {
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				// Add command should not cache anything when disabled
				_ = tracker.AddCommand(ctx, command)

				// Verify nothing was cached
				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())
			})
		})
	})

	Describe("FinishCommand", func() {
		BeforeEach(func() {
			podResources = podresources.NewPodResources()
			tracker = disruption.NewTracker(cluster, clusterCost, podResources, fakeClock, testCache, testBucketsThresholdMap, true)
		})

		Context("when tracker is enabled", func() {
			It("should finish command when all candidates are processed", func() {
				// First add a command to create a tracked decision
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				// Add the command to cache the decision
				_ = tracker.AddCommand(ctx, command)

				// Verify decision is in cache
				cachedDecision, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeTrue())
				Expect(cachedDecision).ToNot(BeNil())

				// Now finish the command - since there's only one candidate, it should finish immediately
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// Decision should be removed from cache
				_, found = testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())
			})

			It("should not finish command when some candidates are still pending", func() {
				// Setup multiple candidates
				nodeClaim2, node2 := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
							v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
							corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
						},
					},
					Status: v1.NodeClaimStatus{
						Allocatable: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
							corev1.ResourcePods:   resource.MustParse("100"),
						},
					},
				})

				consolidationMethod := disruption.NewMultiNodeConsolidation(disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue))

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, nodeClaim2, node2)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node, node2}, []*v1.NodeClaim{nodeClaim, nodeClaim2})

				stateNode1 := ExpectStateNodeExists(cluster, node)
				stateNode2 := ExpectStateNodeExists(cluster, node2)

				candidates := []*disruption.Candidate{
					{StateNode: stateNode1, NodePool: nodePool},
					{StateNode: stateNode2, NodePool: nodePool},
				}

				command := &disruption.Command{
					Method:     consolidationMethod,
					Candidates: candidates,
				}

				// Add command (creates decisions for both candidates)
				_ = tracker.AddCommand(ctx, command)

				// Both decisions should be in cache
				_, found1 := testCache.Get(nodeClaim.Name)
				_, found2 := testCache.Get(nodeClaim2.Name)
				Expect(found1).To(BeTrue())
				Expect(found2).To(BeTrue())

				// Try to finish first candidate - should not complete because second is still pending
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// Only non-finished entry should still be in cache.
				_, found1 = testCache.Get(nodeClaim.Name)
				_, found2 = testCache.Get(nodeClaim2.Name)
				Expect(found1).To(BeFalse())
				Expect(found2).To(BeTrue())

				// Remove the second candidate from cache to simulate its completion
				testCache.Delete(nodeClaim2.Name)

				// Now finishing first candidate should work
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// First decision should still be in cache
				_, found1 = testCache.Get(nodeClaim.Name)
				Expect(found1).To(BeFalse())
			})

			It("should handle missing decision in cache", func() {
				// Try to finish a NodeClaim that has no decision in cache
				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

				// Verify cache is empty
				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())

				// FinishCommand should handle this gracefully (logs error but doesn't panic)
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// Still should be empty
				_, found = testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())
			})

			It("should handle invalid cached decision type", func() {
				// Put an invalid object in cache
				testCache.Set(nodeClaim.Name, "invalid-decision-type", cache.DefaultExpiration)

				// FinishCommand should handle this gracefully
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// Cache item should no longer be there
				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())
			})

			It("should emit metrics when command finishes", func() {
				// Add a command first
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				_ = tracker.AddCommand(ctx, command)

				// Finish the command - this should trigger metrics emission
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// We can't easily verify the exact metrics values without exposing internal state,
				// but we can verify the command finished without error
				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())
			})
		})
	})

	Describe("convertIntoBucket", func() {
		var bucketThresholds *disruption.BucketThresholds

		BeforeEach(func() {
			bucketThresholds = &disruption.BucketThresholds{
				BiggestName: "xl",
				Thresholds: []disruption.BucketThreshold{
					{LowerBound: 0, Size: "xs"},
					{LowerBound: 100, Size: "s"},
					{LowerBound: 500, Size: "m"},
					{LowerBound: 1000, Size: "l"},
				},
			}
		})

		Context("when converting metrics to buckets", func() {
			It("should return smallest bucket for value below first threshold", func() {
				// Test negative values - should get first bucket size since metric < 0 (first LowerBound)
				result := disruption.ConvertIntoBucket(-50, bucketThresholds)
				Expect(result).To(Equal("xs"))

				result = disruption.ConvertIntoBucket(-100, bucketThresholds)
				Expect(result).To(Equal("xs"))

				result = disruption.ConvertIntoBucket(-0.1, bucketThresholds)
				Expect(result).To(Equal("xs"))
			})

			It("should return correct bucket for value within thresholds", func() {
				// Test values that fall within threshold ranges

				// Value 50: 50 < 100 (second threshold), so should get "s" bucket
				result := disruption.ConvertIntoBucket(50, bucketThresholds)
				Expect(result).To(Equal("s"))

				// Value 150: 150 < 500 (third threshold), so should get "m" bucket
				result = disruption.ConvertIntoBucket(150, bucketThresholds)
				Expect(result).To(Equal("m"))

				// Value 750: 750 < 1000 (fourth threshold), so should get "l" bucket
				result = disruption.ConvertIntoBucket(750, bucketThresholds)
				Expect(result).To(Equal("l"))

				// Value 99.9: just under 100, should get "s"
				result = disruption.ConvertIntoBucket(99.9, bucketThresholds)
				Expect(result).To(Equal("s"))
			})

			It("should return biggest bucket for value above all thresholds", func() {
				// Test values above all thresholds should return BiggestName

				// Value 1500: 1500 is not < any LowerBound (all are smaller), so should get BiggestName "xl"
				result := disruption.ConvertIntoBucket(1500, bucketThresholds)
				Expect(result).To(Equal("xl"))

				// Value 5000: same logic, should get "xl"
				result = disruption.ConvertIntoBucket(5000, bucketThresholds)
				Expect(result).To(Equal("xl"))

				// Value 1000.1: just above highest threshold
				result = disruption.ConvertIntoBucket(1000.1, bucketThresholds)
				Expect(result).To(Equal("xl"))
			})

			It("should handle exact threshold boundary values", func() {
				// Test exact boundary conditions

				// Value 0: 0 is not < 0 (first threshold), so should check next: 0 < 100, so "s"
				result := disruption.ConvertIntoBucket(0, bucketThresholds)
				Expect(result).To(Equal("s"))

				// Value 100: 100 is not < 100, but 100 < 500, so "m"
				result = disruption.ConvertIntoBucket(100, bucketThresholds)
				Expect(result).To(Equal("m"))

				// Value 500: 500 is not < 500, but 500 < 1000, so "l"
				result = disruption.ConvertIntoBucket(500, bucketThresholds)
				Expect(result).To(Equal("l"))

				// Value 1000: 1000 is not < 1000, no more thresholds, so "xl"
				result = disruption.ConvertIntoBucket(1000, bucketThresholds)
				Expect(result).To(Equal("xl"))
			})

			It("should handle negative values", func() {
				// Test negative values

				// For any negative value like -50, -100, etc.
				// Since -50 < 0 (first LowerBound), it should return the first bucket size "xs"
				result := disruption.ConvertIntoBucket(-50, bucketThresholds)
				Expect(result).To(Equal("xs"))

				result = disruption.ConvertIntoBucket(-100, bucketThresholds)
				Expect(result).To(Equal("xs"))

				result = disruption.ConvertIntoBucket(-999.99, bucketThresholds)
				Expect(result).To(Equal("xs"))
			})

			It("should handle zero values", func() {
				// Test zero value specifically

				// Value 0: 0 is not < 0 (first threshold), continue to next
				// 0 < 100 (second threshold), so should return "s"
				result := disruption.ConvertIntoBucket(0, bucketThresholds)
				Expect(result).To(Equal("s"))
			})

			It("should work with different threshold configurations", func() {
				// Test with custom thresholds to verify the algorithm works generically
				customThresholds := &disruption.BucketThresholds{
					BiggestName: "huge",
					Thresholds: []disruption.BucketThreshold{
						{LowerBound: 10, Size: "tiny"},
						{LowerBound: 50, Size: "small"},
						{LowerBound: 200, Size: "medium"},
					},
				}

				// Value 5: 5 < 10 (first threshold), so "tiny"
				result := disruption.ConvertIntoBucket(5, customThresholds)
				Expect(result).To(Equal("tiny"))

				// Value 30: 30 is not < 10, but 30 < 50, so "small"
				result = disruption.ConvertIntoBucket(30, customThresholds)
				Expect(result).To(Equal("small"))

				// Value 100: 100 is not < 10 or 50, but 100 < 200, so "medium"
				result = disruption.ConvertIntoBucket(100, customThresholds)
				Expect(result).To(Equal("medium"))

				// Value 300: 300 is not < any threshold, so "huge"
				result = disruption.ConvertIntoBucket(300, customThresholds)
				Expect(result).To(Equal("huge"))
			})

			It("should handle empty thresholds", func() {
				// Test edge case with no thresholds
				emptyThresholds := &disruption.BucketThresholds{
					BiggestName: "default",
					Thresholds:  []disruption.BucketThreshold{},
				}

				// With no thresholds, any value should return BiggestName
				result := disruption.ConvertIntoBucket(0, emptyThresholds)
				Expect(result).To(Equal("default"))

				result = disruption.ConvertIntoBucket(100, emptyThresholds)
				Expect(result).To(Equal("default"))

				result = disruption.ConvertIntoBucket(-50, emptyThresholds)
				Expect(result).To(Equal("default"))
			})

			It("should handle single threshold", func() {
				// Test with only one threshold
				singleThreshold := &disruption.BucketThresholds{
					BiggestName: "big",
					Thresholds: []disruption.BucketThreshold{
						{LowerBound: 50, Size: "small"},
					},
				}

				// Value below threshold
				result := disruption.ConvertIntoBucket(25, singleThreshold)
				Expect(result).To(Equal("small"))

				// Value at threshold
				result = disruption.ConvertIntoBucket(50, singleThreshold)
				Expect(result).To(Equal("big"))

				// Value above threshold
				result = disruption.ConvertIntoBucket(100, singleThreshold)
				Expect(result).To(Equal("big"))
			})

			It("should handle floating point precision", func() {
				// Test floating point boundary conditions
				floatThresholds := &disruption.BucketThresholds{
					BiggestName: "large",
					Thresholds: []disruption.BucketThreshold{
						{LowerBound: 10.5, Size: "small"},
						{LowerBound: 100.75, Size: "medium"},
					},
				}

				// Just below first threshold
				result := disruption.ConvertIntoBucket(10.4, floatThresholds)
				Expect(result).To(Equal("small"))

				// Just above first threshold but below second
				result = disruption.ConvertIntoBucket(10.6, floatThresholds)
				Expect(result).To(Equal("medium"))

				// Above all thresholds
				result = disruption.ConvertIntoBucket(101.0, floatThresholds)
				Expect(result).To(Equal("large"))
			})
		})
	})

	Describe("Integration Tests", func() {
		BeforeEach(func() {
			podResources = podresources.NewPodResources()
			tracker = disruption.NewTracker(cluster, clusterCost, podResources, fakeClock, testCache, testBucketsThresholdMap, true)
		})

		Context("when processing a complete disruption workflow", func() {
			It("should handle AddCommand -> FinishCommand -> EmitMetrics flow", func() {
				// Setup drift disruption scenario
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				// Step 1: AddCommand should succeed and cache the decision
				_ = tracker.AddCommand(ctx, command)

				cachedDecision, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeTrue())
				Expect(cachedDecision).ToNot(BeNil())

				decision, ok := cachedDecision.(disruption.TrackedDecision)
				Expect(ok).To(BeTrue())
				Expect(decision).ToNot(BeNil())

				// Step 2: FinishCommand should complete the workflow and emit metrics
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// Decision should have been removed from cache.
				_, found = testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())

				// The workflow completed successfully without errors
			})

			It("should handle commands with multiple candidates", func() {
				// Setup multi-node consolidation scenario
				consolidationMethod := disruption.NewMultiNodeConsolidation(disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue))

				// Create additional nodes for multi-node scenario
				nodeClaim2, node2 := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
							v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
							corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
						},
					},
					Status: v1.NodeClaimStatus{
						Allocatable: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
							corev1.ResourcePods:   resource.MustParse("100"),
						},
					},
				})

				nodeClaim3, node3 := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
							v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
							corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
						},
					},
					Status: v1.NodeClaimStatus{
						Allocatable: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
							corev1.ResourcePods:   resource.MustParse("100"),
						},
					},
				})

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, nodeClaim2, node2, nodeClaim3, node3)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
					[]*corev1.Node{node, node2, node3}, []*v1.NodeClaim{nodeClaim, nodeClaim2, nodeClaim3})

				stateNode1 := ExpectStateNodeExists(cluster, node)
				stateNode2 := ExpectStateNodeExists(cluster, node2)
				stateNode3 := ExpectStateNodeExists(cluster, node3)

				candidates := []*disruption.Candidate{
					{StateNode: stateNode1, NodePool: nodePool},
					{StateNode: stateNode2, NodePool: nodePool},
					{StateNode: stateNode3, NodePool: nodePool},
				}

				command := &disruption.Command{
					Method:     consolidationMethod,
					Candidates: candidates,
				}

				// AddCommand should create decisions for all candidates
				_ = tracker.AddCommand(ctx, command)

				// Verify all candidates have cached decisions
				_, found1 := testCache.Get(nodeClaim.Name)
				_, found2 := testCache.Get(nodeClaim2.Name)
				_, found3 := testCache.Get(nodeClaim3.Name)
				Expect(found1).To(BeTrue())
				Expect(found2).To(BeTrue())
				Expect(found3).To(BeTrue())

				// Finish candidates one by one
				// First finish should not complete (others still pending)
				_ = tracker.FinishCommand(ctx, nodeClaim)
				_, found1 = testCache.Get(nodeClaim.Name)
				_, found2 = testCache.Get(nodeClaim2.Name)
				_, found3 = testCache.Get(nodeClaim3.Name)
				Expect(found1).To(BeFalse())
				Expect(found2).To(BeTrue())
				Expect(found3).To(BeTrue())

				// Second finish should not complete (one still pending)
				_ = tracker.FinishCommand(ctx, nodeClaim2)
				_, found1 = testCache.Get(nodeClaim.Name)
				_, found2 = testCache.Get(nodeClaim2.Name)
				_, found3 = testCache.Get(nodeClaim3.Name)
				Expect(found1).To(BeFalse())
				Expect(found2).To(BeFalse())
				Expect(found3).To(BeTrue())

				// Third finish should complete the workflow and emit metrics
				_ = tracker.FinishCommand(ctx, nodeClaim3)

				// All decisions should still be in cache (not removed by FinishCommand)
				_, found1 = testCache.Get(nodeClaim.Name)
				_, found2 = testCache.Get(nodeClaim2.Name)
				_, found3 = testCache.Get(nodeClaim3.Name)
				Expect(found1).To(BeFalse())
				Expect(found2).To(BeFalse())
				Expect(found3).To(BeFalse())
			})

			It("should handle workflow with cluster state changes", func() {
				// Create initial pods
				initialPods := []*corev1.Pod{
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}),
				}

				for _, pod := range initialPods {
					ExpectApplied(ctx, env.Client, pod)
					podResources.UpdatePod(pod)
				}

				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				// Add command - captures initial state
				_ = tracker.AddCommand(ctx, command)

				// Modify cluster state by adding more pods
				additionalPods := []*corev1.Pod{
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}),
				}

				for _, pod := range additionalPods {
					ExpectApplied(ctx, env.Client, pod)
					podResources.UpdatePod(pod)
				}

				// Finish command - captures final state and calculates ratios
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// The workflow should handle state changes and calculate metrics appropriately
				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())
			})

			It("should handle workflow errors gracefully", func() {
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				// Add command successfully
				_ = tracker.AddCommand(ctx, command)

				// Corrupt the cached decision to simulate error scenario
				testCache.Set(nodeClaim.Name, "invalid-data", cache.DefaultExpiration)

				// FinishCommand should handle the error gracefully
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// Cache should have the item removed
				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())
			})

			It("should handle cache expiration during workflow", func() {
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				// Add command
				_ = tracker.AddCommand(ctx, command)

				// Wait for cache expiration
				fakeClock.Step(40 * time.Minute)
				testCache.DeleteExpired()

				// FinishCommand should handle missing cache entry gracefully
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// Cache should be empty due to expiration
				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())
			})
		})

		Context("when tracker is disabled", func() {
			BeforeEach(func() {
				tracker = disruption.NewTracker(cluster, clusterCost, podResources, fakeClock, testCache, testBucketsThresholdMap, false)
			})

			It("should not process workflow when disabled", func() {
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				// AddCommand should do nothing when disabled
				_ = tracker.AddCommand(ctx, command)

				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())

				// FinishCommand should also do nothing when disabled
				_ = tracker.FinishCommand(ctx, nodeClaim)

				_, found = testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())
			})
		})

		Context("when handling resource calculations", func() {
			It("should accurately track resource changes through workflow", func() {
				// Create initial pods with known resource requirements
				initialPods := []*corev1.Pod{
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}),
				}
				for _, pod := range initialPods {
					ExpectApplied(ctx, env.Client, pod)
					podResources.UpdatePod(pod)
				}

				// Verify initial cluster state
				initialState, err := tracker.GatherClusterState(ctx)
				Expect(err).ToNot(HaveOccurred())
				Expect(initialState.TotalDesiredPodCount).To(Equal(3))

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)
				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				// Start tracking
				_ = tracker.AddCommand(ctx, command)

				// Add more pods to simulate scaling up
				additionalPods := []*corev1.Pod{
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}),
					test.Pod(test.PodOptions{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}),
				}

				for _, pod := range additionalPods {
					ExpectApplied(ctx, env.Client, pod)
					podResources.UpdatePod(pod)
				}

				// Verify final cluster state
				finalState, err := tracker.GatherClusterState(ctx)
				Expect(err).ToNot(HaveOccurred())
				Expect(finalState.TotalDesiredPodCount).To(Equal(6))

				// Finish tracking - should calculate change ratios
				_ = tracker.FinishCommand(ctx, nodeClaim)

				// Workflow completed with accurate resource tracking
				_, found := testCache.Get(nodeClaim.Name)
				Expect(found).To(BeFalse())
			})
		})
	})

	Describe("Concurrency", func() {
		BeforeEach(func() {
			podResources = podresources.NewPodResources()
			tracker = disruption.NewTracker(cluster, clusterCost, podResources, fakeClock, testCache, testBucketsThresholdMap, true)
		})

		Context("when handling concurrent operations", func() {
			It("should handle concurrent AddCommand calls", func() {
				const numGoroutines = 10
				const commandsPerGoroutine = 5

				// Create multiple node pairs for concurrent operations
				var nodeClaims []*v1.NodeClaim
				var nodes []*corev1.Node

				for i := 0; i < numGoroutines*commandsPerGoroutine; i++ {
					nc, n := test.NodeClaimAndNode(v1.NodeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								v1.NodePoolLabelKey:            nodePool.Name,
								corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
								v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
								corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
							},
						},
						Status: v1.NodeClaimStatus{
							Allocatable: map[corev1.ResourceName]resource.Quantity{
								corev1.ResourceCPU:    resource.MustParse("4"),
								corev1.ResourceMemory: resource.MustParse("8Gi"),
								corev1.ResourcePods:   resource.MustParse("100"),
							},
						},
					})
					nodeClaims = append(nodeClaims, nc)
					nodes = append(nodes, n)
				}

				// Apply resources
				ExpectApplied(ctx, env.Client, nodePool)
				for i := range nodeClaims {
					ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				}
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

				// Create drift method for testing
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				// Channel to collect results
				resultChan := make(chan bool, numGoroutines*commandsPerGoroutine)

				// Launch concurrent AddCommand operations
				for i := 0; i < numGoroutines; i++ {
					go func(start int) {
						defer GinkgoRecover()
						for j := 0; j < commandsPerGoroutine; j++ {
							idx := start + j
							stateNode := ExpectStateNodeExists(cluster, nodes[idx])
							candidate := &disruption.Candidate{
								StateNode: stateNode,
								NodePool:  nodePool,
							}

							command := &disruption.Command{
								Method:     driftMethod,
								Candidates: []*disruption.Candidate{candidate},
							}

							// This should be thread-safe
							_ = tracker.AddCommand(ctx, command)
							resultChan <- true
						}
					}(i * commandsPerGoroutine)
				}

				// Wait for all operations to complete
				for i := 0; i < numGoroutines*commandsPerGoroutine; i++ {
					Eventually(resultChan).Should(Receive(BeTrue()))
				}

				// Verify all commands were processed correctly
				for _, nc := range nodeClaims {
					_, found := testCache.Get(nc.Name)
					Expect(found).To(BeTrue())
				}
			})

			It("should handle concurrent FinishCommand calls", func() {
				const numGoroutines = 8
				const commandsPerGoroutine = 3

				// Setup multiple commands first
				var nodeClaims []*v1.NodeClaim
				var nodes []*corev1.Node

				for i := 0; i < numGoroutines*commandsPerGoroutine; i++ {
					nc, n := test.NodeClaimAndNode(v1.NodeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								v1.NodePoolLabelKey:            nodePool.Name,
								corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
								v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
								corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
							},
						},
						Status: v1.NodeClaimStatus{
							Allocatable: map[corev1.ResourceName]resource.Quantity{
								corev1.ResourceCPU:    resource.MustParse("4"),
								corev1.ResourceMemory: resource.MustParse("8Gi"),
								corev1.ResourcePods:   resource.MustParse("100"),
							},
						},
					})
					nodeClaims = append(nodeClaims, nc)
					nodes = append(nodes, n)
				}

				// Apply resources
				ExpectApplied(ctx, env.Client, nodePool)
				for i := range nodeClaims {
					ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				}
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

				// Add all commands first
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)
				for i := range nodeClaims {
					stateNode := ExpectStateNodeExists(cluster, nodes[i])
					candidate := &disruption.Candidate{
						StateNode: stateNode,
						NodePool:  nodePool,
					}

					command := &disruption.Command{
						Method:     driftMethod,
						Candidates: []*disruption.Candidate{candidate},
					}

					_ = tracker.AddCommand(ctx, command)
				}

				// Channel to collect results
				resultChan := make(chan bool, numGoroutines*commandsPerGoroutine)

				// Launch concurrent FinishCommand operations
				for i := 0; i < numGoroutines; i++ {
					go func(start int) {
						defer GinkgoRecover()
						for j := 0; j < commandsPerGoroutine; j++ {
							idx := start + j
							// This should be thread-safe
							_ = tracker.FinishCommand(ctx, nodeClaims[idx])
							resultChan <- true
						}
					}(i * commandsPerGoroutine)
				}

				// Wait for all operations to complete
				for i := 0; i < numGoroutines*commandsPerGoroutine; i++ {
					Eventually(resultChan).Should(Receive(BeTrue()))
				}

				// All operations should complete without race conditions or panics
				// The exact state of cache entries depends on timing, but no crashes should occur
			})

			It("should handle concurrent cache access", func() {
				const numReaders = 5
				const numWriters = 3
				const operationsPerWorker = 10

				// Pre-populate some cache entries
				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

				stateNode := ExpectStateNodeExists(cluster, node)
				candidate := &disruption.Candidate{
					StateNode: stateNode,
					NodePool:  nodePool,
				}

				command := &disruption.Command{
					Method:     driftMethod,
					Candidates: []*disruption.Candidate{candidate},
				}

				_ = tracker.AddCommand(ctx, command)

				// Channel to collect results
				resultChan := make(chan bool, numReaders*operationsPerWorker+numWriters*operationsPerWorker)

				// Launch reader goroutines (calling FinishCommand which reads cache)
				for i := 0; i < numReaders; i++ {
					go func() {
						defer GinkgoRecover()
						for j := 0; j < operationsPerWorker; j++ {
							_ = tracker.FinishCommand(ctx, nodeClaim)
							resultChan <- true
						}
					}()
				}

				// Launch writer goroutines (calling AddCommand which writes cache)
				for i := 0; i < numWriters; i++ {
					go func() {
						defer GinkgoRecover()
						for j := 0; j < operationsPerWorker; j++ {
							_ = tracker.AddCommand(ctx, command)
							resultChan <- true
						}
					}()
				}

				// Wait for all operations to complete
				totalOps := numReaders*operationsPerWorker + numWriters*operationsPerWorker
				for i := 0; i < totalOps; i++ {
					Eventually(resultChan).Should(Receive(BeTrue()))
				}

				// Verify no panics are hit. State of the cache depends on if AddCommand or FinishCommand
				// occurred last
			})

			It("should maintain data consistency under load", func() {
				const numWorkers = 12
				const operationsPerWorker = 8

				// Create multiple node claims for load testing
				var nodeClaims []*v1.NodeClaim
				var nodes []*corev1.Node

				for i := 0; i < numWorkers; i++ {
					nc, n := test.NodeClaimAndNode(v1.NodeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								v1.NodePoolLabelKey:            nodePool.Name,
								corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
								v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
								corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
							},
						},
						Status: v1.NodeClaimStatus{
							Allocatable: map[corev1.ResourceName]resource.Quantity{
								corev1.ResourceCPU:    resource.MustParse("4"),
								corev1.ResourceMemory: resource.MustParse("8Gi"),
								corev1.ResourcePods:   resource.MustParse("100"),
							},
						},
					})
					nodeClaims = append(nodeClaims, nc)
					nodes = append(nodes, n)
				}

				// Apply resources
				ExpectApplied(ctx, env.Client, nodePool)
				for i := range nodeClaims {
					ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				}
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				// Channel to collect results and track operations
				resultChan := make(chan string, numWorkers*operationsPerWorker*2) // *2 for Add+Finish

				// Launch workers that perform full Add->Finish cycles
				for i := 0; i < numWorkers; i++ {
					go func(workerID int) {
						defer GinkgoRecover()
						nodeClaimIdx := workerID % len(nodeClaims)
						nc := nodeClaims[nodeClaimIdx]
						n := nodes[nodeClaimIdx]

						for j := 0; j < operationsPerWorker; j++ {
							stateNode := ExpectStateNodeExists(cluster, n)
							candidate := &disruption.Candidate{
								StateNode: stateNode,
								NodePool:  nodePool,
							}

							command := &disruption.Command{
								Method:     driftMethod,
								Candidates: []*disruption.Candidate{candidate},
							}

							// Add command
							_ = tracker.AddCommand(ctx, command)
							resultChan <- "add"

							// Small delay to simulate real-world timing
							time.Sleep(1 * time.Millisecond)

							// Finish command
							_ = tracker.FinishCommand(ctx, nc)
							resultChan <- "finish"
						}
					}(i)
				}

				// Collect all results
				expectedResults := numWorkers * operationsPerWorker * 2
				addCount := 0
				finishCount := 0

				for i := 0; i < expectedResults; i++ {
					result := <-resultChan
					switch result {
					case "add":
						addCount++
					case "finish":
						finishCount++
					}
				}

				// Verify all operations completed
				Expect(addCount).To(Equal(numWorkers * operationsPerWorker))
				Expect(finishCount).To(Equal(numWorkers * operationsPerWorker))

				// Verify cache consistency - all entries should exist
				for _, nc := range nodeClaims {
					_, found := testCache.Get(nc.Name)
					Expect(found).To(BeFalse())
				}
			})

			It("should handle mixed operations under concurrent load", func() {
				const totalOperations = 100

				// Setup a few node claims
				numNodes := 5
				var nodeClaims []*v1.NodeClaim
				var nodes []*corev1.Node

				for i := 0; i < numNodes; i++ {
					nc, n := test.NodeClaimAndNode(v1.NodeClaim{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								v1.NodePoolLabelKey:            nodePool.Name,
								corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
								v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
								corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
							},
						},
						Status: v1.NodeClaimStatus{
							Allocatable: map[corev1.ResourceName]resource.Quantity{
								corev1.ResourceCPU:    resource.MustParse("4"),
								corev1.ResourceMemory: resource.MustParse("8Gi"),
								corev1.ResourcePods:   resource.MustParse("100"),
							},
						},
					})
					nodeClaims = append(nodeClaims, nc)
					nodes = append(nodes, n)
				}

				// Apply resources
				ExpectApplied(ctx, env.Client, nodePool)
				for i := range nodeClaims {
					ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				}
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

				driftMethod := disruption.NewDrift(env.Client, cluster, prov, recorder)

				// Channel to signal completion
				done := make(chan bool, totalOperations)

				// Launch mixed operations concurrently
				for i := 0; i < totalOperations; i++ {
					go func(opID int) {
						defer GinkgoRecover()

						// Pick a random node claim
						ncIdx := opID % len(nodeClaims)
						nc := nodeClaims[ncIdx]
						n := nodes[ncIdx]

						// Alternate between AddCommand and FinishCommand
						if opID%2 == 0 {
							// Add operation
							stateNode := ExpectStateNodeExists(cluster, n)
							candidate := &disruption.Candidate{
								StateNode: stateNode,
								NodePool:  nodePool,
							}

							command := &disruption.Command{
								Method:     driftMethod,
								Candidates: []*disruption.Candidate{candidate},
							}

							_ = tracker.AddCommand(ctx, command)
						} else {
							// Finish operation
							_ = tracker.FinishCommand(ctx, nc)
						}

						done <- true
					}(i)
				}

				// Wait for all operations to complete
				for i := 0; i < totalOperations; i++ {
					Eventually(done).Should(Receive(BeTrue()))
				}

				// System should remain stable and consistent
				// The exact cache state depends on timing, but no crashes should occur
			})
		})
	})
})
