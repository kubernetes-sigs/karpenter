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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Balanced Consolidation", func() {
	var nodePool *v1.NodePool
	var labels = map[string]string{"app": "balanced-test"}

	BeforeEach(func() {

		nodePool = test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyBalanced,
					Budgets:             []v1.Budget{{Nodes: "100%"}},
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
				},
			},
		})
	})

	Context("Single-Node", func() {
		It("should approve replacing an oversized node", func() {
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         lo.ToPtr(true),
							BlockOwnerDeletion: lo.ToPtr(true),
						},
					}},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			})

			nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
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
			nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))

			// Replace should create a cheaper node
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodeClaims[0].Name).ToNot(Equal(nodeClaim.Name))
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(mostExpensiveInstance.Name)).To(BeFalse())

			Expect(recorder.Calls(events.ConsolidationApproved)).To(BeNumerically(">=", 1))

			// Verify the event message contains correct scoring details
			_, ok := lo.Find(recorder.Events(), func(e events.Event) bool {
				return e.Reason == events.ConsolidationApproved &&
					strings.Contains(e.Message, "k: 2") &&
					strings.Contains(e.Message, ">= threshold 0.50")
			})
			Expect(ok).To(BeTrue(), "expected ConsolidationApproved event with k: 2 and >= threshold 0.50")
		})

		It("should reject replacing a well-packed node", func() {
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(8, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         lo.ToPtr(true),
							BlockOwnerDeletion: lo.ToPtr(true),
						},
					}},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3500m")},
				},
			})

			// Use the least expensive instance so there is no cheaper option
			nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
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
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			ExpectApplied(ctx, env.Client, rs, nodePool, nodeClaim, node)
			for _, p := range pods {
				ExpectApplied(ctx, env.Client, p)
				ExpectManualBinding(ctx, env.Client, p, node)
			}

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))
			Expect(recorder.Calls(events.ConsolidationApproved)).To(Equal(0))
		})
	})

	Context("Multi-Node", func() {
		It("should approve consolidating multiple underutilized nodes", func() {
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         lo.ToPtr(true),
							BlockOwnerDeletion: lo.ToPtr(true),
						},
					}},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				},
			})

			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
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
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}

			ExpectApplied(ctx, env.Client, rs, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i], pods[i])
				ExpectManualBinding(ctx, env.Client, pods[i], nodes[i])
			}

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(len(cmds[0].Candidates)).To(BeNumerically(">=", 2))

			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims...)

			remaining := ExpectNodeClaims(ctx, env.Client)
			Expect(len(remaining)).To(BeNumerically("<", 3))
		})
	})

	Context("Budgets", func() {
		It("should block disruption when budget is 0%", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0%"}}

			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         lo.ToPtr(true),
							BlockOwnerDeletion: lo.ToPtr(true),
						},
					}},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			})

			nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
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
			nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Budget of 0% should block all disruption even though balanced scoring would approve
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))
			Expect(recorder.Calls(events.ConsolidationApproved)).To(Equal(0))

			// Verify no ConsolidationApproved event was published at all
			approvedEvents := lo.Filter(recorder.Events(), func(e events.Event, _ int) bool {
				return e.Reason == events.ConsolidationApproved
			})
			Expect(approvedEvents).To(BeEmpty(), "expected no ConsolidationApproved events when budget is 0%%")
		})
	})

	Context("Priority Ordering", func() {
		It("should consolidate empty nodes before non-empty nodes when budget is limited", func() {
			// Budget of 1 node means only 1 disruption per cycle
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "1"}}

			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			ownerRef := []metav1.OwnerReference{{
				APIVersion:         "apps/v1",
				Kind:               "ReplicaSet",
				Name:               rs.Name,
				UID:                rs.UID,
				Controller:         lo.ToPtr(true),
				BlockOwnerDeletion: lo.ToPtr(true),
			}}

			// Node 1: empty (no pods)
			emptyNodeClaim, emptyNode := test.NodeClaimAndNode(v1.NodeClaim{
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
			emptyNodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			// Node 2: underutilized (has a small pod)
			underutilizedNodeClaim, underutilizedNode := test.NodeClaimAndNode(v1.NodeClaim{
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
			underutilizedNodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, OwnerReferences: ownerRef},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			})

			ExpectApplied(ctx, env.Client, rs, nodePool, emptyNodeClaim, emptyNode, underutilizedNodeClaim, underutilizedNode, pod)
			ExpectManualBinding(ctx, env.Client, pod, underutilizedNode)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{emptyNode, underutilizedNode},
				[]*v1.NodeClaim{emptyNodeClaim, underutilizedNodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1), "expected exactly 1 disruption command due to budget of 1")

			// The empty node should be selected (higher score: minimal disruption fraction)
			candidateNodeClaimNames := lo.Map(cmds[0].Candidates, func(c *disruption.Candidate, _ int) string { return c.NodeClaim.Name })
			Expect(candidateNodeClaimNames).To(ContainElement(emptyNodeClaim.Name), "expected empty node to be disrupted first")
			Expect(candidateNodeClaimNames).NotTo(ContainElement(underutilizedNodeClaim.Name), "expected underutilized node to NOT be disrupted")
		})
	})

	Context("Decision and Savings", func() {
		It("should choose delete when pods fit on existing nodes", func() {
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			ownerRef := []metav1.OwnerReference{{
				APIVersion:         "apps/v1",
				Kind:               "ReplicaSet",
				Name:               rs.Name,
				UID:                rs.UID,
				Controller:         lo.ToPtr(true),
				BlockOwnerDeletion: lo.ToPtr(true),
			}}

			// Use the second cheapest on-demand instance for node A so the pod
			// from node A can fit on node B but node A is still worth deleting.
			// Pick an instance with at least 2 CPU so the pod fits.
			sourceInstance := onDemandInstances[len(onDemandInstances)/2]
			sourceOffering := sourceInstance.Offerings[0]

			// Node A: moderately expensive, with a small pod
			nodeClaimA, nodeA := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: sourceInstance.Name,
						v1.CapacityTypeLabelKey:        sourceOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       sourceOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaimA.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			podA := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, OwnerReferences: ownerRef},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				},
			})

			// Node B: same instance type, heavily utilized so multi-node consolidation
			// won't try to replace it. Has 1 CPU spare for podA.
			nodeClaimB, nodeB := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: sourceInstance.Name,
						v1.CapacityTypeLabelKey:        sourceOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       sourceOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaimB.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			// Fill node B with many pods so its disruption cost is high
			podsB := test.Pods(30, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, OwnerReferences: ownerRef},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				},
			})

			ExpectApplied(ctx, env.Client, rs, nodePool, nodeClaimA, nodeA, nodeClaimB, nodeB, podA)
			ExpectManualBinding(ctx, env.Client, podA, nodeA)
			for _, p := range podsB {
				ExpectApplied(ctx, env.Client, p)
				ExpectManualBinding(ctx, env.Client, p, nodeB)
			}

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{nodeA, nodeB},
				[]*v1.NodeClaim{nodeClaimA, nodeClaimB})
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).ToNot(BeEmpty())

			// Find the delete command (node A's pod fits on node B, no replacement needed)
			deleteCmd := lo.Filter(cmds, func(cmd *disruption.Command, _ int) bool {
				return cmd.Decision() == disruption.DeleteDecision
			})
			Expect(deleteCmd).ToNot(BeEmpty(), "expected at least one delete command")

			cmd := deleteCmd[0]
			// Savings for delete = full source node price
			Expect(cmd.EstimatedSavings()).To(BeNumerically("~", sourceOffering.Price, 0.01))
		})

		It("should choose replace when pods need a new cheaper node", func() {
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			ownerRef := []metav1.OwnerReference{{
				APIVersion:         "apps/v1",
				Kind:               "ReplicaSet",
				Name:               rs.Name,
				UID:                rs.UID,
				Controller:         lo.ToPtr(true),
				BlockOwnerDeletion: lo.ToPtr(true),
			}}

			// Node A: expensive (mostExpensiveInstance), with a pod that requires
			// significant CPU so it cannot fit on node B (which has no spare capacity).
			nodeClaimA, nodeA := test.NodeClaimAndNode(v1.NodeClaim{
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
			nodeClaimA.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			// Pod requires 2 CPU — fits on many cheaper instance types but not on
			// the leastExpensiveInstance (1 CPU allocatable).
			podA := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, OwnerReferences: ownerRef},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				},
			})

			ExpectApplied(ctx, env.Client, rs, nodePool, nodeClaimA, nodeA, podA)
			ExpectManualBinding(ctx, env.Client, podA, nodeA)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{nodeA},
				[]*v1.NodeClaim{nodeClaimA})
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))

			cmd := cmds[0]
			Expect(cmd.Decision()).To(Equal(disruption.ReplaceDecision))
			// Savings for replace = source price - destination price.
			// The destination should be cheaper than the source.
			Expect(cmd.EstimatedSavings()).To(BeNumerically(">", 0))
			Expect(cmd.EstimatedSavings()).To(BeNumerically("<", mostExpensiveOffering.Price))
		})
	})

	Context("Ratio Sort in Multi-Node", func() {
		It("should select highest-ratio candidates first when nodes have heterogeneous prices and pod counts", func() {
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			ownerRef := []metav1.OwnerReference{{
				APIVersion:         "apps/v1",
				Kind:               "ReplicaSet",
				Name:               rs.Name,
				UID:                rs.UID,
				Controller:         lo.ToPtr(true),
				BlockOwnerDeletion: lo.ToPtr(true),
			}}

			// Pick three on-demand instance types with different prices.
			// onDemandInstances is sorted cheapest-first.
			cheapInstance := onDemandInstances[0] // ~$1.2/hr (4cpu/8mem)
			cheapOffering := cheapInstance.Offerings[0]
			moderateInstance := onDemandInstances[len(onDemandInstances)/2] // ~$8/hr (16cpu/64mem)
			moderateOffering := moderateInstance.Offerings[0]
			expensiveInstance := onDemandInstances[len(onDemandInstances)-1] // ~$16/hr (32cpu/128mem)
			expensiveOffering := expensiveInstance.Offerings[0]

			// Node A: expensive ($16/hr), 10 pods -> disruption cost = 1+10 = 11
			// ratio = 16/11 ~ 1.45
			nodeClaimA, nodeA := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: expensiveInstance.Name,
						v1.CapacityTypeLabelKey:        expensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       expensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaimA.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			podsA := test.Pods(10, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, OwnerReferences: ownerRef},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			})

			// Node B: moderate ($8/hr), 1 pod -> disruption cost = 1+1 = 2
			// ratio = 8/2 = 4.0 (highest -- best value)
			nodeClaimB, nodeB := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: moderateInstance.Name,
						v1.CapacityTypeLabelKey:        moderateOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       moderateOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaimB.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			podB := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, OwnerReferences: ownerRef},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			})

			// Node C: cheap ($1.2/hr), 8 pods -> disruption cost = 1+8 = 9
			// ratio = 1.2/9 ~ 0.13 (lowest -- worst value)
			nodeClaimC, nodeC := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: cheapInstance.Name,
						v1.CapacityTypeLabelKey:        cheapOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       cheapOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("4"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaimC.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			podsC := test.Pods(8, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, OwnerReferences: ownerRef},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			})

			ExpectApplied(ctx, env.Client, rs, nodePool, nodeClaimA, nodeA, nodeClaimB, nodeB, nodeClaimC, nodeC)
			for _, p := range podsA {
				ExpectApplied(ctx, env.Client, p)
				ExpectManualBinding(ctx, env.Client, p, nodeA)
			}
			ExpectApplied(ctx, env.Client, podB)
			ExpectManualBinding(ctx, env.Client, podB, nodeB)
			for _, p := range podsC {
				ExpectApplied(ctx, env.Client, p)
				ExpectManualBinding(ctx, env.Client, p, nodeC)
			}

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{nodeA, nodeB, nodeC},
				[]*v1.NodeClaim{nodeClaimA, nodeClaimB, nodeClaimC})
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).ToNot(BeEmpty(), "expected at least one consolidation command")

			// The multi-node binary search processes candidates sorted by ratio
			// descending: B (4.0), A (1.45), C (0.13). The resulting command's
			// candidates list preserves this order.
			cmd := cmds[0]
			candidateNames := lo.Map(cmd.Candidates, func(c *disruption.Candidate, _ int) string { return c.NodeClaim.Name })

			// B must appear as a candidate (highest ratio means it is always in the
			// binary search window).
			Expect(candidateNames).To(ContainElement(nodeClaimB.Name), "expected Node B (highest ratio) to be in the command")

			// If all three are present, verify B comes before C in the candidate
			// list (ratio ordering preserved).
			idxB := lo.IndexOf(candidateNames, nodeClaimB.Name)
			idxC := lo.IndexOf(candidateNames, nodeClaimC.Name)
			if idxC >= 0 {
				Expect(idxB).To(BeNumerically("<", idxC), "expected Node B to precede Node C in ratio-sorted candidates")
			}
		})
	})

	Context("Mixed-Policy NodePools", func() {
		It("should apply scoring to Balanced pools while non-Balanced pools consolidate normally", func() {
			balancedPool := test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyBalanced,
						Budgets:             []v1.Budget{{Nodes: "100%"}},
						ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
					},
				},
			})
			defaultPool := test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
						Budgets:             []v1.Budget{{Nodes: "100%"}},
						ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
					},
				},
			})

			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			ownerRef := []metav1.OwnerReference{{
				APIVersion:         "apps/v1",
				Kind:               "ReplicaSet",
				Name:               rs.Name,
				UID:                rs.UID,
				Controller:         lo.ToPtr(true),
				BlockOwnerDeletion: lo.ToPtr(true),
			}}

			balancedPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, OwnerReferences: ownerRef},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			})
			balancedNodeClaim, balancedNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            balancedPool.Name,
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
			balancedNodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			defaultPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, OwnerReferences: ownerRef},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			})
			defaultNodeClaim, defaultNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            defaultPool.Name,
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
			defaultNodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			ExpectApplied(ctx, env.Client, rs, balancedPool, defaultPool,
				balancedNodeClaim, balancedNode, balancedPod,
				defaultNodeClaim, defaultNode, defaultPod)
			ExpectManualBinding(ctx, env.Client, balancedPod, balancedNode)
			ExpectManualBinding(ctx, env.Client, defaultPod, defaultNode)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{balancedNode, defaultNode},
				[]*v1.NodeClaim{balancedNodeClaim, defaultNodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).ToNot(BeEmpty(), "expected at least one consolidation command")

			for _, cmd := range cmds {
				ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmd)
				ExpectObjectReconciled(ctx, env.Client, queue, cmd.Candidates[0].NodeClaim)
			}
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, balancedNodeClaim, defaultNodeClaim)

			remaining := ExpectNodeClaims(ctx, env.Client)
			remainingNames := lo.Map(remaining, func(nc *v1.NodeClaim, _ int) string { return nc.Name })
			Expect(remainingNames).NotTo(ContainElement(balancedNodeClaim.Name))
			Expect(remainingNames).NotTo(ContainElement(defaultNodeClaim.Name))

			balancedApproved := lo.Filter(recorder.Events(), func(e events.Event, _ int) bool {
				return e.Reason == events.ConsolidationApproved &&
					strings.Contains(e.Message, "k: 2")
			})
			Expect(balancedApproved).NotTo(BeEmpty(), "expected Balanced pool to emit scored ConsolidationApproved events")
		})
	})

})
