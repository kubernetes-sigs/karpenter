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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Expiration", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node

	BeforeEach(func() {
		nodePool = test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateAfter: v1.MustParseNillableDuration("1h"),
					// Disrupt away!
					Budgets: []v1.Budget{{
						Nodes: "100%",
					}},
				},
			},
		})
		nodePool.Spec.Template.Spec.ExpireAfter = v1.MustParseNillableDuration("30s")
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
					v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				},
			},
			Spec: v1.NodeClaimSpec{
				ExpireAfter: v1.MustParseNillableDuration("30s"),
			},
			Status: v1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU:  resource.MustParse("32"),
					corev1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
	})

	Context("Eligibility", func() {
		It("should replace an expired nodeclaim, staging the replacement first", func() {
			// A pod that has to land somewhere, so the simulation produces a replacement rather than a bare delete.
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					}},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("1")},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// step forward to make the nodeclaim expired
			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			// A replacement is staged before the candidate is removed, so the command carries one replacement.
			Expect(queue.GetCommands()).To(HaveLen(1))
			cmd := queue.GetCommands()[0]
			Expect(cmd.Reason()).To(Equal(v1.DisruptionReasonExpired))
			Expect(cmd.Replacements).To(HaveLen(1))
			Expect(cmd.Candidates).To(HaveLen(1))
			Expect(cmd.Candidates[0].Name()).To(Equal(node.Name))
		})
		It("shouldn't disrupt a nodeclaim that isn't expired yet", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// not yet past expireAfter
			env.Clock.Step(10 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
		})
		It("shouldn't disrupt a nodeclaim when expiration is disabled", func() {
			nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("Never")
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
		})
		It("shouldn't disrupt an expired nodeclaim owned by a static nodepool", func() {
			// Static nodepools size themselves from replicas, so they stay on the forceful expiration path.
			staticNodePool := test.StaticNodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{Replicas: lo.ToPtr(int64(1))},
			})
			nodeClaim.Labels[v1.NodePoolLabelKey] = staticNodePool.Name
			node.Labels[v1.NodePoolLabelKey] = staticNodePool.Name
			ExpectApplied(ctx, env.Client, staticNodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
		})
	})

	Context("Budgets", func() {
		var numNodes = 10
		var nodeClaims []*v1.NodeClaim
		var nodes []*corev1.Node
		var rs *appsv1.ReplicaSet
		BeforeEach(func() {
			// create our RS so we can link a pod to it
			rs = test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			nodeClaims, nodes = test.NodeClaimsAndNodes(numNodes, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Spec: v1.NodeClaimSpec{
					ExpireAfter: v1.MustParseNillableDuration("30s"),
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
		})
		It("should only consider 3 nodes allowed to be disrupted because of budgets", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "30%"}}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			// step forward to expire every nodeclaim at once
			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			ExpectMetricGaugeValue(disruption.NodePoolAllowedDisruptions, 3, map[string]string{
				metrics.NodePoolLabel: nodePool.Name,
				metrics.ReasonLabel:   strings.ToLower(string(v1.DisruptionReasonExpired)),
			})
		})
		It("shouldn't disrupt any expired nodes when the budget is zero", func() {
			// This is the peak-hours case: expireAfter has elapsed for every node, but the operator's budget forbids
			// disruption, so expireAfter acts as a floor on lifetime rather than a deadline.
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0"}}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
			ExpectMetricGaugeValue(disruption.NodePoolAllowedDisruptions, 0, map[string]string{
				metrics.NodePoolLabel: nodePool.Name,
				metrics.ReasonLabel:   strings.ToLower(string(v1.DisruptionReasonExpired)),
			})
			// Every nodeclaim must survive: the forceful path no longer owns these, so nothing may delete them.
			for i := range numNodes {
				ExpectExists(ctx, env.Client, nodeClaims[i])
			}
		})
		It("should respect a budget scoped to the Expired reason", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{
				{Reasons: []v1.DisruptionReason{v1.DisruptionReasonExpired}, Nodes: "0"},
				{Reasons: []v1.DisruptionReason{v1.DisruptionReasonDrifted}, Nodes: "100%"},
			}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			// The Drifted budget is wide open, but it must not grant expiration any headroom.
			Expect(queue.GetCommands()).To(HaveLen(0))
			ExpectMetricGaugeValue(disruption.NodePoolAllowedDisruptions, 0, map[string]string{
				metrics.NodePoolLabel: nodePool.Name,
				metrics.ReasonLabel:   strings.ToLower(string(v1.DisruptionReasonExpired)),
			})
		})
		It("should disrupt the oldest-deadline nodeclaim first", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "1"}}

			// Give the first nodeclaim the earliest deadline by halving its expireAfter. Every nodeclaim shares a
			// creation timestamp, so expireAfter alone orders them.
			nodeClaims[0].Spec.ExpireAfter = v1.MustParseNillableDuration("15s")

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(1))
			Expect(queue.GetCommands()[0].Candidates).To(HaveLen(1))
			Expect(queue.GetCommands()[0].Candidates[0].Name()).To(Equal(nodes[0].Name))
		})
	})
})
