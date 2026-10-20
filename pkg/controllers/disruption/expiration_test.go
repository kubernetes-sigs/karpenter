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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
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
		It("should honor a blocking PDB during expiration", func() {
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "blocked"},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID,
						Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
					}},
				},
			})
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         map[string]string{"app": "blocked"},
				MaxUnavailable: lo.ToPtr(intstr.FromInt(0)),
			})
			ExpectApplied(ctx, env.Client, rs, nodePool, nodeClaim, node, pod, pdb)
			ExpectManualBinding(ctx, env.Client, pod, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("should not expire a node protected by do-not-disrupt without a termination grace period", func() {
			node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
		})
		It("should not expire a do-not-disrupt node even with a termination grace period", func() {
			node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
			nodeClaim.Spec.TerminationGracePeriod = &metav1.Duration{Duration: 5 * time.Minute}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
		})
		It("should expire an empty node without staging a replacement", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(1))
			Expect(queue.GetCommands()[0].Replacements).To(BeEmpty())
		})
		It("should not expire when the replacement cannot be scheduled", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			cloudProvider.ErrorsForNodePool[nodePool.Name] = fmt.Errorf("no compatible instance types")

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("should use static replacement semantics after a dynamic NodePool becomes static", func() {
			staticNodePool := test.StaticNodePool(v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{Name: nodePool.Name},
				Spec:       v1.NodePoolSpec{Replicas: lo.ToPtr(int64(1)), Disruption: v1.Disruption{Budgets: []v1.Budget{{Nodes: "100%"}}}},
			})
			nodeClaim.Labels[v1.NodePoolLabelKey] = staticNodePool.Name
			node.Labels[v1.NodePoolLabelKey] = staticNodePool.Name
			ExpectApplied(ctx, env.Client, staticNodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(1))
			Expect(queue.GetCommands()[0].Replacements).To(HaveLen(1))
			Expect(queue.GetCommands()[0].Replacements[0].IsStaticNodeClaim).To(BeTrue())
		})
		It("should reject changing a static NodePool to dynamic", func() {
			nodePool = test.StaticNodePool(v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{Name: nodePool.Name},
				Spec:       v1.NodePoolSpec{Replicas: lo.ToPtr(int64(1)), Disruption: v1.Disruption{Budgets: []v1.Budget{{Nodes: "100%"}}}},
			})
			nodeClaim.Labels[v1.NodePoolLabelKey] = nodePool.Name
			node.Labels[v1.NodePoolLabelKey] = nodePool.Name
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			nodePool.Spec.Replicas = nil
			Expect(env.Client.Update(ctx, nodePool)).To(MatchError(ContainSubstring("Cannot transition NodePool between static")))
		})
		It("should rediscover an expired candidate after disruption controller restart without duplicating it", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)
			Expect(queue.GetCommands()).To(HaveLen(1))

			restartedQueue := disruption.NewQueue(env.Client, recorder, cluster, env.Clock, prov)
			restartedController := disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, restartedQueue, clusterCost,
				disruption.WithMethods(disruption.NewExpiration(env.Client, cluster, prov, recorder, env.Clock)))
			_, err := restartedController.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(restartedQueue.GetCommands()).To(BeEmpty())
		})
		It("shouldn't disrupt a nodeclaim when expiration is disabled", func() {
			nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("Never")
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
		})
		It("should replace an expired nodeclaim owned by a static nodepool", func() {
			// Static pools use their replica count rather than workload simulation to stage the replacement.
			staticNodePool := test.StaticNodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{Replicas: lo.ToPtr(int64(1))},
			})
			nodeClaim.Labels[v1.NodePoolLabelKey] = staticNodePool.Name
			node.Labels[v1.NodePoolLabelKey] = staticNodePool.Name
			ExpectApplied(ctx, env.Client, staticNodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(1))
			Expect(queue.GetCommands()[0].Reason()).To(Equal(v1.DisruptionReasonExpired))
			Expect(queue.GetCommands()[0].Replacements).To(HaveLen(1))
		})
		It("should block static expiration when the node limit leaves no replacement capacity", func() {
			staticNodePool := test.StaticNodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Replicas:   lo.ToPtr(int64(1)),
					Limits:     v1.Limits{resources.Node: resource.MustParse("1")},
					Disruption: v1.Disruption{Budgets: []v1.Budget{{Nodes: "100%"}}},
				},
			})
			nodeClaim.Labels[v1.NodePoolLabelKey] = staticNodePool.Name
			node.Labels[v1.NodePoolLabelKey] = staticNodePool.Name
			ExpectApplied(ctx, env.Client, staticNodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
		})
		It("should honor an Expired-only budget for static expiration", func() {
			staticNodePool := test.StaticNodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Replicas: lo.ToPtr(int64(1)),
					Disruption: v1.Disruption{Budgets: []v1.Budget{
						{Reasons: []v1.DisruptionReason{v1.DisruptionReasonExpired}, Nodes: "0"},
						{Reasons: []v1.DisruptionReason{v1.DisruptionReasonDrifted}, Nodes: "100%"},
					}},
				},
			})
			nodeClaim.Labels[v1.NodePoolLabelKey] = staticNodePool.Name
			node.Labels[v1.NodePoolLabelKey] = staticNodePool.Name
			ExpectApplied(ctx, env.Client, staticNodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(0))
		})
		It("should apply percentage budgets and expire the oldest static node first", func() {
			staticNodePool := test.StaticNodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Replicas:   lo.ToPtr(int64(2)),
					Disruption: v1.Disruption{Budgets: []v1.Budget{{Nodes: "50%"}}},
				},
			})
			staticClaims, staticNodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					v1.NodePoolLabelKey:            staticNodePool.Name,
					corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
					v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				}},
				Spec: v1.NodeClaimSpec{ExpireAfter: v1.MustParseNillableDuration("30s")},
				Status: v1.NodeClaimStatus{Allocatable: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU: resource.MustParse("32"), corev1.ResourcePods: resource.MustParse("100"),
				}},
			})
			staticClaims[0].Spec.ExpireAfter = v1.MustParseNillableDuration("15s")
			ExpectApplied(ctx, env.Client, staticNodePool)
			for i := range staticClaims {
				ExpectApplied(ctx, env.Client, staticClaims[i], staticNodes[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, staticNodes, staticClaims)

			env.Clock.Step(60 * time.Second)
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(1))
			Expect(queue.GetCommands()[0].Candidates[0].Name()).To(Equal(staticNodes[0].Name))
			ExpectMetricGaugeValue(disruption.NodePoolAllowedDisruptions, 1, map[string]string{
				metrics.NodePoolLabel: staticNodePool.Name,
				metrics.ReasonLabel:   strings.ToLower(string(v1.DisruptionReasonExpired)),
			})
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
		It("should honor an active scheduled Expired budget", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{
				Nodes:    "0",
				Reasons:  []v1.DisruptionReason{v1.DisruptionReasonExpired},
				Schedule: lo.ToPtr("@hourly"),
				Duration: &metav1.Duration{Duration: time.Hour},
			}}
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
		})
		It("should allow expiration after a scheduled budget window ends", func() {
			base := env.Clock.Now().UTC().Truncate(time.Hour).Add(time.Hour)
			env.Clock.SetTime(base.Add(30 * time.Second))
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{
				Nodes:    "0",
				Reasons:  []v1.DisruptionReason{v1.DisruptionReasonExpired},
				Schedule: lo.ToPtr("0 * * * *"),
				Duration: &metav1.Duration{Duration: time.Minute},
			}}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// The budget is active 30 seconds after the hourly hit.
			ExpectSingletonReconciled(ctx, disruptionController)
			Expect(queue.GetCommands()).To(HaveLen(0))

			// After the one-minute duration expires, the same Expired candidate becomes actionable.
			env.Clock.SetTime(base.Add(2 * time.Minute))
			ExpectSingletonReconciled(ctx, disruptionController)
			Expect(queue.GetCommands()).To(HaveLen(1))
		})
	})
})
