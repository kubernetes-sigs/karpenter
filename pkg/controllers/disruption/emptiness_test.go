/*
Copyright 2023 The Kubernetes Authors.

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

// nolint:gosec
package disruption_test

import (
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha5"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Emptiness", func() {
	var nodePool *v1beta1.NodePool
	var nodeClaim *v1beta1.NodeClaim
	var node *v1.Node

	BeforeEach(func() {
		nodePool = test.NodePool(v1beta1.NodePool{
			Spec: v1beta1.NodePoolSpec{
				Disruption: v1beta1.Disruption{
					ConsolidateAfter:    &v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 0)},
					ConsolidationPolicy: v1beta1.ConsolidationPolicyWhenEmpty,
					ExpireAfter:         v1beta1.NillableDuration{Duration: nil},
					// Disrupt away!
					Budgets: []v1beta1.Budget{{
						Nodes: "100%",
					}},
				},
			},
		})
		nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
				},
			},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Empty)
	})
	Context("Events", func() {
		It("should not fire an event for ConsolidationDisabled when the NodePool has consolidation set to WhenUnderutilized", func() {
			nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
			nodePool.Spec.Disruption.ConsolidateAfter = nil
			ExpectApplied(ctx, env.Client, node, nodeClaim, nodePool)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			Expect(recorder.Calls("Unconsolidatable")).To(Equal(0))
		})
		It("should fire an event for ConsolidationDisabled when the NodePool has consolidateAfter set to 'Never'", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{}
			ExpectApplied(ctx, env.Client, node, nodeClaim, nodePool)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// We get six calls here because we have Nodes and NodeClaims that fired for this event
			// and each of the consolidation mechanisms specifies that this event should be fired
			Expect(recorder.Calls("Unconsolidatable")).To(Equal(2))
		})
	})
	Context("Budgets", func() {
		var numNodes = 10
		var nodeClaims []*v1beta1.NodeClaim
		var nodes []*v1.Node
		var rs *appsv1.ReplicaSet
		BeforeEach(func() {
			// create our RS so we can link a pod to it
			rs = test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		})
		It("should only allow 3 empty nodes to be disrupted", func() {
			nodeClaims, nodes = test.NodeClaimsAndNodes(numNodes, v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodePool.Spec.Disruption.Budgets = []v1beta1.Budget{{Nodes: "30%"}}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := 0; i < numNodes; i++ {
				nodeClaims[i].StatusConditions().MarkTrue(v1beta1.Empty)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Step the clock 10 minutes so that the emptiness expires
			fakeClock.Step(10 * time.Minute)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Execute command, thus deleting 3 nodes
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(7))
		})
		It("should allow 2 nodes from each nodePool to be deleted", func() {
			// Create 10 nodepools
			nps := test.NodePools(10, v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Disruption: v1beta1.Disruption{
						ConsolidateAfter:    &v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 30)},
						ConsolidationPolicy: v1beta1.ConsolidationPolicyWhenEmpty,
						ExpireAfter:         v1beta1.NillableDuration{Duration: nil},
						Budgets: []v1beta1.Budget{{
							Nodes: "20%",
						}},
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := 0; i < len(nps); i++ {
				ExpectApplied(ctx, env.Client, nps[i])
			}
			nodeClaims = make([]*v1beta1.NodeClaim, 0, 30)
			nodes = make([]*v1.Node, 0, 30)
			// Create 3 nodes for each nodePool
			for _, np := range nps {
				ncs, ns := test.NodeClaimsAndNodes(3, v1beta1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1beta1.NodePoolLabelKey:     np.Name,
							v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
							v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
							v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
						},
					},
					Status: v1beta1.NodeClaimStatus{
						Allocatable: map[v1.ResourceName]resource.Quantity{
							v1.ResourceCPU:  resource.MustParse("32"),
							v1.ResourcePods: resource.MustParse("100"),
						},
					},
				})
				nodeClaims = append(nodeClaims, ncs...)
				nodes = append(nodes, ns...)
			}
			ExpectApplied(ctx, env.Client, nodePool)
			for i := 0; i < len(nodeClaims); i++ {
				nodeClaims[i].StatusConditions().MarkTrue(v1beta1.Empty)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Step the clock 10 minutes so that the emptiness expires
			fakeClock.Step(10 * time.Minute)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Execute the command in the queue, only deleting 20 nodes
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(10))
		})
	})
	Context("Emptiness", func() {
		It("can delete empty nodes", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)
			wg := sync.WaitGroup{}
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			// Cascade any deletion of the nodeClaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the empty node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("should ignore nodes without the empty status condition", func() {
			_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Empty)
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("should ignore nodes with the karpenter.sh/do-not-disrupt annotation", func() {
			node.Annotations = lo.Assign(node.Annotations, map[string]string{v1beta1.DoNotDisruptAnnotationKey: "true"})
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("should ignore nodes that have pods with the karpenter.sh/do-not-evict annotation", func() {
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha5.DoNotEvictPodAnnotationKey: "true",
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("should ignore nodes that have pods with the karpenter.sh/do-not-disrupt annotation", func() {
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1beta1.DoNotDisruptAnnotationKey: "true",
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("should ignore nodes with the empty status condition set to false", func() {
			nodeClaim.StatusConditions().MarkFalse(v1beta1.Empty, "", "")
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
	})
})
