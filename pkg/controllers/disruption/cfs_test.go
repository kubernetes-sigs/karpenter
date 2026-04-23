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

// Tests for the CFS-based time-slicing disruption loop introduced in RFC #2927.
//
// Phase 1 (Emptiness, StaticDrift, weight=0) runs unconditionally every cycle.
// Phase 2 (Drift weight=3, Multi weight=2, Single weight=1) is CFS-scheduled:
// each method tracks druntime; lowest druntime gets the next turn, and higher
// weight means druntime grows slower so the method gets more turns.
//
// Drift runs inside a time-capped loop so it can process multiple candidates in
// one cycle.  The loop stops immediately after any disruption that creates
// replacement NodeClaims (hadReplacements=true) to avoid double-booking pods
// from nodes whose replacements haven't yet appeared in cluster state.

import (
	"time"

	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("CFS Time-Slicing", func() {
	// -------------------------------------------------------------------------
	// Weight constants
	// -------------------------------------------------------------------------
	Describe("Method weights", func() {
		It("should assign weight 0 to Phase 1 methods (Emptiness, StaticDrift)", func() {
			c := disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			Expect(disruption.NewEmptiness(c).Weight()).To(BeEquivalentTo(0))
			Expect(disruption.NewStaticDrift(cluster, prov, cloudProvider).Weight()).To(BeEquivalentTo(0))
		})

		It("should assign decreasing weights to Phase 2 methods (Drift > Multi > Single)", func() {
			c := disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			driftW := disruption.NewDrift(env.Client, cluster, prov, recorder, fakeClock).Weight()
			multiW := disruption.NewMultiNodeConsolidation(c).Weight()
			singleW := disruption.NewSingleNodeConsolidation(c).Weight()
			Expect(driftW).To(BeNumerically(">", multiW), "Drift weight should exceed Multi")
			Expect(multiW).To(BeNumerically(">", singleW), "Multi weight should exceed Single")
			Expect(singleW).To(BeNumerically(">", 0), "Single weight should be positive (Phase 2)")
		})
	})

	// -------------------------------------------------------------------------
	// Time-cap loop – empty-node batching
	// -------------------------------------------------------------------------
	Describe("Time-cap loop (Drift)", func() {
		var nodePool *v1.NodePool

		BeforeEach(func() {
			nodePool = test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidateAfter: v1.MustParseNillableDuration("Never"),
						Budgets:          []v1.Budget{{Nodes: "100%"}},
					},
				},
			})
		})

		It("should process all empty drifted nodes in a single reconcile cycle", func() {
			// Build 5 empty drifted nodes.  Empty candidates require no replacement
			// NodeClaims, so hadReplacements stays false and the loop keeps iterating.
			const numNodes = 5
			nodeClaims, nodes := test.NodeClaimsAndNodes(numNodes, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					// Do NOT set ProviderID here — test.NodeClaimsAndNodes calls NodeClaimAndNode
					// for each index, so a ProviderID set in the template would be shared across
					// all five NodeClaims (the struct literal is evaluated once).  Leaving it empty
					// lets test.NodeClaim auto-generate a unique ProviderID per node.
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			for _, nc := range nodeClaims {
				nc.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			}
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			// Mark cluster as synced so the reconcile does not return early due to stale NodeClaims
			// from previous tests still present in the API server informer cache.
			cluster.SetSynced(true)

			// Single reconcile should taint all 5 nodes via the time-cap loop.
			ExpectSingletonReconciled(ctx, disruptionController)

			ExpectTaintedNodeCount(ctx, env.Client, numNodes)
			// No replacement NodeClaims created (all empty → delete commands).
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(numNodes))
		})

		It("should process only one non-empty drifted node per reconcile cycle", func() {
			// Two non-empty drifted nodes.  The first disruption creates a replacement
			// NodeClaim (hadReplacements=true), stopping the loop immediately.
			// The second node is only disrupted in the next reconcile cycle.

			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			makePod := func() *corev1.Pod {
				return test.Pod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         boolPtr(true),
							BlockOwnerDeletion: boolPtr(true),
						}},
					},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							// Large CPU so neither pod can consolidate onto the other's node.
							corev1.ResourceCPU: resource.MustParse("30"),
						},
					},
				})
			}

			makeNC := func(driftTime metav1.Time) (*v1.NodeClaim, *corev1.Node) {
				nc, n := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
							v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
							corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
						},
					},
					Status: v1.NodeClaimStatus{
						ProviderID:  test.RandomProviderID(),
						Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32")},
					},
				})
				nc.Status.Conditions = append(nc.Status.Conditions, status.Condition{
					Type:               v1.ConditionTypeDrifted,
					Status:             metav1.ConditionTrue,
					Reason:             v1.ConditionTypeDrifted,
					Message:            v1.ConditionTypeDrifted,
					LastTransitionTime: driftTime,
				})
				return nc, n
			}

			// nodeClaim2 drifted 1 hour earlier → processed first.
			nc1, node1 := makeNC(metav1.Now())
			nc2, node2 := makeNC(metav1.Time{Time: fakeClock.Now().Add(-time.Hour)})
			pod1 := makePod()
			pod2 := makePod()

			// Limit to a budget that would allow both nodes to be disrupted, so we can
			// confirm the loop stops after the first replacement (not due to budget).
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "2"}}
			ExpectApplied(ctx, env.Client, rs, pod1, pod2, nc1, node1, nc2, node2, nodePool)
			ExpectManualBinding(ctx, env.Client, pod1, node1)
			ExpectManualBinding(ctx, env.Client, pod2, node2)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{node1, node2}, []*v1.NodeClaim{nc1, nc2})
			cluster.SetSynced(true)

			// First reconcile: hadReplacements stops the loop after nc2 (earliest drift).
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1), "only one non-empty node should be disrupted per cycle")
			Expect(cmds[0].Candidates[0].NodeClaim.Name).To(Equal(nc2.Name))

			// Deploy replacement and confirm node2 is deleted.
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, cluster, cloudProvider, cmds[0])
			// Fetch the replacement NC/node so we can update cluster state for them.
			// ExpectMakeNewNodeClaimsReady sets the initialized label/conditions in the API
			// server but calls cluster.UpdateNode before those labels are applied, leaving
			// the replacement uninitialized in cluster state.  The state-controller run
			// below propagates the initialized labels into the in-memory cluster state so
			// that the second reconcile's scheduling simulation does not see the replacement
			// as an uninitialized existing node and block nc1's disruption.
			replacementNC := ExpectExists(ctx, env.Client, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: cmds[0].Replacements[0].Name}})
			replacementNode := ExpectExists(ctx, env.Client, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: replacementNC.Status.NodeName}})
			ExpectObjectReconciled(ctx, env.Client, queue, nc2)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nc2)
			ExpectNotFound(ctx, env.Client, nc2, node2)
			ExpectExists(ctx, env.Client, nc1)

			// Second reconcile: nc1 is now disrupted.
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{node1, replacementNode}, []*v1.NodeClaim{nc1, replacementNC})
			ExpectSingletonReconciled(ctx, disruptionController)
			ExpectTaintedNodeCount(ctx, env.Client, 1)
		})

		It("should process empty drifted nodes first and then the first non-empty node in one cycle", func() {
			// 2 empty + 1 non-empty.  The loop should delete both empty nodes
			// (hadReplacements=false each time) and then issue a replace for the
			// non-empty node (hadReplacements=true), stopping after it.

			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         boolPtr(true),
						BlockOwnerDeletion: boolPtr(true),
					}},
				},
			})

			makeEmptyNC := func() (*v1.NodeClaim, *corev1.Node) {
				nc, n := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
							v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
							corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
						},
					},
					Status: v1.NodeClaimStatus{
						ProviderID: test.RandomProviderID(),
						Allocatable: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:  resource.MustParse("32"),
							corev1.ResourcePods: resource.MustParse("100"),
						},
					},
				})
				nc.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
				return nc, n
			}

			ncEmpty1, nodeEmpty1 := makeEmptyNC()
			ncEmpty2, nodeEmpty2 := makeEmptyNC()
			ncNonEmpty, nodeNonEmpty := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			ncNonEmpty.StatusConditions().SetTrue(v1.ConditionTypeDrifted)

			ExpectApplied(ctx, env.Client, rs, pod, ncEmpty1, nodeEmpty1, ncEmpty2, nodeEmpty2, ncNonEmpty, nodeNonEmpty, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, nodeNonEmpty)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{nodeEmpty1, nodeEmpty2, nodeNonEmpty},
				[]*v1.NodeClaim{ncEmpty1, ncEmpty2, ncNonEmpty})
			cluster.SetSynced(true)

			// One reconcile should handle all three disruptions.
			ExpectSingletonReconciled(ctx, disruptionController)

			// 2 empty deletes + 1 replace = 3 tainted, 1 replacement NodeClaim added.
			ExpectTaintedNodeCount(ctx, env.Client, 3)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(4)) // 3 original + 1 replacement
		})

		It("should not disrupt more nodes than the budget allows in a single reconcile cycle", func() {
			// 5 empty drifted nodes with budget={Nodes:"2"}.  disruptWithTimeCap calls
			// disrupt() in a loop, rebuilding the disruption budget from cluster state on
			// each iteration.  After 2 nodes are MarkedForDeletion the budget drops to 0,
			// ComputeCommands skips all remaining candidates, and the loop exits.
			// Exactly 2 of the 5 nodes should be tainted after one reconcile cycle.
			const numNodes = 5
			nodeClaims, nodes := test.NodeClaimsAndNodes(numNodes, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			for _, nc := range nodeClaims {
				nc.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			}
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "2"}}
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			cluster.SetSynced(true)

			ExpectSingletonReconciled(ctx, disruptionController)

			ExpectTaintedNodeCount(ctx, env.Client, 2)
			// Empty-node drift commands create no replacement NodeClaims.
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(numNodes))
		})

		It("should not disrupt any node when all NodePool disruption budgets are zero", func() {
			// Budget={Nodes:"0"} drives allowedDisruptions to 0 for every reason.
			// computeBudgetMappings returns 0 for all NodePools, so hasBudgetForMethod
			// returns false for every Phase 2 method (Drift, Multi, Single).  The eligible
			// list is empty and no disruptions occur even though the node is drifted.
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         boolPtr(true),
						BlockOwnerDeletion: boolPtr(true),
					}},
				},
			})
			nc, node := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU: resource.MustParse("32"),
					},
				},
			})
			nc.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0"}}
			ExpectApplied(ctx, env.Client, rs, pod, nc, node, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{node}, []*v1.NodeClaim{nc})
			cluster.SetSynced(true)

			ExpectSingletonReconciled(ctx, disruptionController)

			ExpectTaintedNodeCount(ctx, env.Client, 0)
		})
	})

	// -------------------------------------------------------------------------
	// Phase 1 always runs
	// -------------------------------------------------------------------------
	Describe("Phase 1 unconditional execution", func() {
		It("should run Phase 1 (Emptiness) and Phase 2 (Drift) in the same reconcile cycle", func() {
			// Phase 1 methods (weight=0) run every cycle before Phase 2.
			// Phase 2 runs one method per cycle via CFS scheduling.
			// This test verifies that both Emptiness (Phase 1) and Drift (Phase 2)
			// can disrupt their respective candidates in a single reconcile.

			// NodePool: WhenEmpty policy so Emptiness applies to the empty node.
			np := test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmpty,
						Budgets:             []v1.Budget{{Nodes: "100%"}},
					},
				},
			})

			// Empty node with ConsolidatableCondition → Emptiness (Phase 1) will delete it.
			ncEmpty, nodeEmpty := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            np.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			ncEmpty.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			// Non-empty drifted node → Drift (Phase 2) will replace it.
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs) // apply rs first so rs.UID is populated
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         boolPtr(true),
						BlockOwnerDeletion: boolPtr(true),
					}},
				},
			})
			ncDrift, nodeDrift := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            np.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			ncDrift.StatusConditions().SetTrue(v1.ConditionTypeDrifted)

			ExpectApplied(ctx, env.Client, np, pod, ncEmpty, nodeEmpty, ncDrift, nodeDrift)
			ExpectManualBinding(ctx, env.Client, pod, nodeDrift)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{nodeEmpty, nodeDrift}, []*v1.NodeClaim{ncEmpty, ncDrift})
			cluster.SetSynced(true)

			// Single reconcile: Phase 1 deletes the empty node, Phase 2 replaces the drifted node.
			ExpectSingletonReconciled(ctx, disruptionController)

			// Both nodes should be tainted for deletion.
			ExpectTaintedNodeCount(ctx, env.Client, 2)
			// One replacement NodeClaim was created for the drifted node.
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(3)) // 2 original + 1 replacement
		})
	})

	// -------------------------------------------------------------------------
	// Phase 2 CFS method fall-through
	// -------------------------------------------------------------------------
	Describe("Phase 2 CFS method fall-through", func() {
		It("should fall through to MultiNodeConsolidation when Drift finds no candidates", func() {
			// nc1 and nc2 are Consolidatable but not Drifted.  Drift has the highest
			// weight (lowest initial druntime) so pickLowestDruntime selects it first,
			// but GetCandidates returns zero candidates and disrupt() returns success=false.
			// Drift is removed from the eligible list and MultiNodeConsolidation gets its
			// turn, consolidating both nodes into a single cheaper replacement NodeClaim.
			np := test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
						ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
						Budgets:             []v1.Budget{{Nodes: "100%"}},
					},
				},
			})
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, np, rs)

			makePod := func() *corev1.Pod {
				return test.Pod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         boolPtr(true),
							BlockOwnerDeletion: boolPtr(true),
						}},
					},
				})
			}

			makeNC := func() (*v1.NodeClaim, *corev1.Node) {
				nc, n := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            np.Name,
							corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
							v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
							corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
						},
					},
					Status: v1.NodeClaimStatus{
						ProviderID: test.RandomProviderID(),
						Allocatable: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:  resource.MustParse("32"),
							corev1.ResourcePods: resource.MustParse("100"),
						},
					},
				})
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				// Intentionally no ConditionTypeDrifted: Drift should find no candidates.
				return nc, n
			}

			nc1, node1 := makeNC()
			nc2, node2 := makeNC()
			pod1, pod2 := makePod(), makePod()
			ExpectApplied(ctx, env.Client, pod1, pod2, nc1, node1, nc2, node2)
			ExpectManualBinding(ctx, env.Client, pod1, node1)
			ExpectManualBinding(ctx, env.Client, pod2, node2)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{node1, node2}, []*v1.NodeClaim{nc1, nc2})
			cluster.SetSynced(true)

			ExpectSingletonReconciled(ctx, disruptionController)

			// Multi consolidated nc1+nc2 into one replacement — both originals should be tainted.
			ExpectTaintedNodeCount(ctx, env.Client, 2)
			// nc1 + nc2 + 1 replacement = 3 NodeClaims.
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(3))
		})
	})
})

// boolPtr returns a pointer to the given bool value.
func boolPtr(b bool) *bool { return &b }
