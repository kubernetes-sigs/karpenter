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
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
)

var _ = Describe("Consolidation", func() {
	var nodePool *v1.NodePool
	var nodeClaim, spotNodeClaim *v1.NodeClaim
	var node, spotNode *corev1.Node
	var labels = map[string]string{
		"app": "test",
	}
	BeforeEach(func() {
		nodePool = test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
					// Disrupt away!
					Budgets: []v1.Budget{{
						Nodes: "100%",
					}},
					ConsolidateAfter: v1.MustParseNillableDuration("0s"),
				},
			},
		})
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
					v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				},
			},
			Status: v1.NodeClaimStatus{
				Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32")},
			},
		})
		spotNodeClaim, spotNode = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: mostExpensiveSpotInstance.Name,
					v1.CapacityTypeLabelKey:        mostExpensiveSpotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       mostExpensiveSpotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				},
			},
			Status: v1.NodeClaimStatus{
				Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32")},
			},
		})
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		spotNodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{SpotToSpotConsolidation: new(true)}}))
	})
	Context("Events", func() {
		It("should not fire an event for ConsolidationDisabled when the NodePool has consolidation set to WhenEmptyOrUnderutilized", func() {
			nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
			nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
			ExpectApplied(ctx, env.Client, node, nodeClaim, nodePool)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(recorder.Calls(events.Unconsolidatable)).To(Equal(0))
		})
		It("should fire an event for ConsolidationDisabled when the NodePool has consolidation set to WhenEmpty", func() {
			pod := test.Pod()
			nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmpty
			nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("1m")
			ExpectApplied(ctx, env.Client, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)
			Expect(recorder.Calls(events.Unconsolidatable)).To(Equal(4))
		})
		It("should fire an event for ConsolidationDisabled when the NodePool has consolidateAfter set to 'Never'", func() {
			pod := test.Pod()
			nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("Never")
			ExpectApplied(ctx, env.Client, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)
			// We get six calls here because we have Nodes and NodeClaims that fired for this event
			// from Emptiness, MultiNodeConsolidation, and SingleNodeConsolidation
			Expect(recorder.Calls(events.Unconsolidatable)).To(Equal(6))
		})
		It("should fire an event when a candidate does not have a resolvable instance type", func() {
			pod := test.Pod()
			delete(nodeClaim.Labels, corev1.LabelInstanceTypeStable)
			delete(node.Labels, corev1.LabelInstanceTypeStable)

			ExpectApplied(ctx, env.Client, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)
			// We get four calls since we only care about this since we don't emit for empty node consolidation
			Expect(recorder.Calls(events.Unconsolidatable)).To(Equal(4))
		})
		It("should fire an event when a candidate does not have the capacity type label", func() {
			pod := test.Pod()
			delete(nodeClaim.Labels, v1.CapacityTypeLabelKey)
			delete(node.Labels, v1.CapacityTypeLabelKey)

			ExpectApplied(ctx, env.Client, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)
			// We get four calls since we only care about this since we don't emit for empty node consolidation
			Expect(recorder.Calls(events.Unconsolidatable)).To(Equal(4))
		})
		It("should fire an event when a candidate does not have the zone label", func() {
			pod := test.Pod()
			delete(nodeClaim.Labels, corev1.LabelTopologyZone)
			delete(node.Labels, corev1.LabelTopologyZone)

			ExpectApplied(ctx, env.Client, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)
			// We get four calls since we only care about this since we don't emit for empty node consolidation
			Expect(recorder.Calls(events.Unconsolidatable)).To(Equal(4))
		})
	})
	Context("Metrics", func() {
		BeforeEach(func() {
			disruption.FailedValidationsTotal.Reset()
		})
		It("should correctly report eligible nodes", func() {
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1.DoNotDisruptAnnotationKey: "true",
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)
			ExpectMetricGaugeValue(disruption.EligibleNodes, 0, map[string]string{
				metrics.ReasonLabel: "underutilized",
			})

			// remove the do-not-disrupt annotation to make the node eligible for consolidation and update cluster state
			pod.SetAnnotations(map[string]string{})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			ExpectMetricGaugeValue(disruption.EligibleNodes, 1, map[string]string{
				metrics.ReasonLabel: "underutilized",
			})
		})
		DescribeTable("should correctly report invalidated commands for emptiness disruption", func(validatorOpt TestEmptinessValidatorOption) {
			nodes := []*corev1.Node{node}
			nodeClaims := []*v1.NodeClaim{nodeClaim}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			emptyConsolidation := disruption.NewEmptiness(c, disruption.WithValidator(NewTestEmptinessValidator(nodes, nodeClaims, nodePool, validatorOpt)))
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, emptyConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, emptyConsolidation.ShouldDisrupt, emptyConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			cmds, err := emptyConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).ToNot(HaveOccurred())
			Expect(cmds).To(Equal([]disruption.Command{}))

			Expect(emptyConsolidation.IsConsolidated()).To(BeFalse())
			ExpectMetricCounterValue(disruption.FailedValidationsTotal, 1, map[string]string{disruption.ConsolidationTypeLabel: emptyConsolidation.ConsolidationType()})
		},
			Entry("when a candidate is blocked by budgets", WithEmptinessBlockingBudget()),
			Entry("when candidates are filtered out due to pod churn", WithEmptinessChurn()),
			Entry("when candidates are filtered out due to candidate being nominated", WithEmptinessNodeNomination()),
		)
		DescribeTable("should correctly report invalidated commands for multi node disruption", func(validatorOpt TestConsolidationValidatorOption) {
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(2, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			nodeClaim2, node2 := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("1")},
				},
			})
			nodeClaim2.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], node, node2, nodeClaim, nodeClaim2, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node2)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, node2)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node, node2}, []*v1.NodeClaim{nodeClaim, nodeClaim2})

			c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			multiNodeConsolidation := disruption.NewMultiNodeConsolidation(c, disruption.WithValidator(NewTestMultiConsolidationValidator(nodePool, validatorOpt)))
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, multiNodeConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, multiNodeConsolidation.ShouldDisrupt, multiNodeConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			cmds, err := multiNodeConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).To(Succeed())
			Expect(cmds).To(Equal([]disruption.Command{}))

			Expect(multiNodeConsolidation.IsConsolidated()).To(BeFalse())
			ExpectMetricCounterValue(disruption.FailedValidationsTotal, 2, map[string]string{disruption.ConsolidationTypeLabel: multiNodeConsolidation.ConsolidationType()})
		},
			Entry("when candidates are blocked by budgets", WithUnderutilizedBlockingBudget()),
			Entry("when candidates are filtered out due to pod churn", WithUnderutilizedChurn()),
			Entry("when candidates are filtered out due to candidate being nominated", WithUnderutilizedNodeNomination()),
		)
		DescribeTable("should correctly report invalidated commands for single node disruption", func(validatorOpt TestConsolidationValidatorOption) {
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			singleNodeConsolidation := disruption.NewSingleNodeConsolidation(c, disruption.WithValidator(NewTestSingleConsolidationValidator(nodePool, validatorOpt)))
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, singleNodeConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, singleNodeConsolidation.ShouldDisrupt, singleNodeConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			cmds, err := singleNodeConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).To(Succeed())
			Expect(cmds).To(Equal([]disruption.Command{}))

			Expect(singleNodeConsolidation.IsConsolidated()).To(BeFalse())
			ExpectMetricCounterValue(disruption.FailedValidationsTotal, 1, map[string]string{disruption.ConsolidationTypeLabel: singleNodeConsolidation.ConsolidationType()})
		},
			Entry("when a candidate is blocked by budgets", WithUnderutilizedBlockingBudget()),
			Entry("when candidates are filtered out due to pod churn", WithUnderutilizedChurn()),
			Entry("when candidates are filtered out due to candidate being nominated", WithUnderutilizedNodeNomination()),
		)
	})
	Context("Budgets", func() {
		var numNodes = 10
		var nodeClaims []*v1.NodeClaim
		var nodes []*corev1.Node
		var rs *appsv1.ReplicaSet
		BeforeEach(func() {
			nodeClaims, nodes = test.NodeClaimsAndNodes(numNodes, v1.NodeClaim{
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
			// create our RS so we can link a pod to it
			rs = test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		})
		It("should only allow 3 empty nodes to be disrupted", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "30%"}}
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			ExpectSingletonReconciled(ctx, disruptionController)

			metric, found := FindMetricWithLabelValues("karpenter_nodepools_allowed_disruptions", map[string]string{
				metrics.NodePoolLabel: nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetGauge().GetValue()).To(BeNumerically("==", 3))

			// Execute command, thus deleting 3 nodes
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(7))
		})
		It("should allow all empty nodes to be disrupted", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			ExpectSingletonReconciled(ctx, disruptionController)

			metric, found := FindMetricWithLabelValues("karpenter_nodepools_allowed_disruptions", map[string]string{
				metrics.NodePoolLabel: nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetGauge().GetValue()).To(BeNumerically("==", 10))

			// Execute command, thus deleting all nodes
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(0))
		})
		It("should allow no empty nodes to be disrupted", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0%"}}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			ExpectSingletonReconciled(ctx, disruptionController)
			metric, found := FindMetricWithLabelValues("karpenter_nodepools_allowed_disruptions", map[string]string{
				metrics.NodePoolLabel: nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetGauge().GetValue()).To(BeNumerically("==", 0))

			// There should be no commands in the queue
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(numNodes))
			Expect(numNodes).To(Equal(numNodes))
		})
		It("should only allow 3 nodes to be deleted in multi node consolidation delete", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "30%"}}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// make a pod for each nodes, where they each all fit into one node.
			// this should make the optimal multi node decision to delete 9.
			// budgets will make it so we can only delete 3.
			pods := test.Pods(numNodes, test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: map[corev1.ResourceName]resource.Quantity{
						// 100m * 10 = 1 vCPU. This should be less than the largest node capacity.
						corev1.ResourceCPU: resource.MustParse("100m"),
					},
				},
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, pods[i])
				ExpectManualBinding(ctx, env.Client, pods[i], nodes[i])
			}

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			ExpectSingletonReconciled(ctx, disruptionController)

			// Execute command, thus deleting 3 nodes
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(7))
		})
		It("should only allow 3 nodes to be deleted in single node consolidation delete", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "30%"}}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// make a pod for each node, where only two pods can fit each node.
			// this will skip over multi node consolidation and go to single
			// node consolidation delete
			pods := test.Pods(numNodes, test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: map[corev1.ResourceName]resource.Quantity{
						// 15 + 15 = 30 < 32
						corev1.ResourceCPU: resource.MustParse("15"),
					},
				},
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, pods[i])
				ExpectManualBinding(ctx, env.Client, pods[i], nodes[i])
			}

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			// Reconcile 5 times, enqueuing 3 commands total.
			for range 5 {
				ExpectSingletonReconciled(ctx, disruptionController)
			}
			// Execute all commands in the queue, only deleting 3 nodes
			cmds := queue.GetCommands()
			for _, cmd := range cmds {
				ExpectObjectReconciled(ctx, env.Client, queue, cmd.Candidates[0].NodeClaim)
			}
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(7))
		})
		It("should allow 2 nodes from each nodePool to be deleted", func() {
			// Create 10 nodepools
			nps := test.NodePools(10, v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
						ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
						Budgets: []v1.Budget{{
							// 1/2 of 3 nodes == 1.5 nodes. This should round up to 2.
							Nodes: "50%",
						}},
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nps {
				ExpectApplied(ctx, env.Client, nps[i])
			}
			nodeClaims = make([]*v1.NodeClaim, 0, 30)
			nodes = make([]*corev1.Node, 0, 30)
			// Create 3 nodes for each nodePool
			for _, np := range nps {
				ncs, ns := test.NodeClaimsAndNodes(3, v1.NodeClaim{
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
				nodeClaims = append(nodeClaims, ncs...)
				nodes = append(nodes, ns...)
			}
			ExpectApplied(ctx, env.Client, nodePool)
			for i := 0; i < len(nodeClaims); i++ {
				nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			ExpectSingletonReconciled(ctx, disruptionController)

			for _, np := range nps {
				metric, found := FindMetricWithLabelValues("karpenter_nodepools_allowed_disruptions", map[string]string{
					metrics.NodePoolLabel: np.Name,
				})
				Expect(found).To(BeTrue())
				Expect(metric.GetGauge().GetValue()).To(BeNumerically("==", 2))
			}

			// Execute the command in the queue, only deleting 20 node claims
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(10))
		})
		It("should allow all nodes from each nodePool to be deleted", func() {
			// Create 10 nodepools
			nps := test.NodePools(10, v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
						ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
						Budgets: []v1.Budget{{
							Nodes: "100%",
						}},
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nps {
				ExpectApplied(ctx, env.Client, nps[i])
			}
			nodeClaims = make([]*v1.NodeClaim, 0, 30)
			nodes = make([]*corev1.Node, 0, 30)
			// Create 3 nodes for each nodePool
			for _, np := range nps {
				ncs, ns := test.NodeClaimsAndNodes(3, v1.NodeClaim{
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
				nodeClaims = append(nodeClaims, ncs...)
				nodes = append(nodes, ns...)
			}
			ExpectApplied(ctx, env.Client, nodePool)
			for i := 0; i < len(nodeClaims); i++ {
				nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			ExpectSingletonReconciled(ctx, disruptionController)

			for _, np := range nps {
				metric, found := FindMetricWithLabelValues("karpenter_nodepools_allowed_disruptions", map[string]string{
					metrics.NodePoolLabel: np.Name,
				})
				Expect(found).To(BeTrue())
				Expect(metric.GetGauge().GetValue()).To(BeNumerically("==", 3))
			}

			// Execute the command in the queue, deleting all node claims
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(0))
		})
		It("should allow no nodes from each nodePool to be deleted", func() {
			// Create 10 nodepools
			nps := test.NodePools(10, v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
						ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
						Budgets: []v1.Budget{{
							Nodes: "0%",
						}},
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nps {
				ExpectApplied(ctx, env.Client, nps[i])
			}
			nodeClaims = make([]*v1.NodeClaim, 0, 30)
			nodes = make([]*corev1.Node, 0, 30)
			// Create 3 nodes for each nodePool
			for _, np := range nps {
				ncs, ns := test.NodeClaimsAndNodes(3, v1.NodeClaim{
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
				nodeClaims = append(nodeClaims, ncs...)
				nodes = append(nodes, ns...)
			}
			ExpectApplied(ctx, env.Client, nodePool)
			for i := 0; i < len(nodeClaims); i++ {
				nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			ExpectSingletonReconciled(ctx, disruptionController)
			for _, np := range nps {
				metric, found := FindMetricWithLabelValues("karpenter_nodepools_allowed_disruptions", map[string]string{
					metrics.NodePoolLabel: np.Name,
				})
				Expect(found).To(BeTrue())
				Expect(metric.GetGauge().GetValue()).To(BeNumerically("==", 0))
			}

			// There should be no commands in the queue
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(30))
		})
		It("should not mark empty node consolidated if the candidates can't be disrupted due to budgets with one nodepool", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0%"}}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			emptyConsolidation := disruption.NewEmptiness(c)
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, emptyConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, emptyConsolidation.ShouldDisrupt, emptyConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			cmds, err := emptyConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).To(Succeed())
			Expect(cmds).To(Equal([]disruption.Command{}))

			Expect(emptyConsolidation.IsConsolidated()).To(BeFalse())
		})
		It("should not mark empty node consolidated if all candidates can't be disrupted due to budgets with many nodepools", func() {
			// Create 10 nodepools
			nps := test.NodePools(10, v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
						ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
						Budgets: []v1.Budget{{
							Nodes: "0%",
						}},
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nps {
				ExpectApplied(ctx, env.Client, nps[i])
			}
			nodeClaims = make([]*v1.NodeClaim, 0, 30)
			nodes = make([]*corev1.Node, 0, 30)
			// Create 3 nodes for each nodePool
			for _, np := range nps {
				ncs, ns := test.NodeClaimsAndNodes(3, v1.NodeClaim{
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
				nodeClaims = append(nodeClaims, ncs...)
				nodes = append(nodes, ns...)
			}
			ExpectApplied(ctx, env.Client, nodePool)
			for i := 0; i < len(nodeClaims); i++ {
				nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			emptyConsolidation := disruption.NewEmptiness(c)
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, emptyConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, emptyConsolidation.ShouldDisrupt, emptyConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			cmds, err := emptyConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).To(Succeed())
			Expect(cmds).To(HaveLen(0))
			Expect(cmds).To(Equal([]disruption.Command{}))

			Expect(emptyConsolidation.IsConsolidated()).To(BeFalse())
		})
		It("should not mark multi node consolidated if the candidates can't be disrupted due to budgets with one nodepool", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0%"}}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				// Add a pod so that the node is non-empty and eligible for consolidation
				pod := test.Pod(test.PodOptions{
					ObjectMeta:           metav1.ObjectMeta{Labels: labels, OwnerReferences: []metav1.OwnerReference{metav1.OwnerReference{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: new(true), BlockOwnerDeletion: new(true)}}},
					ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
				})
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, nodes[i])
			}
			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			multiConsolidation := disruption.NewMultiNodeConsolidation(c)
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, multiConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, multiConsolidation.ShouldDisrupt, multiConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			cmds, err := multiConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).To(Succeed())
			Expect(cmds).To(Equal([]disruption.Command{}))

			Expect(multiConsolidation.IsConsolidated()).To(BeFalse())
		})
		It("should not mark multi node consolidated if all candidates can't be disrupted due to budgets with many nodepools", func() {
			// Create 10 nodepools
			nps := test.NodePools(10, v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
						ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
						Budgets: []v1.Budget{{
							Nodes: "0%",
						}},
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nps {
				ExpectApplied(ctx, env.Client, nps[i])
			}
			nodeClaims = make([]*v1.NodeClaim, 0, 30)
			nodes = make([]*corev1.Node, 0, 30)
			// Create 3 nodes for each nodePool
			for _, np := range nps {
				ncs, ns := test.NodeClaimsAndNodes(3, v1.NodeClaim{
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
				nodeClaims = append(nodeClaims, ncs...)
				nodes = append(nodes, ns...)
			}
			ExpectApplied(ctx, env.Client, nodePool)
			for i := 0; i < len(nodeClaims); i++ {
				nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				// Add a pod so that the node is non-empty and eligible for consolidation
				pod := test.Pod(test.PodOptions{
					ObjectMeta:           metav1.ObjectMeta{Labels: labels, OwnerReferences: []metav1.OwnerReference{metav1.OwnerReference{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: new(true), BlockOwnerDeletion: new(true)}}},
					ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
				})
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, nodes[i])
			}

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			multiConsolidation := disruption.NewMultiNodeConsolidation(c)
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, multiConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, multiConsolidation.ShouldDisrupt, multiConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			cmds, err := multiConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).To(Succeed())
			Expect(cmds).To(Equal([]disruption.Command{}))

			Expect(multiConsolidation.IsConsolidated()).To(BeFalse())
		})
		It("should not mark single node consolidated if the candidates can't be disrupted due to budgets with one nodepool", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0%"}}

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range numNodes {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				// Add a pod so that the node is non-empty and eligible for consolidation
				pod := test.Pod(test.PodOptions{
					ObjectMeta:           metav1.ObjectMeta{Labels: labels, OwnerReferences: []metav1.OwnerReference{metav1.OwnerReference{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: new(true), BlockOwnerDeletion: new(true)}}},
					ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
				})
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, nodes[i])
			}
			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			singleConsolidation := disruption.NewSingleNodeConsolidation(c)
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, singleConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, singleConsolidation.ShouldDisrupt, singleConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			cmds, err := singleConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).To(Succeed())
			Expect(cmds).To(Equal([]disruption.Command{}))

			Expect(singleConsolidation.IsConsolidated()).To(BeFalse())
		})
		It("should not mark single node consolidated if all candidates can't be disrupted due to budgets with many nodepools", func() {
			// Create 10 nodepools
			nps := test.NodePools(10, v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
						ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
						Budgets: []v1.Budget{{
							Nodes: "0%",
						}},
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nps {
				ExpectApplied(ctx, env.Client, nps[i])
			}
			nodeClaims = make([]*v1.NodeClaim, 0, 30)
			nodes = make([]*corev1.Node, 0, 30)
			// Create 3 nodes for each nodePool
			for _, np := range nps {
				ncs, ns := test.NodeClaimsAndNodes(3, v1.NodeClaim{
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
				nodeClaims = append(nodeClaims, ncs...)
				nodes = append(nodes, ns...)
			}
			ExpectApplied(ctx, env.Client, nodePool)
			for i := 0; i < len(nodeClaims); i++ {
				nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				// Add a pod so that the node is non-empty and eligible for consolidation
				pod := test.Pod(test.PodOptions{
					ObjectMeta:           metav1.ObjectMeta{Labels: labels, OwnerReferences: []metav1.OwnerReference{metav1.OwnerReference{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: new(true), BlockOwnerDeletion: new(true)}}},
					ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
				})
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, nodes[i])
			}

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			singleConsolidation := disruption.NewSingleNodeConsolidation(c)
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, singleConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, singleConsolidation.ShouldDisrupt, singleConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			cmds, err := singleConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).To(Succeed())
			Expect(cmds).To(Equal([]disruption.Command{}))

			Expect(singleConsolidation.IsConsolidated()).To(BeFalse())
		})
	})
	Context("Replace", func() {
		DescribeTable("can replace node",
			func(spotToSpot bool) {
				nodeClaim = lo.Ternary(spotToSpot, spotNodeClaim, nodeClaim)
				node = lo.Ternary(spotToSpot, spotNode, node)
				// create our RS so we can link a pod to it
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
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}}})
				ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool)

				// bind pods to node
				ExpectManualBinding(ctx, env.Client, pod, node)

				// inform cluster state about nodes and nodeClaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
				ExpectSingletonReconciled(ctx, disruptionController)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
				ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)
				// Cascade any deletion of the nodeclaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

				// should create a new nodeclaim as there is a cheaper one that can hold the pod
				nodeClaims := ExpectNodeClaims(ctx, env.Client)
				nodes := ExpectNodes(ctx, env.Client)
				Expect(nodeClaims).To(HaveLen(1))
				Expect(nodes).To(HaveLen(1))

				// Expect that the new nodeclaim does not request the most expensive instance type
				Expect(nodeClaims[0].Name).ToNot(Equal(nodeClaim.Name))
				Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Has(corev1.LabelInstanceTypeStable)).To(BeTrue())
				Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(mostExpensiveInstance.Name)).To(BeFalse())

				// and delete the old one
				ExpectNotFound(ctx, env.Client, nodeClaim, node)
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		It("can replace node without sizing the replacement for a pending pod that needs a new node", func() {
			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			// A pending pod that fits on no existing node, only on a new node. It needs that new capacity regardless of
			// any disruption decision, so the candidate's replacement must not be sized and priced to carry it.
			pending := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("48")},
				},
			})

			ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool, pending)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			// the replacement carries only the candidate's pod; the pending pod's new capacity is provisioned
			// independently of consolidation
			Expect(cmds[0].Replacements).To(HaveLen(1))
			Expect(cmds[0].Results.NewNodeClaims).To(HaveLen(1))
			Expect(cmds[0].Results.NewNodeClaims[0].Pods).To(HaveLen(1))
			Expect(cmds[0].Results.NewNodeClaims[0].Pods[0].Name).To(Equal(pod.Name))

			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)
			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// should create a new nodeclaim as there is a cheaper one that can hold the pod
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodes).To(HaveLen(1))

			// Expect that the new nodeclaim does not request the most expensive instance type
			Expect(nodeClaims[0].Name).ToNot(Equal(nodeClaim.Name))
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Has(corev1.LabelInstanceTypeStable)).To(BeTrue())
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(mostExpensiveInstance.Name)).To(BeFalse())

			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaim, node)

			// pending pod is still here and hasn't been scheduled
			pending = ExpectPodExists(ctx, env.Client, pending.Name, pending.Namespace)
			Expect(pending.Spec.NodeName).To(BeEmpty())
		})
		DescribeTable("only requires Spot for an on-demand replacement when a Spot offering is available", func(spotAvailable bool) {
			offerings := func(price float64) []cloudprovider.Offering {
				return []cloudprovider.Offering{
					{
						Available:    true,
						Compatible:   true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1"}),
						Price:        price,
					},
					{
						Available:    spotAvailable,
						Compatible:   true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeSpot, corev1.LabelTopologyZone: "test-zone-1"}),
						Price:        price / 2,
					},
				}
			}
			currentInstance := fake.NewInstanceType("current", fake.WithOfferings(offerings(1.5)...))
			replacementInstance := fake.NewInstanceType("replacement", fake.WithOfferings(offerings(1.0)...))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{currentInstance, replacementInstance}
			ExpectSingletonReconciled(ctx, pricingController)

			nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: currentInstance.Name,
						v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
						corev1.LabelTopologyZone:       "test-zone-1",
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

			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ctReq := cmds[0].Replacements[0].Requirements.Get(v1.CapacityTypeLabelKey)
			Expect(ctReq.Has(v1.CapacityTypeSpot)).To(Equal(spotAvailable))
			Expect(ctReq.Has(v1.CapacityTypeOnDemand)).To(Equal(!spotAvailable))
		},
			Entry("when a Spot offering is available", true),
			Entry("when no Spot offering is available", false),
		)
		It("should price candidates with the NodeClaimPriceProvider", func() {
			cloudprovider.SetNodeClaimPriceProvider(fixedPricer(0.001))
			DeferCleanup(test.ResetNodeClaimPriceProvider)
			pod := replicaSetPod()
			ExpectApplied(ctx, env.Client, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, func(context.Context, *disruption.Candidate) bool { return true },
				disruption.GracefulDisruptionClass, queue)
			Expect(err).ToNot(HaveOccurred())
			Expect(candidates).To(HaveLen(1))
			Expect(candidates[0].Price).To(Equal(0.001))
		})
		It("should not replace a node that the NodeClaimPriceProvider prices below every replacement", func() {
			cloudprovider.SetNodeClaimPriceProvider(fixedPricer(0.0001))
			DeferCleanup(test.ResetNodeClaimPriceProvider)
			disruptionController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
				disruption.WithMethods(NewMethodsWithNopValidator()...))
			pod := replicaSetPod()
			ExpectApplied(ctx, env.Client, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(BeEmpty())
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("should replace the same node when it's priced by its offering", func() {
			disruptionController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
				disruption.WithMethods(NewMethodsWithNopValidator()...))
			pod := replicaSetPod()
			ExpectApplied(ctx, env.Client, pod, node, nodeClaim, nodePool)
			ExpectManualBinding(ctx, env.Client, pod, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			Expect(queue.GetCommands()).To(HaveLen(1))
		})
		It("cannot replace spot with spot if less than minimum InstanceTypes flexibility", func() {
			// Forcefully shrink the possible instanceTypes to be lower than 15 to replace a nodeclaim
			cloudProvider.InstanceTypes = lo.Slice(fake.InstanceTypesAssorted(), 0, 5)
			// Forcefully assign lowest possible instancePrice to make sure we have atleast one instance
			// that is lower than the current node.
			cloudProvider.InstanceTypes[0].Offerings[0].Price = 0.001
			cloudProvider.InstanceTypes[0].Offerings[0].Requirements[v1.CapacityTypeLabelKey] = scheduling.NewRequirement(
				v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeSpot)
			spotInstances = lo.Filter(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
				for _, o := range i.Offerings {
					if o.Requirements.Get(v1.CapacityTypeLabelKey).Any() == v1.CapacityTypeSpot {
						return true
					}
				}
				return false
			})
			// Sort the spot instances by pricing from low to high
			sort.Slice(spotInstances, func(i, j int) bool {
				return spotInstances[i].Offerings.Cheapest().Price < spotInstances[j].Offerings.Cheapest().Price
			})
			mostExpSpotInstance := spotInstances[len(spotInstances)-1]
			mostExpSpotOffering := mostExpSpotInstance.Offerings[0]
			spotNodeClaim.Labels = lo.Assign(spotNodeClaim.Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: mostExpSpotInstance.Name,
				v1.CapacityTypeLabelKey:        mostExpSpotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpSpotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})

			spotNode.Labels = lo.Assign(spotNode.Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: mostExpSpotInstance.Name,
				v1.CapacityTypeLabelKey:        mostExpSpotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpSpotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})

			ExpectSingletonReconciled(ctx, pricingController)
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pod, spotNode, spotNodeClaim, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, spotNode)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{spotNode}, []*v1.NodeClaim{spotNodeClaim})

			// consolidation won't delete the old nodeclaim until the new nodeclaim is ready
			ExpectSingletonReconciled(ctx, disruptionController)
			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)

			// shouldn't delete the node
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))

			// Expect Unconsolidatable events to be fired
			_, ok := lo.Find(recorder.Events(), func(e events.Event) bool {
				return strings.Contains(e.Message, fmt.Sprintf("SpotToSpotConsolidation requires %d cheaper instance type options than the current candidate to consolidate, got %d",
					disruption.MinInstanceTypesForSpotToSpotConsolidation, 1))
			})
			Expect(ok).To(BeTrue())
		})
		It("cannot replace spot with spot if the spotToSpotConsolidation is disabled", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{SpotToSpotConsolidation: new(false)}}))
			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pod, spotNode, spotNodeClaim, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, spotNode)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{spotNode}, []*v1.NodeClaim{spotNodeClaim})

			// consolidation won't delete the old nodeclaim until the new nodeclaim is ready
			ExpectSingletonReconciled(ctx, disruptionController)
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))

			// shouldn't delete the node
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))

			// Expect Unconsolidatable events to be fired
			_, ok := lo.Find(recorder.Events(), func(e events.Event) bool {
				return strings.Contains(e.Message, "SpotToSpotConsolidation is disabled, can't replace a spot node with a spot node")
			})
			Expect(ok).To(BeTrue())
		})
		It("cannot replace spot with spot if it is part of the 15 cheapest instance types.", func() {
			cloudProvider.InstanceTypes = lo.Slice(fake.InstanceTypesAssorted(), 0, 20)
			// Forcefully assign lowest possible instancePrice to make sure we have atleast one instance
			// that is lower than the current node.
			cloudProvider.InstanceTypes[0].Offerings[0].Price = 0.001
			cloudProvider.InstanceTypes[0].Offerings[0].Requirements[v1.CapacityTypeLabelKey] = scheduling.NewRequirement(
				v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeSpot)
			// Also sort the cloud provider instances by pricing from low to high
			sort.Slice(cloudProvider.InstanceTypes, func(i, j int) bool {
				return cloudProvider.InstanceTypes[i].Offerings.Cheapest().Price < cloudProvider.InstanceTypes[j].Offerings.Cheapest().Price
			})
			spotInstances = lo.Filter(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
				for _, o := range i.Offerings {
					if o.Requirements.Get(v1.CapacityTypeLabelKey).Any() == v1.CapacityTypeSpot {
						return true
					}
				}
				return false
			})

			spotInstance := spotInstances[1]
			spotOffering := spotInstance.Offerings[0]
			spotNodeClaim.Labels = lo.Assign(spotNodeClaim.Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: spotInstance.Name,
				v1.CapacityTypeLabelKey:        spotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       spotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})

			spotNode.Labels = lo.Assign(spotNode.Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: spotInstance.Name,
				v1.CapacityTypeLabelKey:        spotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       spotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})
			ExpectSingletonReconciled(ctx, pricingController)

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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pod, spotNode, spotNodeClaim, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, spotNode)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{spotNode}, []*v1.NodeClaim{spotNodeClaim})

			// consolidation won't delete the old nodeclaim until the new nodeclaim is ready
			ExpectSingletonReconciled(ctx, disruptionController)

			// we didn't create a new nodeclaim or delete the old one
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, spotNodeClaim)
			ExpectExists(ctx, env.Client, spotNode)
		})
		It("spot to spot consolidation should order the instance types by price before enforcing minimum flexibility.", func() {
			// Fetch 18 spot instances
			spotInstances = lo.Slice(lo.Filter(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
				for _, o := range i.Offerings {
					if o.Requirements.Get(v1.CapacityTypeLabelKey).Any() == v1.CapacityTypeSpot {
						return true
					}
				}
				return false
			}), 0, 18)
			// Assign the prices for 18 spot instance in ascending order incrementally
			for i, inst := range spotInstances {
				inst.Offerings[0].Price = 1.00 + float64(i)*0.1
			}
			// Force an instancetype that is outside the bound of 15 instances to have the cheapest price among the lot.
			spotInstances[16].Offerings[0].Price = 0.001

			// We now have these spot instance in the list as lowest priced and highest priced instanceTypes
			cheapestSpotInstanceType := spotInstances[16]
			mostExpensiveInstanceType := spotInstances[17]

			// Add these spot instance with this special condition to cloud provider instancetypes
			cloudProvider.InstanceTypes = spotInstances

			expectedInstanceTypesForConsolidation := make([]*cloudprovider.InstanceType, len(spotInstances))
			copy(expectedInstanceTypesForConsolidation, spotInstances)
			// Sort the spot instances by pricing from low to high
			sort.Slice(expectedInstanceTypesForConsolidation, func(i, j int) bool {
				return expectedInstanceTypesForConsolidation[i].Offerings[0].Price < expectedInstanceTypesForConsolidation[j].Offerings[0].Price
			})
			// These 15 cheapest instance types should eventually be considered for consolidation.
			var expectedInstanceTypesNames []string
			for i := range 15 {
				expectedInstanceTypesNames = append(expectedInstanceTypesNames, expectedInstanceTypesForConsolidation[i].Name)
			}

			// Assign the most expensive spot instancetype so that it will definitely be replaced through consolidation
			spotNodeClaim.Labels = lo.Assign(spotNodeClaim.Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: mostExpensiveInstanceType.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
			})

			spotNode.Labels = lo.Assign(spotNode.Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: mostExpensiveInstanceType.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
			})

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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pod, spotNode, spotNodeClaim, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, spotNode)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{spotNode}, []*v1.NodeClaim{spotNodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, spotNodeClaim)
			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, spotNodeClaim)

			// should create a new nodeclaim as there is a cheaper one that can hold the pod
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodes).To(HaveLen(1))

			// Expect that the new nodeclaim does not request the most expensive instance type
			Expect(nodeClaims[0].Name).ToNot(Equal(spotNodeClaim.Name))
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Has(corev1.LabelInstanceTypeStable)).To(BeTrue())
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(mostExpensiveInstanceType.Name)).To(BeFalse())

			// Make sure that the cheapest instance that was outside the bound of 15 instance types is considered for consolidation.
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(cheapestSpotInstanceType.Name)).To(BeTrue())
			spotInstancesConsideredForConsolidation := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Values()

			// Make sure that we send only 15 instance types.
			Expect(len(spotInstancesConsideredForConsolidation)).To(Equal(15))

			// Make sure we considered the first 15 cheapest instance types.
			for i := range 15 {
				Expect(spotInstancesConsideredForConsolidation).To(ContainElement(expectedInstanceTypesNames[i]))
			}

			// and delete the old one
			ExpectNotFound(ctx, env.Client, spotNodeClaim, spotNode)
		})
		It("spot to spot consolidation should consider the max of default and minimum number of instanceTypeOptions from minValues in requirement for truncation if minimum number of instanceTypeOptions from minValues in requirement is greater than 15.", func() {
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
				{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpExists,

					MinValues: new(16),
				},
			}
			// Fetch 18 spot instances
			spotInstances = lo.Slice(lo.Filter(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
				for _, o := range i.Offerings {
					if o.Requirements.Get(v1.CapacityTypeLabelKey).Any() == v1.CapacityTypeSpot {
						return true
					}
				}
				return false
			}), 0, 18)
			// Assign the prices for 18 spot instance in ascending order incrementally
			for i, inst := range spotInstances {
				inst.Offerings[0].Price = 1.00 + float64(i)*0.1
			}
			// Force an instancetype that is outside the bound of 15 instances to have the cheapest price among the lot.
			spotInstances[16].Offerings[0].Price = 0.001

			// We now have these spot instance in the list as lowest priced and highest priced instanceTypes
			cheapestSpotInstanceType := spotInstances[16]
			mostExpensiveInstanceType := spotInstances[17]

			// Add these spot instance with this special condition to cloud provider instancetypes
			cloudProvider.InstanceTypes = spotInstances

			expectedInstanceTypesForConsolidation := make([]*cloudprovider.InstanceType, len(spotInstances))
			copy(expectedInstanceTypesForConsolidation, spotInstances)
			// Sort the spot instances by pricing from low to high
			sort.Slice(expectedInstanceTypesForConsolidation, func(i, j int) bool {
				return expectedInstanceTypesForConsolidation[i].Offerings[0].Price < expectedInstanceTypesForConsolidation[j].Offerings[0].Price
			})
			// These 15 cheapest instance types should eventually be considered for consolidation.
			var expectedInstanceTypesNames []string
			for i := range 16 {
				expectedInstanceTypesNames = append(expectedInstanceTypesNames, expectedInstanceTypesForConsolidation[i].Name)
			}

			// Assign the most expensive spot instancetype so that it will definitely be replaced through consolidation
			spotNodeClaim.Labels = lo.Assign(spotNodeClaim.Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: mostExpensiveInstanceType.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
			})

			spotNode.Labels = lo.Assign(spotNode.Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: mostExpensiveInstanceType.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
			})

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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pod, spotNode, spotNodeClaim, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, spotNode)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{spotNode}, []*v1.NodeClaim{spotNodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, spotNodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, spotNodeClaim)

			// should create a new nodeclaim as there is a cheaper one that can hold the pod
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodes).To(HaveLen(1))

			// Expect that the new nodeclaim does not request the most expensive instance type
			Expect(nodeClaims[0].Name).ToNot(Equal(spotNodeClaim.Name))
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Has(corev1.LabelInstanceTypeStable)).To(BeTrue())
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(mostExpensiveInstanceType.Name)).To(BeFalse())

			// Make sure that the cheapest instance that was outside the bound of 15 instance types is considered for consolidation.
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(cheapestSpotInstanceType.Name)).To(BeTrue())
			spotInstancesConsideredForConsolidation := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Values()

			// Make sure that we send only 16 instance types.
			Expect(len(spotInstancesConsideredForConsolidation)).To(Equal(16))

			// Make sure we considered the first 16 cheapest instance types.
			for i := range 16 {
				Expect(spotInstancesConsideredForConsolidation).To(ContainElement(expectedInstanceTypesNames[i]))
			}

			// and delete the old one
			ExpectNotFound(ctx, env.Client, spotNodeClaim, spotNode)
		})
		It("should handle failing filterOutSameInstanceType correctly (without a panic) when minValues isn't satisfied", func() {
			// Create a NodePool that has minValues in requirement
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
				{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpExists,

					MinValues: new(2),
				},
				{
					Key:      v1.CapacityTypeLabelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{v1.CapacityTypeOnDemand},
				},
			}
			currentInstanceType := fake.NewInstanceType("current-on-demand",
				fake.WithResources(corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("5"),
				}),
				fake.WithOfferings(cloudprovider.Offering{
					Available:    true,
					Compatible:   true,
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
					Price:        0.5,
				}),
			)
			otherInstanceType := fake.NewInstanceType("other-on-demand",
				fake.WithResources(corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("5"),
				}),
				fake.WithOfferings(cloudprovider.Offering{
					Available:    true,
					Compatible:   true,
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
					Price:        0.4,
				}),
			)
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				currentInstanceType,
				otherInstanceType,
			}
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: currentInstanceType.Name,
						v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
						corev1.LabelTopologyZone:       "test-zone-1a",
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("4"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			for i := range nodeClaims {
				nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}
			ExpectApplied(ctx, env.Client, nodePool)
			ExpectApplied(ctx, env.Client, lo.Map(nodeClaims, func(o *v1.NodeClaim, _ int) client.Object { return o })...)
			ExpectApplied(ctx, env.Client, lo.Map(nodes, func(o *corev1.Node, _ int) client.Object { return o })...)
			pods := test.Pods(4, test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("2"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, lo.Map(pods, func(o *corev1.Pod, _ int) client.Object { return o })...)

			// Schedule a single pod to each of the first two nodes and two pods to the third
			// Expect that the first two nodes should attempt to multi-node consolidate into each other with a replacement
			// When multi-node consolidation is performed it should hit the edge case where minValues is not satisfied after
			// performing filterOutSameInstanceType since, when the first two nodes combine, there are only two options available
			// for replacement; however, one of them is the same type as the nodes already are, meaning it will get filtered out
			// and no longer satisfy the minValues for the NodePool requirement
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[2])
			ExpectManualBinding(ctx, env.Client, pods[3], nodes[2])
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
			ExpectSingletonReconciled(ctx, disruptionController)
			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims...)

			// Eventually expect consolidation to evaluate that it can delete one of the first two nodes
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		})
		It("spot to spot consolidation should consider the default for truncation if minimum number of instanceTypeOptions from minValues in requirement is less than 15.", func() {
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
				{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpExists,

					MinValues: new(10),
				},
			}
			// Fetch 18 spot instances
			spotInstances = lo.Slice(lo.Filter(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
				for _, o := range i.Offerings {
					if o.Requirements.Get(v1.CapacityTypeLabelKey).Any() == v1.CapacityTypeSpot {
						return true
					}
				}
				return false
			}), 0, 18)
			// Assign the prices for 18 spot instance in ascending order incrementally
			for i, inst := range spotInstances {
				inst.Offerings[0].Price = 1.00 + float64(i)*0.1
			}
			// Force an instancetype that is outside the bound of 15 instances to have the cheapest price among the lot.
			spotInstances[16].Offerings[0].Price = 0.001

			// We now have these spot instance in the list as lowest priced and highest priced instanceTypes
			cheapestSpotInstanceType := spotInstances[16]
			mostExpensiveInstanceType := spotInstances[17]

			// Add these spot instance with this special condition to cloud provider instancetypes
			cloudProvider.InstanceTypes = spotInstances

			expectedInstanceTypesForConsolidation := make([]*cloudprovider.InstanceType, len(spotInstances))
			copy(expectedInstanceTypesForConsolidation, spotInstances)
			// Sort the spot instances by pricing from low to high
			sort.Slice(expectedInstanceTypesForConsolidation, func(i, j int) bool {
				return expectedInstanceTypesForConsolidation[i].Offerings[0].Price < expectedInstanceTypesForConsolidation[j].Offerings[0].Price
			})
			// These 15 cheapest instance types should eventually be considered for consolidation.
			var expectedInstanceTypesNames []string
			for i := range 15 {
				expectedInstanceTypesNames = append(expectedInstanceTypesNames, expectedInstanceTypesForConsolidation[i].Name)
			}

			// Assign the most expensive spot instancetype so that it will definitely be replaced through consolidation
			spotNodeClaim.Labels = lo.Assign(spotNodeClaim.Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: mostExpensiveInstanceType.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
			})

			spotNode.Labels = lo.Assign(spotNode.Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: mostExpensiveInstanceType.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
			})

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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pod, spotNode, spotNodeClaim, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, spotNode)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{spotNode}, []*v1.NodeClaim{spotNodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, spotNodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, spotNodeClaim)

			// should create a new nodeclaim as there is a cheaper one that can hold the pod
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodes).To(HaveLen(1))

			// Expect that the new nodeclaim does not request the most expensive instance type
			Expect(nodeClaims[0].Name).ToNot(Equal(spotNodeClaim.Name))
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Has(corev1.LabelInstanceTypeStable)).To(BeTrue())
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(mostExpensiveInstanceType.Name)).To(BeFalse())

			// Make sure that the cheapest instance that was outside the bound of 15 instance types is considered for consolidation.
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(cheapestSpotInstanceType.Name)).To(BeTrue())
			spotInstancesConsideredForConsolidation := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Values()

			// Make sure that we send only 15 instance types.
			Expect(len(spotInstancesConsideredForConsolidation)).To(Equal(15))

			// Make sure we considered the first 15 cheapest instance types.
			for i := range 15 {
				Expect(spotInstancesConsideredForConsolidation).To(ContainElement(expectedInstanceTypesNames[i]))
			}

			// and delete the old one
			ExpectNotFound(ctx, env.Client, spotNodeClaim, spotNode)
		})
		DescribeTable("Consolidation should fail if filterByPrice breaks the minimum requirement from the NodePools.",
			func(spotToSpot bool) {
				// Create a NodePool that has minValues in requirement
				nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
					{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpExists,

						MinValues: new(16),
					},
				}
				// Fetch 18 spot instances
				spotInstances = lo.Slice(spotInstances, 0, 18)
				// Fetch 18 od instances
				onDemandInstances = lo.Slice(onDemandInstances, 0, 18)

				// We now have these spot instance in the list as lowest priced and highest priced instanceTypes
				// This means that we have 15 instance types to replace spot instance which is enough for consolidation
				// but note that we have minValues for instanceTypes as 16. So, we should fail the consolidation.
				mostExpensiveInstanceType := spotInstances[15]
				mostExpensiveODInstanceType := onDemandInstances[15]

				// Assign borderline instanceType as the most expensive so that we have exactly 15 instances to replace for consolidation.
				spotNodeClaim.Labels = lo.Assign(spotNodeClaim.Labels, map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: mostExpensiveInstanceType.Name,
					v1.CapacityTypeLabelKey:        mostExpensiveInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       mostExpensiveInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
				})

				spotNode.Labels = lo.Assign(spotNode.Labels, map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: mostExpensiveInstanceType.Name,
					v1.CapacityTypeLabelKey:        mostExpensiveInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       mostExpensiveInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
				})

				nodeClaim.Labels = lo.Assign(spotNodeClaim.Labels, map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: mostExpensiveODInstanceType.Name,
					v1.CapacityTypeLabelKey:        mostExpensiveODInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       mostExpensiveODInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
				})

				node.Labels = lo.Assign(spotNode.Labels, map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: mostExpensiveODInstanceType.Name,
					v1.CapacityTypeLabelKey:        mostExpensiveODInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       mostExpensiveODInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
				})

				nodeClaim = lo.Ternary(spotToSpot, spotNodeClaim, nodeClaim)
				node = lo.Ternary(spotToSpot, spotNode, node)
				// Add these spot instance with this special condition to cloud provider instancetypes
				cloudProvider.InstanceTypes = append(cloudProvider.InstanceTypes, lo.Ternary(spotToSpot, spotInstances, onDemandInstances)...)
				ExpectSingletonReconciled(ctx, pricingController)

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
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}}})
				ExpectApplied(ctx, env.Client, rs, pod, spotNode, spotNodeClaim, nodePool)

				// bind pods to node
				ExpectManualBinding(ctx, env.Client, pod, spotNode)

				// inform cluster state about nodes and nodeclaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{spotNode}, []*v1.NodeClaim{spotNodeClaim})

				// consolidation won't delete the old nodeclaim until the new nodeclaim is ready
				ExpectSingletonReconciled(ctx, disruptionController)

				// we didn't create a new nodeclaim or delete the old one
				Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
				Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
				ExpectExists(ctx, env.Client, spotNodeClaim)
				ExpectExists(ctx, env.Client, spotNode)
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		DescribeTable("can replace nodes if another nodePool returns no instance types",
			func(spotToSpot bool) {
				nodeClaim = lo.Ternary(spotToSpot, spotNodeClaim, nodeClaim)
				node = lo.Ternary(spotToSpot, spotNode, node)
				// create our RS so we can link a pod to it
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
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}}})

				nodePool2 := test.NodePool()
				cloudProvider.InstanceTypesForNodePool[nodePool2.Name] = nil
				ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool, nodePool2)

				// bind pods to node
				ExpectManualBinding(ctx, env.Client, pod, node)

				// inform cluster state about nodes and nodeclaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
				ExpectSingletonReconciled(ctx, disruptionController)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
				ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)

				// Cascade any deletion of the nodeclaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

				// should create a new nodeclaim as there is a cheaper one that can hold the pod
				nodeclaims := ExpectNodeClaims(ctx, env.Client)
				nodes := ExpectNodes(ctx, env.Client)
				Expect(nodeclaims).To(HaveLen(1))
				Expect(nodes).To(HaveLen(1))

				// Expect that the new nodeclaim does not request the most expensive instance type
				Expect(nodeclaims[0].Name).ToNot(Equal(nodeClaim.Name))
				Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeclaims[0].Spec.Requirements...).Has(corev1.LabelInstanceTypeStable)).To(BeTrue())
				Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeclaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(mostExpensiveInstance.Name)).To(BeFalse())

				// and delete the old one
				ExpectNotFound(ctx, env.Client, nodeClaim, node)
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		DescribeTable("can replace nodes, considers PDB",
			func(spotToSpot bool) {
				nodeClaim = lo.Ternary(spotToSpot, spotNodeClaim, nodeClaim)
				node = lo.Ternary(spotToSpot, spotNode, node)
				// create our RS so we can link a pod to it
				rs := test.ReplicaSet()
				ExpectApplied(ctx, env.Client, rs)
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

				pods := test.Pods(3, test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: labels,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "ReplicaSet",
								Name:               rs.Name,
								UID:                rs.UID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}}})
				pdb := test.PodDisruptionBudget(test.PDBOptions{
					Labels:         labels,
					MaxUnavailable: fromInt(0),
					Status: &policyv1.PodDisruptionBudgetStatus{
						ObservedGeneration: 1,
						DisruptionsAllowed: 0,
						CurrentHealthy:     1,
						DesiredHealthy:     1,
						ExpectedPods:       1,
					},
				})

				ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodePool, pdb)

				// bind pods to node
				ExpectManualBinding(ctx, env.Client, pods[0], node)
				ExpectManualBinding(ctx, env.Client, pods[1], node)
				ExpectManualBinding(ctx, env.Client, pods[2], node)

				// inform cluster state about nodes and nodeclaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
				ExpectSingletonReconciled(ctx, disruptionController)

				// we didn't create a new nodeclaim or delete the old one
				Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
				Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
				ExpectExists(ctx, env.Client, nodeClaim)
				ExpectExists(ctx, env.Client, node)
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		DescribeTable("can replace nodes, considers PDB policy",
			func(spotToSpot bool) {
				nodeClaim = lo.Ternary(spotToSpot, spotNodeClaim, nodeClaim)
				node = lo.Ternary(spotToSpot, spotNode, node)
				if env.Version.Minor() < 27 {
					Skip("PDB policy ony enabled by default for K8s >= 1.27.x")
				}
				// create our RS so we can link a pod to it
				rs := test.ReplicaSet()
				ExpectApplied(ctx, env.Client, rs)
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

				pods := test.Pods(3, test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: labels,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "ReplicaSet",
								Name:               rs.Name,
								UID:                rs.UID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}}})

				pdb := test.PodDisruptionBudget(test.PDBOptions{
					Labels:         labels,
					MaxUnavailable: fromInt(0),
					Status: &policyv1.PodDisruptionBudgetStatus{
						ObservedGeneration: 1,
						DisruptionsAllowed: 0,
						CurrentHealthy:     1,
						DesiredHealthy:     1,
						ExpectedPods:       1,
					},
				})
				alwaysAllow := policyv1.AlwaysAllow
				pdb.Spec.UnhealthyPodEvictionPolicy = &alwaysAllow

				ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodePool, pdb)

				// bind pods to node
				ExpectManualBinding(ctx, env.Client, pods[0], node)
				ExpectManualBinding(ctx, env.Client, pods[1], node)
				ExpectManualBinding(ctx, env.Client, pods[2], node)

				// set all of these pods to unhealthy so the PDB won't stop their eviction
				for _, p := range pods {
					p.Status.Conditions = []corev1.PodCondition{
						{
							Type:               corev1.PodReady,
							Status:             corev1.ConditionFalse,
							LastProbeTime:      metav1.Now(),
							LastTransitionTime: metav1.Now(),
						},
					}
					ExpectApplied(ctx, env.Client, p)
				}

				// inform cluster state about nodes and nodeclaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
				ExpectSingletonReconciled(ctx, disruptionController)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
				ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)

				// Cascade any deletion of the nodeclaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

				// should create a new nodeclaim as there is a cheaper one that can hold the pod
				nodeclaims := ExpectNodeClaims(ctx, env.Client)
				nodes := ExpectNodes(ctx, env.Client)
				Expect(nodeclaims).To(HaveLen(1))
				Expect(nodes).To(HaveLen(1))

				// we didn't create a new nodeclaim or delete the old one
				Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
				Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
				ExpectNotFound(ctx, env.Client, nodeClaim, node)
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		DescribeTable("can replace nodes, PDB namespace must match",
			func(spotToSpot bool) {
				nodeClaim = lo.Ternary(spotToSpot, spotNodeClaim, nodeClaim)
				node = lo.Ternary(spotToSpot, spotNode, node)
				// create our RS so we can link a pod to it
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
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}}})

				namespace := test.Namespace()
				pdb := test.PodDisruptionBudget(test.PDBOptions{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespace.Name,
					},
					Labels:         labels,
					MaxUnavailable: fromInt(0),
					Status: &policyv1.PodDisruptionBudgetStatus{
						ObservedGeneration: 1,
						DisruptionsAllowed: 0,
						CurrentHealthy:     1,
						DesiredHealthy:     1,
						ExpectedPods:       1,
					},
				})

				// bind pods to node
				ExpectApplied(ctx, env.Client, rs, pod, nodeClaim, node, nodePool, namespace, pdb)
				ExpectManualBinding(ctx, env.Client, pod, node)

				// inform cluster state about nodes and nodeclaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
				ExpectSingletonReconciled(ctx, disruptionController)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
				ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)

				// Cascade any deletion of the nodeclaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

				// should create a new nodeclaim as there is a cheaper one that can hold the pod
				Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
				Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
				ExpectNotFound(ctx, env.Client, nodeClaim, node)
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		DescribeTable("can replace nodes, considers karpenter.sh/do-not-disrupt set to true on nodes",
			func(spotToSpot bool) {
				nodeClaim = lo.Ternary(spotToSpot, spotNodeClaim, nodeClaim)
				node = lo.Ternary(spotToSpot, spotNode, node)
				// create our RS so we can link a pod to it
				rs := test.ReplicaSet()
				ExpectApplied(ctx, env.Client, rs)
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

				pods := test.Pods(3, test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: labels,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "ReplicaSet",
								Name:               rs.Name,
								UID:                rs.UID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						},
					},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("2"),
						},
					},
				})
				annotatedNodeClaim, annotatedNode := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							v1.DoNotDisruptAnnotationKey: "true",
						},
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
							v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
							corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
						},
					},
					Status: v1.NodeClaimStatus{
						Allocatable: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:  resource.MustParse("5"),
							corev1.ResourcePods: resource.MustParse("100"),
						},
					},
				})

				if spotToSpot {
					annotatedNodeClaim.Labels = lo.Assign(annotatedNodeClaim.Labels, map[string]string{
						corev1.LabelInstanceTypeStable: mostExpensiveSpotInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveSpotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveSpotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					})
					annotatedNode.Labels = lo.Assign(annotatedNode.Labels, map[string]string{
						corev1.LabelInstanceTypeStable: mostExpensiveSpotInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveSpotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveSpotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					})
				}

				ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
				ExpectApplied(ctx, env.Client, nodeClaim, node, annotatedNodeClaim, annotatedNode)

				// bind pods to node
				ExpectManualBinding(ctx, env.Client, pods[0], node)
				ExpectManualBinding(ctx, env.Client, pods[1], node)
				ExpectManualBinding(ctx, env.Client, pods[2], annotatedNode)

				// inform cluster state about nodes and nodeClaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node, annotatedNode}, []*v1.NodeClaim{nodeClaim, annotatedNodeClaim})
				ExpectSingletonReconciled(ctx, disruptionController)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
				ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)
				// Cascade any deletion of the nodeClaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

				// we should delete the non-annotated node and replace with a cheaper node
				Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
				Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
				ExpectNotFound(ctx, env.Client, nodeClaim, node)
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		DescribeTable("can replace nodes, considers karpenter.sh/do-not-disrupt set to true on pods",
			func(spotToSpot bool) {
				nodeClaim = lo.Ternary(spotToSpot, spotNodeClaim, nodeClaim)
				node = lo.Ternary(spotToSpot, spotNode, node)
				// create our RS so we can link a pod to it
				rs := test.ReplicaSet()
				ExpectApplied(ctx, env.Client, rs)
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

				pods := test.Pods(3, test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: labels,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "ReplicaSet",
								Name:               rs.Name,
								UID:                rs.UID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						},
					},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("2"),
						},
					},
				})
				nodeClaim2, node2 := test.NodeClaimAndNode(v1.NodeClaim{
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
							corev1.ResourceCPU:  resource.MustParse("5"),
							corev1.ResourcePods: resource.MustParse("100"),
						},
					},
				})
				if spotToSpot {
					nodeClaim2.Labels = lo.Assign(nodeClaim2.Labels, map[string]string{
						corev1.LabelInstanceTypeStable: mostExpensiveSpotInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveSpotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveSpotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					})
					node2.Labels = lo.Assign(node2.Labels, map[string]string{
						corev1.LabelInstanceTypeStable: mostExpensiveSpotInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveSpotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveSpotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					})
				}
				// Block this pod from being disrupted with karpenter.sh/do-not-disrupt
				pods[2].Annotations = lo.Assign(pods[2].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})

				ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
				ExpectApplied(ctx, env.Client, nodeClaim, node, nodeClaim2, node2)

				// bind pods to node
				ExpectManualBinding(ctx, env.Client, pods[0], node)
				ExpectManualBinding(ctx, env.Client, pods[1], node)
				ExpectManualBinding(ctx, env.Client, pods[2], node2)

				// inform cluster state about nodes and nodeClaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node, node2}, []*v1.NodeClaim{nodeClaim, nodeClaim2})
				ExpectSingletonReconciled(ctx, disruptionController)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
				ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)

				// Cascade any deletion of the nodeclaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

				// we should delete the non-annotated node and replace with a cheaper node
				Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
				Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
				ExpectNotFound(ctx, env.Client, nodeClaim, node)
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		It("won't replace node if any spot replacement is more expensive", func() {
			currentInstance := fake.NewInstanceType("current-on-demand",
				fake.WithOfferings(cloudprovider.Offering{
					Available:    false,
					Compatible:   true,
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
					Price:        0.5,
				}),
			)
			replacementInstance := fake.NewInstanceType("potential-spot-replacement",
				fake.WithOfferings(
					cloudprovider.Offering{
						Available:    true,
						Compatible:   true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeSpot, corev1.LabelTopologyZone: "test-zone-1a"}),
						Price:        1.0,
					},
					cloudprovider.Offering{
						Available:    true,
						Compatible:   true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeSpot, corev1.LabelTopologyZone: "test-zone-1b"}),
						Price:        0.2,
					},
					cloudprovider.Offering{
						Available:    true,
						Compatible:   true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeSpot, corev1.LabelTopologyZone: "test-zone-1c"}),
						Price:        0.4,
					},
				),
			)
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				currentInstance,
				replacementInstance,
			}
			ExpectSingletonReconciled(ctx, pricingController)

			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: currentInstance.Name,
						v1.CapacityTypeLabelKey:        currentInstance.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       currentInstance.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32")},
				},
			})

			ExpectApplied(ctx, env.Client, rs, pod, nodeClaim, node, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
			ExpectExists(ctx, env.Client, node)
		})
		It("won't replace on-demand node if on-demand replacement is more expensive", func() {
			currentInstance := fake.NewInstanceType("current-on-demand",
				fake.WithOfferings(cloudprovider.Offering{
					Available:    false,
					Compatible:   true,
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
					Price:        0.5,
				}),
			)
			replacementInstance := fake.NewInstanceType("on-demand-replacement",
				fake.WithOfferings(
					cloudprovider.Offering{
						Available:    true,
						Compatible:   true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
						Price:        0.6,
					},
					cloudprovider.Offering{
						Available:    true,
						Compatible:   true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1b"}),
						Price:        0.6,
					},
					cloudprovider.Offering{
						Available:    true,
						Compatible:   true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeSpot, corev1.LabelTopologyZone: "test-zone-1b"}),
						Price:        0.2,
					},
					cloudprovider.Offering{
						Available:    true,
						Compatible:   true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeSpot, corev1.LabelTopologyZone: "test-zone-1c"}),
						Price:        0.3,
					},
				),
			)

			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				currentInstance,
				replacementInstance,
			}
			ExpectSingletonReconciled(ctx, pricingController)

			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			// nodePool should require on-demand instance for this test case
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
				{
					Key:      v1.CapacityTypeLabelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{v1.CapacityTypeOnDemand},
				},
			}
			nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: currentInstance.Name,
						v1.CapacityTypeLabelKey:        currentInstance.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       currentInstance.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32")},
				},
			})

			ExpectApplied(ctx, env.Client, rs, pod, nodeClaim, node, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
			ExpectExists(ctx, env.Client, node)
		})
	})
	Context("Delete", func() {
		var nodeClaims []*v1.NodeClaim
		var nodes []*corev1.Node

		BeforeEach(func() {
			nodeClaims, nodes = test.NodeClaimsAndNodes(2, v1.NodeClaim{
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
			for _, nc := range nodeClaims {
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}
		})
		It("can delete nodes", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])
		})
		It("does not delete nodes with pod churn, deletes nodes without pod churn", func() {
			// create our RS so we can link a pod to it
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range 2 {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			sort.Slice(nodes, func(i, j int) bool {
				return nodes[i].Name < nodes[j].Name
			})

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
			emptyConsolidation := disruption.NewEmptiness(c, disruption.WithValidator(NewTestEmptinessValidator(nodes, nodeClaims, nodePool, WithEmptinessChurn())))
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, emptyConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, emptyConsolidation.ShouldDisrupt, emptyConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			// this test validator invalidates the command because it creates pod churn during validaiton
			cmds, err := emptyConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).To(Succeed())
			Expect(len(cmds)).To(Equal(1))
			for _, cmd := range cmds {
				Expect(cmd.Results).To(Equal(pscheduling.Results{}))
				Expect(cmd.Candidates).To(HaveLen(1))
				// the test validator manually binds a pod to nodes[0], causing it to no longer be eligible
				Expect(cmd.Candidates[0].StateNode.Node.Name).To(Equal(nodes[1].Name))
				Expect(cmd.Decision()).To(Equal(disruption.DeleteDecision))
			}

			Expect(emptyConsolidation.IsConsolidated()).To(BeFalse())
		})
		It("can delete nodes if another nodePool has no node template", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			nodeClassNodePool := test.NodePool()
			nodeClassNodePool.Spec.Template.Spec.NodeClassRef = nil
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])
		})
		It("can delete nodes, when non-Karpenter capacity can fit pods", func() {
			unmanagedNode := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU:  resource.MustParse("32"),
					corev1.ResourcePods: resource.MustParse("100"),
				},
			})
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], unmanagedNode, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[0])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], unmanagedNode}, []*v1.NodeClaim{nodeClaims[0]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaims[0])

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0])

			// we can fit all of our pod capacity on the unmanaged node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0])
		})
		It("can delete nodes, considers PDB", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			// only pod[2] is covered by the PDB
			pods[2].Labels = labels
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         labels,
				MaxUnavailable: fromInt(0),
				Status: &policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: 1,
					DisruptionsAllowed: 0,
					CurrentHealthy:     1,
					DesiredHealthy:     1,
					ExpectedPods:       1,
				},
			})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool, pdb)

			// two pods on node 1
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			// one on node 2, but it has a PDB with zero disruptions allowed
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaims[0])

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0])

			// we don't need a new node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// but we expect to delete the nodeclaim with more pods (node) as the pod on nodeClaim2 has a PDB preventing
			// eviction
			ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0])
		})
		It("can delete nodes, considers karpenter.sh/do-not-disrupt set to true on nodes", func() {
			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			nodeClaims[1].Annotations = lo.Assign(nodeClaims[1].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
			nodes[1].Annotations = lo.Assign(nodeClaims[1].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaims[0])
			// Cascade any deletion of the nodeClaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0])

			// we should delete the non-annotated node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0])
		})
		It("can delete nodes, considers karpenter.sh/do-not-disrupt set to true on pods", func() {
			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			// Block this pod from being disrupted with karpenter.sh/do-not-disrupt
			pods[2].Annotations = lo.Assign(pods[2].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaims[0])

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0])

			// we should delete the non-annotated node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0])
		})
		It("does not consolidate nodes with karpenter.sh/do-not-disrupt set to true on pods when the NodePool's TerminationGracePeriod is not nil", func() {
			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			// Block this pod from being disrupted with karpenter.sh/do-not-disrupt
			pods[0].Annotations = lo.Assign(pods[0].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
			pods[1].Annotations = lo.Assign(pods[1].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
			pods[2].Annotations = lo.Assign(pods[2].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})

			nodeClaims[0].Spec.TerminationGracePeriod = &metav1.Duration{Duration: time.Second * 300}
			nodeClaims[1].Spec.TerminationGracePeriod = &metav1.Duration{Duration: time.Second * 300}

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0])

			// we should delete the non-annotated node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		})
		It("does not consolidate nodes with pods with blocking PDBs when the NodePool's TerminationGracePeriod is not nil", func() {
			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			budget := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         labels,
				MaxUnavailable: fromInt(0),
			})

			nodeClaims[0].Spec.TerminationGracePeriod = &metav1.Duration{Duration: time.Second * 300}
			nodeClaims[1].Spec.TerminationGracePeriod = &metav1.Duration{Duration: time.Second * 300}

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool, budget)
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0])

			// we should delete the non-annotated node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		})
		It("does not consolidate nodes with pods that have a duration-based do-not-disrupt annotation that is still active", func() {
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			// Set a 2m duration-based do-not-disrupt annotation on pod[2]
			pods[2].Annotations = lo.Assign(pods[2].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "2m"})
			pods[2].Status.StartTime = &metav1.Time{Time: env.Clock.Now()}

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])

			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Grace period is still active on pod[2], so nodes[1] should not be consolidated
			// Only nodes[0] (no annotated pods) can be consolidated
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0])

			// nodes[0] deleted, nodes[1] still protected
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0])
		})
		It("can consolidate nodes after duration-based do-not-disrupt annotation expires", func() {
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			// All pods have a 2m duration-based do-not-disrupt annotation
			for _, p := range pods {
				p.Annotations = lo.Assign(p.Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "2m"})
				p.Status.StartTime = &metav1.Time{Time: env.Clock.Now()}
			}

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])

			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// All pods have active grace periods, no consolidation should happen
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))

			// Advance clock past the 2m grace period
			env.Clock.Step(3 * time.Minute)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Grace periods expired, consolidation should now proceed
			cmds = queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
		})
		It("can delete nodes, considers invalid do-not-disrupt annotation format as not blocking", func() {
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			// Set an invalid format annotation - should not block consolidation
			pods[2].Annotations = lo.Assign(pods[2].Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "invalid-format"})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])

			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Invalid annotation format should not block consolidation
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])
		})
		It("can delete nodes, evicts pods without an ownerRef", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			// pod[2] is a stand-alone (non ReplicaSet) pod
			pods[2].OwnerReferences = nil
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)

			// two pods on node 1
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			// one on node 2, but it's a standalone pod
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			// we don't need a new node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// but we expect to delete the nodeclaim with the fewest pods (nodeclaim 2) even though the pod has no ownerRefs
			// and will not be recreated
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])
		})
		It("won't delete node if it would require pods to schedule on an uninitialized node", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims, intentionally leaving node as not ready
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[0]))
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaims[0]))
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[1]}, []*v1.NodeClaim{nodeClaims[1]})

			ExpectSingletonReconciled(ctx, disruptionController)
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))

			// shouldn't delete the node
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))

			// Expect Unconsolidatable events to be fired
			evts := recorder.Events()
			_, ok := lo.Find(evts, func(e events.Event) bool {
				return strings.Contains(e.Message, "Not all pods would schedule")
			})
			Expect(ok).To(BeTrue())
			_, ok = lo.Find(evts, func(e events.Event) bool {
				return strings.Contains(e.Message, "would schedule against uninitialized nodeclaim")
			})
			Expect(ok).To(BeTrue())
		})
		It("should not consolidate pods onto a destination node that is under its consolidateAfter window", func() {
			// Override consolidateAfter to a nonzero duration.
			// Both nodes are leastExpensiveInstance (from BeforeEach), so no cheaper replacement
			// exists — the only consolidation path is to delete nodes[1] and move its pod
			// to nodes[0]. With consolidateAfter still active on nodes[0], this should be blocked.
			nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("1m")

			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)

			// bind pods to node — 2 on nodes[0], 1 on nodes[1]
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// Initialize both nodes — this sets the Initialized condition timestamp to ~env.Clock.Now()
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})

			// Do NOT advance clock — both nodes are within the 1m consolidateAfter window.
			// The scheduler should refuse to place non-pending pods onto nodes[0] because it's under consolidateAfter,
			// blocking the delete of nodes[1] (which would need to reschedule pods[2] onto nodes[0]).
			ExpectSingletonReconciled(ctx, disruptionController)

			// No consolidation should happen — destination node is under consolidateAfter
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))

			// Advance clock past the 1m consolidateAfter window
			env.Clock.Step(2 * time.Minute)

			// Re-sync cluster state and re-mark nodeClaims as consolidatable after state update
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			for _, nc := range nodeClaims {
				nc = ExpectExists(ctx, env.Client, nc)
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				ExpectApplied(ctx, env.Client, nc)
				ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nc))
			}
			// Reset the disruption controller so consolidation methods are not cached as consolidated
			*queue = lo.FromPtr(disruption.NewQueue(env.Client, recorder, cluster, env.Clock, prov, nodePoolBackoff))
			disruptionController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
				disruption.WithMethods(NewMethodsWithNopValidator()...))
			ExpectSingletonReconciled(ctx, disruptionController)

			// Now consolidation should proceed — consolidateAfter window has expired
			cmds = queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])
		})
		It("should consolidate pods onto a destination node with consolidateAfter set to Never", func() {
			// consolidateAfter: Never means the node never becomes a consolidation *candidate* (source),
			// but it should still be a valid *destination* for pods during consolidation.
			// Set up two NodePools: source allows consolidation (0s), destination has Never.
			destinationNodePool := test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
						Budgets:             []v1.Budget{{Nodes: "100%"}},
						ConsolidateAfter:    v1.MustParseNillableDuration("Never"),
					},
				},
			})

			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			// destination node belongs to the "Never" NodePool
			destinationNodeClaim, destinationNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            destinationNodePool.Name,
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

			ExpectApplied(ctx, env.Client, rs, pod, nodeClaims[0], nodes[0], destinationNodeClaim, destinationNode, nodePool, destinationNodePool)
			ExpectManualBinding(ctx, env.Client, pod, nodes[0])

			// Initialize both nodes
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{nodes[0], destinationNode}, []*v1.NodeClaim{nodeClaims[0], destinationNodeClaim})

			// Do NOT advance clock — destination node should still be valid because
			// consolidateAfter: Never means the node is never "under consolidateAfter"
			ExpectSingletonReconciled(ctx, disruptionController)

			// Consolidation should succeed — destination node accepts pods regardless of time
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0])

			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0])
		})
		It("should replace a node with a cheaper one even when consolidateAfter has not expired", func() {
			// The replace path launches a new NodeClaim rather than scheduling onto an existing node.
			// consolidateAfter on the destination should not block replacements since there is no
			// existing destination node to gate — we're creating a brand new one.
			nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("1m")

			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})
			// Use the most expensive node as the source — consolidation should replace it with a cheaper one
			ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool)

			// bind pod to the expensive node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// Initialize node and advance clock past consolidateAfter so the SOURCE is eligible
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			env.Clock.Step(2 * time.Minute)

			// Re-sync state after clock advance
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			ExpectSingletonReconciled(ctx, disruptionController)

			// Should create a replacement — the replace path is not blocked by consolidateAfter
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// Should have replaced with a cheaper node
			remainingNodeClaims := ExpectNodeClaims(ctx, env.Client)
			remainingNodes := ExpectNodes(ctx, env.Client)
			Expect(remainingNodeClaims).To(HaveLen(1))
			Expect(remainingNodes).To(HaveLen(1))
			Expect(remainingNodeClaims[0].Name).ToNot(Equal(nodeClaim.Name))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("should consider initialized nodes before uninitialized nodes", func() {
			defaultInstanceType := fake.NewInstanceType("default-instance-type",
				fake.WithResources(corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("3"),
					corev1.ResourceMemory: resource.MustParse("3Gi"),
					corev1.ResourcePods:   resource.MustParse("110"),
				}),
			)
			smallInstanceType := fake.NewInstanceType("small-instance-type",
				fake.WithResources(corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
					corev1.ResourcePods:   resource.MustParse("10"),
				}),
			)
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				defaultInstanceType,
				smallInstanceType,
			}
			ExpectSingletonReconciled(ctx, pricingController)
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			podCount := 100
			pods := test.Pods(podCount, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, rs, nodePool)

			// Setup 100 nodeclaims/nodes with a single nodeclaim/node that is initialized
			elem := rand.Intn(100) //nolint:gosec
			for i := range podCount {
				m, n := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							corev1.LabelInstanceTypeStable: defaultInstanceType.Name,
							v1.CapacityTypeLabelKey:        defaultInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
							corev1.LabelTopologyZone:       defaultInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
						},
					},
					Status: v1.NodeClaimStatus{
						Allocatable: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:    resource.MustParse("3"),
							corev1.ResourceMemory: resource.MustParse("3Gi"),
							corev1.ResourcePods:   resource.MustParse("100"),
						},
					},
				})
				m.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				ExpectApplied(ctx, env.Client, pods[i], m, n)
				ExpectManualBinding(ctx, env.Client, pods[i], n)

				if i == elem {
					ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{n}, []*v1.NodeClaim{m})
				} else {
					ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(m))
					ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
				}
			}

			// Create a pod and nodeclaim/node that will eventually be scheduled onto the initialized node
			consolidatableNodeClaim, consolidatableNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: smallInstanceType.Name,
						v1.CapacityTypeLabelKey:        smallInstanceType.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       smallInstanceType.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
						corev1.ResourcePods:   resource.MustParse("100"),
					},
				},
			})
			consolidatableNodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)

			// create a new RS so we can link a pod to it
			rs = test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			consolidatablePod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, consolidatableNodeClaim, consolidatableNode, consolidatablePod)
			ExpectManualBinding(ctx, env.Client, consolidatablePod, consolidatableNode)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{consolidatableNode}, []*v1.NodeClaim{consolidatableNodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, consolidatableNodeClaim)
			// Expect no events that state that the pods would schedule against a uninitialized node
			evts := recorder.Events()
			_, ok := lo.Find(evts, func(e events.Event) bool {
				return strings.Contains(e.Message, "would schedule against uninitialized nodeclaim")
			})
			Expect(ok).To(BeFalse())

			// the nodeclaim with the small instance should consolidate onto the initialized node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(100))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(100))
			ExpectNotFound(ctx, env.Client, consolidatableNodeClaim, consolidatableNode)
		})
		It("can delete nodes with a permanently pending pod", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			pending := test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{
					"non-existent": "node-label",
				},
			})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool, pending)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])

			// pending pod is still here and hasn't been scheduled anywayre
			pending = ExpectPodExists(ctx, env.Client, pending.Name, pending.Namespace)
			Expect(pending.Spec.NodeName).To(BeEmpty())
		})
		It("can delete nodes with a pending pod that needs a new node", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			// A pending pod that fits on no existing node, only on a new node. Whether or not that node ever launches
			// (e.g. the cloud provider is out of capacity), the pod needs new capacity regardless of any disruption
			// decision, so it must not block consolidation of unrelated nodes.
			pending := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("48")},
				},
			})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool, pending)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Replacements).To(BeEmpty())
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			// we don't need a new node to replace the candidate, and the pending pod's new capacity is provisioned
			// independently of consolidation
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])

			// pending pod is still here and hasn't been scheduled
			pending = ExpectPodExists(ctx, env.Client, pending.Name, pending.Namespace)
			Expect(pending.Spec.NodeName).To(BeEmpty())
		})
		Context("Pending Pod Baseline", func() {
			// Nodes and pods are pinned to a "pool" label so each spec controls exactly which existing node a pod can use.
			// The NodePool allows both pools, so a pod that fits no existing node can still land on a new NodeClaim.
			const poolLabel = "pool"
			var rs *appsv1.ReplicaSet
			var nodeClaimsToInit []*v1.NodeClaim
			var nodesToInit []*corev1.Node

			// poolNode makes a 32 cpu node in pool; setup applies and initializes it.
			poolNode := func(pool string) *corev1.Node {
				nc, n := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
							v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
							corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
							poolLabel:                      pool,
						},
					},
					Status: v1.NodeClaimStatus{
						// Capacity counts against NodePool limits; the linked Node copies both from the NodeClaim.
						Capacity: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:  resource.MustParse("32"),
							corev1.ResourcePods: resource.MustParse("100"),
						},
						Allocatable: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:  resource.MustParse("32"),
							corev1.ResourcePods: resource.MustParse("100"),
						},
					},
				})
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				nodeClaimsToInit, nodesToInit = append(nodeClaimsToInit, nc), append(nodesToInit, n)
				return n
			}
			// poolPodOptions describes a reschedulable pod requesting cpu; an empty pool leaves it unpinned.
			poolPodOptions := func(pool, cpu string) test.PodOptions {
				opts := test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: labels,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "ReplicaSet",
								Name:               rs.Name,
								UID:                rs.UID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
					},
				}
				if pool != "" {
					opts.NodeSelector = map[string]string{poolLabel: pool}
				}
				return opts
			}
			poolPod := func(pool, cpu string) *corev1.Pod { return test.Pod(poolPodOptions(pool, cpu)) }
			pendingPoolPod := func(pool, cpu string) *corev1.Pod { return test.UnschedulablePod(poolPodOptions(pool, cpu)) }
			// setup applies every poolNode and the given pods, binds each bound pod to its node, and initializes cluster state.
			setup := func(bound map[*corev1.Pod]*corev1.Node, pending ...*corev1.Pod) {
				GinkgoHelper()
				objs := []client.Object{rs, nodePool}
				for i := range nodesToInit {
					objs = append(objs, nodeClaimsToInit[i], nodesToInit[i])
				}
				for p := range bound {
					objs = append(objs, p)
				}
				for _, p := range pending {
					objs = append(objs, p)
				}
				ExpectApplied(ctx, env.Client, objs...)
				for p, n := range bound {
					ExpectManualBinding(ctx, env.Client, p, n)
				}
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodesToInit, nodeClaimsToInit)
			}
			// simulateWithout runs the consolidation simulation for the candidate on node, the way computeConsolidation and
			// validateCommand do.
			simulateWithout := func(node *corev1.Node) pscheduling.Results {
				GinkgoHelper()
				snc := disruption.NewSingleNodeConsolidation(disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue))
				candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, snc.ShouldDisrupt, snc.Class(), queue)
				Expect(err).To(Succeed())
				candidate, ok := lo.Find(candidates, func(c *disruption.Candidate) bool { return c.Name() == node.Name })
				Expect(ok).To(BeTrue())
				results, err := disruption.SimulateScheduling(ctx, env.Client, cluster, prov, env.Clock, recorder, []pscheduling.Options{pscheduling.IsConsolidationSimulation}, disruption.ConsolidationSimulationOptions(), candidate)
				Expect(err).To(Succeed())
				return results
			}
			// podsOf returns the names of the pods each new NodeClaim holds.
			podsOf := func(results pscheduling.Results) [][]string {
				return lo.Map(results.NewNodeClaims, func(nc *pscheduling.NodeClaim, _ int) []string {
					return lo.Map(nc.Pods, func(p *corev1.Pod, _ int) string { return p.Name })
				})
			}
			// addReservation adds a reservation with a single free slot, priced far below on-demand like every reservation.
			addReservation := func() {
				reservationID := fmt.Sprintf("r-%s", mostExpensiveInstance.Name)
				mostExpensiveInstance.Requirements.Add(scheduling.NewRequirement(cloudprovider.ReservationIDLabel, corev1.NodeSelectorOpIn, reservationID))
				mostExpensiveInstance.Requirements.Get(v1.CapacityTypeLabelKey).Insert(v1.CapacityTypeReserved)
				mostExpensiveInstance.Offerings = append(mostExpensiveInstance.Offerings, &cloudprovider.Offering{
					Price:               mostExpensiveOffering.Price / 1_000_000.0,
					Available:           true,
					Compatible:          true,
					ReservationCapacity: 1,
					Requirements: scheduling.NewLabelRequirements(map[string]string{
						v1.CapacityTypeLabelKey:     v1.CapacityTypeReserved,
						corev1.LabelTopologyZone:    mostExpensiveOffering.Zone(),
						v1alpha1.LabelReservationID: reservationID,
					}),
				})
				ExpectSingletonReconciled(ctx, pricingController)
				ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{ReservedCapacity: new(true)}}))
			}
			withCPULimit := func(cpu string) func() {
				return func() { nodePool.Spec.Limits = v1.Limits{corev1.ResourceCPU: resource.MustParse(cpu)} }
			}

			BeforeEach(func() {
				// Apply the ReplicaSet up front so it has a UID for the pods' owner references.
				rs = test.ReplicaSet()
				ExpectApplied(ctx, env.Client, rs)
				nodeClaimsToInit, nodesToInit = nil, nil
				nodePool.Spec.Template.Spec.Requirements = append(nodePool.Spec.Template.Spec.Requirements, v1.NodeSelectorRequirementWithMinValues{
					Key:      poolLabel,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"x", "c"},
				})
			})

			// Each case has node x, filled by a filler pod, and the candidate c holding one pod. The pending pod is pinned
			// to x. Expectations name pods by role: "candidate" for c's pod, "pending" for the pending pod.
			type baselineCase struct {
				fillerCPU, candidatePool, candidateCPU, pendingCPU string
				prep                                               func()
				// newNodeClaims are the pods on each NodeClaim the disruption is charged for.
				newNodeClaims [][]string
				// unschedulable are the pods the simulation can't place.
				unschedulable []string
				// replacementReserved, when set, is whether the first charged NodeClaim may use the reservation.
				replacementReserved *bool
			}
			DescribeTable("should charge the disruption only for the capacity it causes",
				func(c baselineCase) {
					if c.prep != nil {
						c.prep()
					}
					nodeX, nodeC := poolNode("x"), poolNode("c")
					candidatePod := poolPod(c.candidatePool, c.candidateCPU)
					pending := pendingPoolPod("x", c.pendingCPU)
					setup(map[*corev1.Pod]*corev1.Node{poolPod("x", c.fillerCPU): nodeX, candidatePod: nodeC}, pending)
					names := map[string]string{"candidate": candidatePod.Name, "pending": pending.Name}

					results := simulateWithout(nodeC)
					Expect(podsOf(results)).To(Equal(lo.Map(c.newNodeClaims, func(roles []string, _ int) []string {
						return lo.Map(roles, func(r string, _ int) string { return names[r] })
					})))
					Expect(lo.Map(lo.Keys(results.PodErrors), func(p *corev1.Pod, _ int) string { return p.Name })).To(
						ConsistOf(lo.Map(c.unschedulable, func(r string, _ int) string { return names[r] })))
					if c.replacementReserved != nil {
						Expect(results.NewNodeClaims[0].Requirements.Has(cloudprovider.ReservationIDLabel)).To(Equal(*c.replacementReserved))
					}
				},
				// x has 2 cpu free and x and c use the whole 64 cpu limit, so the pending pod can't schedule at all. Deleting c
				// releases c's share of the limit, letting the pending pod fit a new node; that node isn't a cost of deleting c.
				Entry("ignores a pending pod a NodePool limit keeps from scheduling", baselineCase{
					fillerCPU: "30", candidateCPU: "1", pendingCPU: "10", prep: withCPULimit("64"),
				}),
				// x has 4 cpu free, so the pending pod fits there now. Without c, c's larger pod is placed on x first and pushes
				// the pending pod onto a new node, a node only the disruption makes necessary.
				Entry("counts a new node the disruption pushes a pending pod onto", baselineCase{
					fillerCPU: "28", candidateCPU: "3500m", pendingCPU: "3",
					newNodeClaims: [][]string{{"pending"}},
				}),
				// x has 1 cpu free, so both pods need a new node without c; c's pod must not be packed onto the pending pod's.
				Entry("sizes a replacement for the candidate's pods only", baselineCase{
					fillerCPU: "31", candidateCPU: "2", pendingCPU: "3",
					newNodeClaims: [][]string{{"candidate"}},
				}),
				// The pending pod needs a new node and takes the only reserved slot, so c's replacement can't be priced at the
				// reservation's rate.
				Entry("keeps a reserved slot for the pending pod that takes it", baselineCase{
					fillerCPU: "31", candidatePool: "c", candidateCPU: "1", pendingCPU: "2", prep: addReservation,
					newNodeClaims: [][]string{{"candidate"}}, replacementReserved: new(false),
				}),
				Entry("ignores a pending pod bound for a reserved slot when the candidate's pods fit elsewhere", baselineCase{
					fillerCPU: "30", candidateCPU: "1", pendingCPU: "3", prep: addReservation,
				}),
				// Without c, 64 cpu of the 96 cpu limit is free. The pending pod, placed first, claims it for its new node, so
				// c's pod, pinned to pool c, has no limit left for a replacement. Dropping the pending pod would spend the
				// headroom it needs on the replacement.
				Entry("keeps NodePool limit headroom for the pending pod that needs it", baselineCase{
					fillerCPU: "30", candidatePool: "c", candidateCPU: "1", pendingCPU: "10", prep: withCPULimit("96"),
					unschedulable: []string{"candidate"},
				}),
			)
			It("should keep a candidate's pod out of a topology domain a pending pod's new node takes", func() {
				// x has 4 cpu free, enough for c's pod, but the 10 cpu pending pod pinned to x needs a new node there either way.
				// c's pod has pool anti-affinity to it, so with the pending pod placed first (it is larger) c's pod can't take x
				// and needs a new node in pool c. Dropping the pending pod would let c's pod take x and leave the pending pod
				// unable to schedule there once it can launch.
				nodeX, nodeC := poolNode("x"), poolNode("c")
				pending := pendingPoolPod("x", "10")
				pending.Labels = map[string]string{"app": "pending"}
				candidateOpts := poolPodOptions("", "1")
				candidateOpts.PodAntiRequirements = []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: pending.Labels},
					TopologyKey:   poolLabel,
				}}
				candidatePod := test.Pod(candidateOpts)
				setup(map[*corev1.Pod]*corev1.Node{poolPod("x", "28"): nodeX, candidatePod: nodeC}, pending)

				Expect(podsOf(simulateWithout(nodeC))).To(Equal([][]string{{candidatePod.Name}}))
			})
			// x has 12 cpu free, wanted by the pending pod and the pod on deleting node d. Whichever is placed second needs a
			// new node no matter what happens to c, so neither may block deleting c.
			DescribeTable("can delete nodes when a deleting node's pod and a pending pod compete for the same space",
				func(migratingCPU, pendingCPU string) {
					nodeX, nodeD, nodeC := poolNode("x"), poolNode("x"), poolNode("c")
					setup(map[*corev1.Pod]*corev1.Node{
						poolPod("x", "20"):         nodeX,
						poolPod("x", migratingCPU): nodeD,
						poolPod("", "500m"):        nodeC,
					}, pendingPoolPod("x", pendingCPU))
					cluster.MarkForDeletion(nodeD.Spec.ProviderID)

					results := simulateWithout(nodeC)
					Expect(results.AllNonPendingPodsScheduled()).To(BeTrue())
					Expect(results.NewNodeClaims).To(BeEmpty())
				},
				Entry("when the pending pod is the one left needing a new node", "11", "10"),
				Entry("when the deleting node's pod is the one left needing a new node", "10", "11"),
			)
			It("can delete multiple nodes with a pending pod that needs a new node", func() {
				// x has 4 cpu free: enough for the 1 cpu pods on c1 and c2, not for the 10 cpu pending pod pinned to x, which
				// needs a new node whatever happens to c1 and c2. x's filler can't be disrupted, so x is not a candidate.
				nodeX, nodeC1, nodeC2 := poolNode("x"), poolNode("c"), poolNode("c")
				filler := poolPod("x", "28")
				filler.Annotations = map[string]string{v1.DoNotDisruptAnnotationKey: "true"}
				setup(map[*corev1.Pod]*corev1.Node{filler: nodeX, poolPod("", "1"): nodeC1, poolPod("", "1"): nodeC2}, pendingPoolPod("x", "10"))

				mnc := disruption.NewMultiNodeConsolidation(disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue), disruption.WithValidator(NopValidator{}))
				budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, mnc.Reason())
				Expect(err).To(Succeed())
				candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, mnc.ShouldDisrupt, mnc.Class(), queue)
				Expect(err).To(Succeed())
				Expect(candidates).To(HaveLen(2))

				cmds, err := mnc.ComputeCommands(ctx, budgets, candidates...)
				Expect(err).To(Succeed())
				Expect(cmds).To(HaveLen(1))
				// both candidates go without a replacement; the pending pod's node is left to provisioning
				Expect(cmds[0].Decision()).To(Equal(disruption.DeleteDecision))
				Expect(cmds[0].Replacements).To(BeEmpty())
				Expect(lo.Map(cmds[0].Candidates, func(c *disruption.Candidate, _ int) string { return c.Name() })).To(ConsistOf(nodeC1.Name, nodeC2.Name))
			})
			Context("Caching", func() {
				BeforeEach(func() {
					// Every node is full enough that none can be consolidated, so a pass simulates every candidate. The pending
					// pod fits no existing node, so the baseline has to run.
					bound := map[*corev1.Pod]*corev1.Node{}
					for range 3 {
						bound[poolPod("", "30")] = poolNode("c")
					}
					setup(bound, pendingPoolPod("", "48"))
				})
				// expectSchedulerBuilds runs a single-node consolidation pass, publishing through rec, and expects it to build
				// want schedulers. Every scheduler build records one topology build sample.
				expectSchedulerBuilds := func(rec events.Recorder, want int) {
					GinkgoHelper()
					snc := disruption.NewSingleNodeConsolidation(disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, rec, queue))
					budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, env.Clock, env.Client, cloudProvider, recorder, snc.Reason())
					Expect(err).To(Succeed())
					candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, snc.ShouldDisrupt, snc.Class(), queue)
					Expect(err).To(Succeed())
					Expect(candidates).To(HaveLen(3))
					pscheduling.TopologyBuildDurationSeconds.Reset()
					cmds, err := snc.ComputeCommands(ctx, budgets, candidates...)
					Expect(err).To(Succeed())
					Expect(cmds).To(BeEmpty())
					ExpectMetricHistogramSampleCountValue("karpenter_scheduler_topology_build_duration_seconds", uint64(want), map[string]string{})
				}
				It("should solve the pending pod baseline once per single-node consolidation pass", func() {
					// One simulation per candidate, plus a single baseline shared by all of them.
					expectSchedulerBuilds(recorder, 3+1)
				})
				It("should re-solve the pending pod baseline when the cluster's consolidation state changes during the pass", func() {
					// Once the first candidate is found unconsolidatable, after the pass solved its baseline and before the next
					// candidate is simulated, move the cluster's consolidation state, as any binding, deletion, or node change
					// would. The clock steps first because the consolidation state is the time of the last change.
					changed := false
					hooked := &hookedRecorder{Recorder: recorder, onPublish: func(evt events.Event) {
						if changed || evt.Reason != events.Unconsolidatable {
							return
						}
						changed = true
						env.Clock.Step(time.Second)
						cluster.MarkUnconsolidated()
					}}
					// One simulation per candidate, plus the baseline solved before the change and the one re-solved after it.
					expectSchedulerBuilds(hooked, 3+2)
					Expect(changed).To(BeTrue())
				})
			})
		})
		It("won't delete nodes if it would make a non-pending pod go pending", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			// setup labels and node selectors so we force the pods onto the nodes we want
			nodes[0].Labels["foo"] = "1"
			nodes[1].Labels["foo"] = "2"

			pods[0].Spec.NodeSelector = map[string]string{"foo": "1"}
			pods[1].Spec.NodeSelector = map[string]string{"foo": "1"}
			pods[2].Spec.NodeSelector = map[string]string{"foo": "2"}

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// No node can be deleted as it would cause one of the three pods to go pending
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		})
		It("can delete nodes while an invalid node pool exists", func() {
			// this invalid node pool should not be enough to stop all disruption
			badNodePool := &v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bad-nodepool",
				},
				Spec: v1.NodePoolSpec{
					Template: v1.NodeClaimTemplate{
						Spec: v1.NodeClaimTemplateSpec{
							Requirements: []v1.NodeSelectorRequirementWithMinValues{},
							NodeClassRef: &v1.NodeClassReference{
								Group: "karpenter.test.sh",
								Kind:  "TestNodeClass",
								Name:  "non-existent",
							},
						},
					},
				},
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, badNodePool, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)
			cloudProvider.ErrorsForNodePool[badNodePool.Name] = fmt.Errorf("unable to fetch instance types")

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])
		})
	})
	Context("TTL", func() {
		var nodeClaims []*v1.NodeClaim
		var nodes []*corev1.Node

		BeforeEach(func() {
			disruptionController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(NewMethodsWithRealValidator()...))
			nodeClaims, nodes = test.NodeClaimsAndNodes(2, v1.NodeClaim{
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
		})
		It("should wait for the node TTL for non-empty nodes before consolidating", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			// assign the nodeclaims to the least expensive offering so only one of them gets deleted
			nodeClaims[0].Labels = lo.Assign(nodeClaims[0].Labels, map[string]string{
				corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
				v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})
			nodes[0].Labels = lo.Assign(nodes[0].Labels, map[string]string{
				corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
				v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})
			nodeClaims[1].Labels = lo.Assign(nodeClaims[1].Labels, map[string]string{
				corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
				v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})
			nodes[1].Labels = lo.Assign(nodes[1].Labels, map[string]string{
				corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
				v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})

			finished := atomic.Bool{}
			ExpectParallelized(
				func() {
					defer finished.Store(true)
					ExpectSingletonReconciled(ctx, disruptionController)
				},
				func() {
					// wait for the controller to block on the validation timeout
					Eventually(env.Clock.HasWaiters, time.Second*10).Should(BeTrue())
					// controller should be blocking during the timeout
					Expect(finished.Load()).To(BeFalse())
					// and the node should not be deleted yet
					ExpectExists(ctx, env.Client, nodeClaims[0])
					ExpectExists(ctx, env.Client, nodeClaims[1])

					// advance the clock so that the timeout expires
					env.Clock.Step(31 * time.Second)

					// controller should finish
					Eventually(finished.Load, 10*time.Second).Should(BeTrue())
				},
			)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			// nodeclaim should be deleted after the TTL due to emptiness
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])
		})
		It("should not consolidate if the action picks different instance types after the node TTL wait", func() {
			// create our RS so we can link a pod to it
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
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodePool, pod)
			ExpectManualBinding(ctx, env.Client, pod, nodes[0])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0]}, []*v1.NodeClaim{nodeClaims[0]})

			var wg sync.WaitGroup
			wg.Add(1)
			finished := atomic.Bool{}
			ExpectParallelized(
				func() {
					defer finished.Store(true)
					ExpectSingletonReconciled(ctx, disruptionController)
				},
				func() {
					// wait for the disruptionController to block on the validation timeout
					Eventually(env.Clock.HasWaiters, time.Second*10).Should(BeTrue())
					// controller should be blocking during the timeout
					Expect(finished.Load()).To(BeFalse())

					// and the node should not be deleted yet
					ExpectExists(ctx, env.Client, nodes[0])

					// add an additional pod to the node to change the consolidation decision
					pod2 := test.Pod(test.PodOptions{
						ObjectMeta: metav1.ObjectMeta{Labels: labels,
							OwnerReferences: []metav1.OwnerReference{
								{
									APIVersion:         "apps/v1",
									Kind:               "ReplicaSet",
									Name:               rs.Name,
									UID:                rs.UID,
									Controller:         new(true),
									BlockOwnerDeletion: new(true),
								},
							},
						},
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("1"),
							},
						},
					})
					ExpectApplied(ctx, env.Client, pod2)
					ExpectManualBinding(ctx, env.Client, pod2, nodes[0])
					ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[0]))

					// advance the clock so that the timeout expires
					env.Clock.Step(31 * time.Second)
					// controller should finish
					Eventually(finished.Load, 10*time.Second).Should(BeTrue())
				},
			)

			// nothing should be removed since the node is no longer empty
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodes[0])
		})
		It("should not consolidate if the action becomes invalid during the node TTL wait", func() {
			pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.DoNotDisruptAnnotationKey: "true",
				},
			}})
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodePool, pod)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0]}, []*v1.NodeClaim{nodeClaims[0]})

			finished := atomic.Bool{}
			ExpectParallelized(
				func() {
					defer finished.Store(true)
					ExpectSingletonReconciled(ctx, disruptionController)
				},
				func() {
					// wait for the disruptionController to block on the validation timeout
					Eventually(env.Clock.HasWaiters, time.Second*10).Should(BeTrue())
					// controller should be blocking during the timeout
					Expect(finished.Load()).To(BeFalse())
					// and the node should not be deleted yet
					ExpectExists(ctx, env.Client, nodeClaims[0])

					// make the node non-empty by binding it
					ExpectManualBinding(ctx, env.Client, pod, nodes[0])
					ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[0]))

					// advance the clock so that the timeout expires
					env.Clock.Step(31 * time.Second)
					// controller should finish
					Eventually(finished.Load, 10*time.Second).Should(BeTrue())
				},
			)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))

			// nothing should be removed since the node is no longer empty
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaims[0])
		})
		It("should not replace node if a pod schedules with karpenter.sh/do-not-disrupt set to true during the TTL wait", func() {
			pod := test.Pod()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			ExpectParallelized(
				func() {
					ExpectSingletonReconciled(ctx, disruptionController)
				},
				func() {
					Eventually(env.Clock.HasWaiters, time.Second*10).Should(BeTrue())
					doNotDisruptPod := test.Pod(test.PodOptions{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								v1.DoNotDisruptAnnotationKey: "true",
							},
						},
					})
					ExpectApplied(ctx, env.Client, doNotDisruptPod)
					ExpectManualBinding(ctx, env.Client, doNotDisruptPod, node)

					// we would normally be able to replace a node, but we are blocked by the do-not-disrupt pods during validation
					Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
					Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
					ExpectExists(ctx, env.Client, node)
					// advance the clock so that the timeout expires
					env.Clock.Step(31 * time.Second)
				},
			)
		})
		It("should not replace node if a pod schedules with a blocking PDB during the TTL wait", func() {
			pod := test.Pod()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			ExpectParallelized(
				func() {
					ExpectSingletonReconciled(ctx, disruptionController)
				},
				func() {
					Eventually(env.Clock.HasWaiters, time.Second*10).Should(BeTrue())
					blockingPDBPod := test.Pod(test.PodOptions{
						ObjectMeta: metav1.ObjectMeta{
							Labels: labels,
						},
					})
					pdb := test.PodDisruptionBudget(test.PDBOptions{
						Labels:         labels,
						MaxUnavailable: fromInt(0),
					})
					ExpectApplied(ctx, env.Client, blockingPDBPod, pdb)
					ExpectManualBinding(ctx, env.Client, blockingPDBPod, node)

					// we would normally be able to replace a node, but we are blocked by the PDB during validation
					Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
					Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
					ExpectExists(ctx, env.Client, node)
					// advance the clock so that the timeout expires
					env.Clock.Step(31 * time.Second)
				},
			)
		})
		It("should not delete node if pods schedule with karpenter.sh/do-not-disrupt set to true during the TTL wait", func() {
			pods := test.Pods(2, test.PodOptions{})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], pods[0], pods[1])

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})

			ExpectParallelized(
				func() {
					ExpectSingletonReconciled(ctx, disruptionController)
				},
				func() {
					Eventually(env.Clock.HasWaiters, time.Second*10).Should(BeTrue())
					doNotDisruptPods := test.Pods(2, test.PodOptions{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								v1.DoNotDisruptAnnotationKey: "true",
							},
						},
					})
					ExpectApplied(ctx, env.Client, doNotDisruptPods[0], doNotDisruptPods[1])
					ExpectManualBinding(ctx, env.Client, doNotDisruptPods[0], nodes[0])
					ExpectManualBinding(ctx, env.Client, doNotDisruptPods[1], nodes[1])

					// we would normally be able to consolidate down to a single node, but we are blocked by the do-not-disrupt pods during validation
					Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
					Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
					ExpectExists(ctx, env.Client, nodes[0])
					ExpectExists(ctx, env.Client, nodes[1])
					// advance the clock so that the timeout expires
					env.Clock.Step(31 * time.Second)
				},
			)
		})
		It("should not delete node if pods schedule with a blocking PDB during the TTL wait", func() {
			pods := test.Pods(2, test.PodOptions{})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], pods[0], pods[1])

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})

			ExpectParallelized(
				func() {
					ExpectSingletonReconciled(ctx, disruptionController)
				},
				func() {
					Eventually(env.Clock.HasWaiters, time.Second*10).Should(BeTrue())
					blockingPDBPods := test.Pods(2, test.PodOptions{
						ObjectMeta: metav1.ObjectMeta{
							Labels: labels,
						},
					})
					pdb := test.PodDisruptionBudget(test.PDBOptions{
						Labels:         labels,
						MaxUnavailable: fromInt(0),
					})
					ExpectApplied(ctx, env.Client, blockingPDBPods[0], blockingPDBPods[1], pdb)
					ExpectManualBinding(ctx, env.Client, blockingPDBPods[0], nodes[0])
					ExpectManualBinding(ctx, env.Client, blockingPDBPods[1], nodes[1])

					// we would normally be able to consolidate down to a single node, but we are blocked by the PDB during validation
					Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
					Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
					ExpectExists(ctx, env.Client, nodes[0])
					ExpectExists(ctx, env.Client, nodes[1])
					// advance the clock so that the timeout expires
					env.Clock.Step(31 * time.Second)
				},
			)
		})
	})
	Context("Multi-NodeClaim", func() {
		var nodeClaims, spotNodeClaims []*v1.NodeClaim
		var nodes, spotNodes []*corev1.Node

		BeforeEach(func() {
			nodeClaims = []*v1.NodeClaim{}
			spotNodeClaims = []*v1.NodeClaim{}
			nodes = []*corev1.Node{}
			spotNodes = []*corev1.Node{}
			nodeClaims, nodes = test.NodeClaimsAndNodes(3, v1.NodeClaim{
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
			spotNodeClaims, spotNodes = test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveSpotInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveSpotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveSpotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			for i := range nodeClaims {
				nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
				spotNodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}
		})
		DescribeTable("can merge 3 nodes into 1", func(spotToSpot bool) {
			nodeClaims = lo.Ternary(spotToSpot, spotNodeClaims, nodeClaims)
			nodes = lo.Ternary(spotToSpot, spotNodes, nodes)
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2], nodePool)
			ExpectMakeNodesInitialized(ctx, env.Client, env.Clock, nodes[0], nodes[1], nodes[2])

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[2])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1], nodes[2]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0], nodeClaims[1], nodeClaims[2])

			// three nodeclaims should be replaced with a single nodeclaim
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2])
		},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		DescribeTable("can merge 3 nodes into 1 without available Spot offerings", func(requirements []v1.NodeSelectorRequirementWithMinValues, includeUnavailableSpot bool) {
			if requirements != nil {
				nodePool.Spec.Template.Spec.Requirements = requirements
			}
			offerings := func(price float64) []cloudprovider.Offering {
				result := []cloudprovider.Offering{{
					Available:    true,
					Compatible:   true,
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1"}),
					Price:        price,
				}}
				if includeUnavailableSpot {
					result = append(result, cloudprovider.Offering{
						Available:    false,
						Compatible:   true,
						Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeSpot, corev1.LabelTopologyZone: "test-zone-1"}),
						Price:        price / 2,
					})
				}
				return result
			}
			currentInstance := fake.NewInstanceType("current-on-demand-only",
				fake.WithOfferings(offerings(1.5)...),
			)
			replacementInstance := fake.NewInstanceType("replacement-on-demand-only",
				fake.WithOfferings(offerings(1.0)...),
			)
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{currentInstance, replacementInstance}
			ExpectSingletonReconciled(ctx, pricingController)

			nodeClaims, nodes = test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: currentInstance.Name,
						v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
						corev1.LabelTopologyZone:       "test-zone-1",
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			for i := range nodeClaims {
				nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}

			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2], nodePool)
			ExpectMakeNodesInitialized(ctx, env.Client, env.Clock, nodes[0], nodes[1], nodes[2])

			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[2])

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1], nodes[2]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2]})
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Replacements[0].Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeOnDemand)).To(BeTrue())
			Expect(cmds[0].Replacements[0].Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeSpot)).To(BeFalse())

			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0], nodeClaims[1], nodeClaims[2])

			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2])
		},
			Entry("when the NodePool does not constrain capacity type", nil, false),
			Entry("when the NodePool allows Spot and on-demand", []v1.NodeSelectorRequirementWithMinValues{{
				Key:      v1.CapacityTypeLabelKey,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{v1.CapacityTypeSpot, v1.CapacityTypeOnDemand},
			}}, true),
		)
		It("can merge 3 nodes into 1 if the candidates have both spot and on-demand", func() {
			// By default all the 3 nodeClaims are OD.
			nodeClaims = lo.Ternary(false, spotNodeClaims, nodeClaims)
			nodes = lo.Ternary(false, spotNodes, nodes)
			// Change one of them to spot.
			nodeClaims[2].Labels = lo.Assign(nodeClaims[2].Labels, map[string]string{
				corev1.LabelInstanceTypeStable: mostExpensiveSpotInstance.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveSpotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveSpotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})
			nodes[2].Labels = lo.Assign(nodeClaims[2].Labels, map[string]string{
				corev1.LabelInstanceTypeStable: mostExpensiveSpotInstance.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveSpotOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveSpotOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			})
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2], nodePool)
			ExpectMakeNodesInitialized(ctx, env.Client, env.Clock, nodes[0], nodes[1], nodes[2])

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[2])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1], nodes[2]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0], nodeClaims[1], nodeClaims[2])

			// three nodeclaims should be replaced with a single nodeclaim
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2])
		})
		DescribeTable("won't merge 2 nodes into 1 of the same type",
			func(spotToSpot bool) {
				leastExpInstance := lo.Ternary(spotToSpot, leastExpensiveInstance, leastExpensiveSpotInstance)
				leastExpOffering := lo.Ternary(spotToSpot, leastExpensiveOffering, leastExpensiveSpotOffering)
				nodeClaims = lo.Ternary(spotToSpot, nodeClaims, spotNodeClaims)
				nodes = lo.Ternary(spotToSpot, nodes, spotNodes)
				// create our RS so we can link a pod to it
				rs := test.ReplicaSet()
				ExpectApplied(ctx, env.Client, rs)
				pods := test.Pods(3, test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: labels,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "ReplicaSet",
								Name:               rs.Name,
								UID:                rs.UID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}}})

				// Make the nodeclaims the least expensive instance type and make them of the same type
				nodeClaims[0].Labels = lo.Assign(nodeClaims[0].Labels, map[string]string{
					corev1.LabelInstanceTypeStable: leastExpInstance.Name,
					v1.CapacityTypeLabelKey:        leastExpOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       leastExpOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				})
				nodes[0].Labels = lo.Assign(nodes[0].Labels, map[string]string{
					corev1.LabelInstanceTypeStable: leastExpInstance.Name,
					v1.CapacityTypeLabelKey:        leastExpOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       leastExpOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				})
				nodeClaims[1].Labels = lo.Assign(nodeClaims[1].Labels, map[string]string{
					corev1.LabelInstanceTypeStable: leastExpInstance.Name,
					v1.CapacityTypeLabelKey:        leastExpOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       leastExpOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				})
				nodes[1].Labels = lo.Assign(nodes[1].Labels, map[string]string{
					corev1.LabelInstanceTypeStable: leastExpInstance.Name,
					v1.CapacityTypeLabelKey:        leastExpOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       leastExpOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				})
				ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)
				ExpectMakeNodesInitialized(ctx, env.Client, env.Clock, nodes[0], nodes[1])

				// bind pods to nodes
				ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
				ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])
				ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

				// inform cluster state about nodes and nodeclaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
				ExpectSingletonReconciled(ctx, disruptionController)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

				// Cascade any deletion of the nodeclaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0])

				// We have [cheap-node, cheap-node] which multi-node consolidation could consolidate via
				// [delete cheap-node, delete cheap-node, launch cheap-node]. This isn't the best method though
				// as we should instead just delete one of the nodes instead of deleting both and launching a single
				// identical replacement. This test verifies the filterOutSameType function from multi-node consolidation
				// works to ensure we perform the least-disruptive action.
				Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
				Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
				// should have just deleted the node with the fewest pods
				ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0])
				// and left the other node alone
				ExpectExists(ctx, env.Client, nodeClaims[1])
				ExpectExists(ctx, env.Client, nodes[1])
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		DescribeTable("should wait for the node TTL for non-empty nodes before consolidating (multi-node)",
			func(spotToSpot bool) {
				disruptionController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(NewMethodsWithRealValidator()...))
				nodeClaims = lo.Ternary(spotToSpot, nodeClaims, spotNodeClaims)
				nodes = lo.Ternary(spotToSpot, nodes, spotNodes)
				// create our RS so we can link a pod to it
				rs := test.ReplicaSet()
				ExpectApplied(ctx, env.Client, rs)
				pods := test.Pods(3, test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: labels,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "ReplicaSet",
								Name:               rs.Name,
								UID:                rs.UID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}}})

				ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)

				// bind pods to nodes
				ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
				ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
				ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

				// inform cluster state about nodes and nodeclaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})

				finished := atomic.Bool{}
				ExpectParallelized(
					func() {
						defer finished.Store(true)
						ExpectSingletonReconciled(ctx, disruptionController)
					},
					func() {
						// wait for the controller to block on the validation timeout
						Eventually(env.Clock.HasWaiters, time.Second*5).Should(BeTrue())
						// controller should be blocking during the timeout
						Expect(finished.Load()).To(BeFalse())
						// and the node should not be deleted yet
						ExpectExists(ctx, env.Client, nodeClaims[0])
						ExpectExists(ctx, env.Client, nodeClaims[1])

						// advance the clock so that the timeout expires
						env.Clock.Step(31 * time.Second)

						// controller should finish
						Eventually(finished.Load, 10*time.Second).Should(BeTrue())
					},
				)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
				ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

				// Cascade any deletion of the nodeclaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0], nodeClaims[1])

				// should launch a single smaller replacement node
				Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
				Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
				// and delete the two large ones
				ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1])
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		DescribeTable("should consolidate multiple empty nodes via multi-nodeclaim consolidation",
			func(spotToSpot bool) {
				disruptionController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(NewMethodsWithRealValidator()...))
				nodeClaims = lo.Ternary(spotToSpot, nodeClaims, spotNodeClaims)
				nodes = lo.Ternary(spotToSpot, nodes, spotNodes)
				// create our RS so we can link a pod to it
				rs := test.ReplicaSet()
				ExpectApplied(ctx, env.Client, rs)
				pods := test.Pods(3, test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: labels,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "ReplicaSet",
								Name:               rs.Name,
								UID:                rs.UID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}}})

				// Apply nodes without binding pods (nodes are empty)
				ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2], nodePool)
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1], nodes[2]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2]})

				finished := atomic.Bool{}
				ExpectParallelized(
					func() {
						defer finished.Store(true)
						ExpectSingletonReconciled(ctx, disruptionController)
					},
					func() {
						// wait for the controller to block on the validation timeout (multi-node)
						Eventually(env.Clock.HasWaiters, time.Second*5).Should(BeTrue())
						// controller should be blocking during the timeout
						Expect(finished.Load()).To(BeFalse())
						// and the node should not be deleted yet
						ExpectExists(ctx, env.Client, nodeClaims[0])
						ExpectExists(ctx, env.Client, nodeClaims[1])
						ExpectExists(ctx, env.Client, nodeClaims[2])

						// advance the clock so that the multi-node validation timeout expires
						env.Clock.Step(31 * time.Second)
						Eventually(finished.Load, 10*time.Second).Should(BeTrue())
					},
				)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))

				// Multi-node consolidation deletes all empty nodes
				ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

				// Cascade any deletion of the nodeclaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0], nodeClaims[1], nodeClaims[2])

				Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
				Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
		DescribeTable("should continue to single nodeclaim consolidation when multi-nodeclaim consolidation fails validation after the node ttl",
			func(spotToSpot bool) {
				disruptionController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(NewMethodsWithRealValidator()...))
				nodeClaims = lo.Ternary(spotToSpot, nodeClaims, spotNodeClaims)
				nodes = lo.Ternary(spotToSpot, nodes, spotNodes)
				// create our RS so we can link a pod to it
				rs := test.ReplicaSet()
				ExpectApplied(ctx, env.Client, rs)
				pods := test.Pods(3, test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: labels,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "ReplicaSet",
								Name:               rs.Name,
								UID:                rs.UID,
								Controller:         new(true),
								BlockOwnerDeletion: new(true),
							},
						}}})

				ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2], nodePool)

				// bind pods to nodes
				ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
				ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])
				ExpectManualBinding(ctx, env.Client, pods[2], nodes[2])

				// inform cluster state about nodes and nodeclaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1], nodes[2]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2]})

				var wg sync.WaitGroup
				wg.Add(1)
				finished := atomic.Bool{}
				ExpectParallelized(
					func() {
						defer finished.Store(true)
						ExpectSingletonReconciled(ctx, disruptionController)
					},
					func() {
						// wait for the controller to block on the validation timeout
						Eventually(env.Clock.HasWaiters, time.Second*5).Should(BeTrue())
						// controller should be blocking during the timeout
						Expect(finished.Load()).To(BeFalse())

						// and the node should not be deleted yet
						ExpectExists(ctx, env.Client, nodeClaims[0])
						ExpectExists(ctx, env.Client, nodeClaims[1])
						ExpectExists(ctx, env.Client, nodeClaims[2])

						var extraPods []*corev1.Pod
						for range 2 {
							extraPods = append(extraPods, test.Pod(test.PodOptions{
								ResourceRequirements: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceCPU: *resource.NewQuantity(1, resource.DecimalSI)},
								},
							}))
						}
						ExpectApplied(ctx, env.Client, extraPods[0], extraPods[1])
						// bind the extra pods to node1 and node 2 to make the consolidation decision invalid
						// we bind to 2 nodes so we can deterministically expect that node3 is consolidated in
						// single nodeclaim consolidation
						ExpectManualBinding(ctx, env.Client, extraPods[0], nodes[0])
						ExpectManualBinding(ctx, env.Client, extraPods[1], nodes[1])

						ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[0]))
						ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[1]))

						// advance the clock so that the timeout expires for multi-nodeclaim consolidation
						env.Clock.Step(31 * time.Second)

						// wait for the controller to block on the validation timeout for single nodeclaim consolidation
						Eventually(env.Clock.HasWaiters, time.Second*5).Should(BeTrue())
						// advance the clock so that the timeout expires for single nodeclaim consolidation
						env.Clock.Step(31 * time.Second)

						// controller should finish
						Eventually(finished.Load, 10*time.Second).Should(BeTrue())
					},
				)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

				// Cascade any deletion of the nodeclaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0], nodeClaims[1], nodeClaims[2])

				// should have 2 nodes after single nodeclaim consolidation deletes one
				Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
				Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
				// and delete node3 in single nodeclaim consolidation
				ExpectNotFound(ctx, env.Client, nodeClaims[2], nodes[2])
			},
			Entry("if the candidate is on-demand node", false),
			Entry("if the candidate is spot node", true),
		)
	})
	Context("Node Lifetime Consideration", func() {
		var nodeClaims []*v1.NodeClaim
		var nodes []*corev1.Node

		BeforeEach(func() {
			nodePool.Spec.Template.Spec.ExpireAfter = v1.MustParseNillableDuration("3s")
			nodeClaims, nodes = test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Spec: v1.NodeClaimSpec{
					ExpireAfter: v1.MustParseNillableDuration("3s"),
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
		})
		It("should consolidate the node with best savings ratio first", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0]) // ensure node1 is the oldest node
			time.Sleep(2 * time.Second)                             // this sleep is unfortunate, but necessary.  The creation time is from etcd, and we can't mock it, so we
			// need to sleep to force the second node to be created a bit after the first node.
			ExpectApplied(ctx, env.Client, nodeClaims[1], nodes[1])

			// two pods on node 0, one on node 1
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			env.Clock.SetTime(time.Now())
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaims[1])

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			// Node 1 has fewer pods (lower RescheduleDisruptionCost) so better
			// savings ratio (price/disruption). It sorts first and consolidates.
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])
		})
	})
	Context("Topology Consideration", func() {
		var nodeClaims []*v1.NodeClaim
		var nodes []*corev1.Node
		var oldNodeClaimNames sets.Set[string]

		BeforeEach(func() {
			testZone1Instance := leastExpensiveInstanceWithZone("test-zone-1")
			testZone2Instance := mostExpensiveInstanceWithZone("test-zone-2")
			testZone3Instance := leastExpensiveInstanceWithZone("test-zone-3")

			nodeClaims, nodes = test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelTopologyZone:       "test-zone-1",
						corev1.LabelInstanceTypeStable: testZone1Instance.Name,
						v1.CapacityTypeLabelKey:        testZone1Instance.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("1")},
				},
			})
			nodeClaims[1].Labels = lo.Assign(nodeClaims[1].Labels, map[string]string{
				corev1.LabelTopologyZone:       "test-zone-2",
				corev1.LabelInstanceTypeStable: testZone2Instance.Name,
				v1.CapacityTypeLabelKey:        testZone2Instance.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
			})
			nodes[1].Labels = lo.Assign(nodes[1].Labels, map[string]string{
				corev1.LabelTopologyZone:       "test-zone-2",
				corev1.LabelInstanceTypeStable: testZone2Instance.Name,
				v1.CapacityTypeLabelKey:        testZone2Instance.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
			})

			nodeClaims[2].Labels = lo.Assign(nodeClaims[2].Labels, map[string]string{
				corev1.LabelTopologyZone:       "test-zone-3",
				corev1.LabelInstanceTypeStable: testZone3Instance.Name,
				v1.CapacityTypeLabelKey:        testZone3Instance.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
			})
			nodes[2].Labels = lo.Assign(nodes[2].Labels, map[string]string{
				corev1.LabelTopologyZone:       "test-zone-3",
				corev1.LabelInstanceTypeStable: testZone3Instance.Name,
				v1.CapacityTypeLabelKey:        testZone3Instance.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
			})
			oldNodeClaimNames = sets.New(nodeClaims[0].Name, nodeClaims[1].Name, nodeClaims[2].Name)
			for _, nc := range nodeClaims {
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}
		})
		It("can replace node maintaining zonal topology spread", func() {
			labels = map[string]string{
				"app": "test-zonal-spread",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			tsc := corev1.TopologySpreadConstraint{
				MaxSkew:           1,
				TopologyKey:       corev1.LabelTopologyZone,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
			}
			pods := test.Pods(4, test.PodOptions{
				ResourceRequirements:      corev1.ResourceRequirements{Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("1")}},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{tsc},
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2], nodePool)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[2])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1], nodes[2]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2]})

			ExpectSkew(ctx, env.Client, "default", &tsc).To(ConsistOf(1, 1, 1))
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			// should create a new node as there is a cheaper one that can hold the pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(3))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(3))
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])

			// Find the new node associated with the nodeclaim
			newNodeClaim, ok := lo.Find(ExpectNodeClaims(ctx, env.Client), func(m *v1.NodeClaim) bool {
				return !oldNodeClaimNames.Has(m.Name)
			})
			Expect(ok).To(BeTrue())
			newNode, ok := lo.Find(ExpectNodes(ctx, env.Client), func(n *corev1.Node) bool {
				return newNodeClaim.Status.ProviderID == n.Spec.ProviderID
			})
			Expect(ok).To(BeTrue())

			// we need to emulate the replicaset controller and bind a new pod to the newly created node
			ExpectApplied(ctx, env.Client, pods[3])
			ExpectManualBinding(ctx, env.Client, pods[3], newNode)

			// we should maintain our skew, the new node must be in the same zone as the old node it replaced
			ExpectSkew(ctx, env.Client, "default", &tsc).To(ConsistOf(1, 1, 1))
		})
		It("won't delete node if it would violate pod anti-affinity", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("1")}},
				PodAntiRequirements: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
						TopologyKey:   corev1.LabelHostname,
					},
				},
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					},
				},
			})

			// Make the Zone 2 instance also the least expensive instance
			zone2Instance := leastExpensiveInstanceWithZone("test-zone-2")
			nodes[1].Labels = lo.Assign(nodes[1].Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelTopologyZone:       "test-zone-2",
				corev1.LabelInstanceTypeStable: zone2Instance.Name,
				v1.CapacityTypeLabelKey:        zone2Instance.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
			})
			nodeClaims[1].Labels = lo.Assign(nodeClaims[1].Labels, map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelTopologyZone:       "test-zone-2",
				corev1.LabelInstanceTypeStable: zone2Instance.Name,
				v1.CapacityTypeLabelKey:        zone2Instance.Offerings[0].Requirements.Get(v1.CapacityTypeLabelKey).Any(),
			})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2], nodePool)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[2])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1], nodes[2]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// our nodes are already the cheapest available, so we can't replace them.  If we delete, it would
			// violate the anti-affinity rule, so we can't do anything.
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(3))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(3))
			ExpectExists(ctx, env.Client, nodeClaims[0])
			ExpectExists(ctx, env.Client, nodeClaims[1])
			ExpectExists(ctx, env.Client, nodeClaims[2])
		})
	})
	Context("Parallelization", func() {
		It("should not schedule an additional node when receiving pending pods while consolidating", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					},
				},
			})

			node.Finalizers = []string{"karpenter.sh/test-finalizer"}
			nodeClaim.Finalizers = []string{"karpenter.sh/test-finalizer"}

			ExpectApplied(ctx, env.Client, rs, pod, nodeClaim, node, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			ExpectSingletonReconciled(ctx, disruptionController)
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])

			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))

			// Add a new pending pod that should schedule while node is not yet deleted
			pod = test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should not consolidate a node that is launched for pods on a deleting node", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			podOpts := test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"),
					},
				},
			}

			var pods []*corev1.Pod
			for range 5 {
				pod := test.UnschedulablePod(podOpts)
				pods = append(pods, pod)
			}
			ExpectApplied(ctx, env.Client, rs, nodePool)
			ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, lo.Map(pods, func(p *corev1.Pod, _ int) *corev1.Pod { return p.DeepCopy() })...)

			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodes).To(HaveLen(1))

			// Update cluster state with new node
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[0]))

			// Mark the node for deletion and re-trigger reconciliation
			oldNodeName := nodes[0].Name
			cluster.MarkForDeletion(nodes[0].Spec.ProviderID)
			ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, lo.Map(pods, func(p *corev1.Pod, _ int) *corev1.Pod { return p.DeepCopy() })...)

			// Make sure that the cluster state is aware of the current node state
			nodes = ExpectNodes(ctx, env.Client)
			Expect(nodes).To(HaveLen(2))
			newNode, _ := lo.Find(nodes, func(n *corev1.Node) bool { return n.Name != oldNodeName })

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nil)

			// Wait for the nomination cache to expire
			time.Sleep(time.Second * 11)

			// Re-create the pods to re-bind them
			for i := range 2 {
				ExpectDeleted(ctx, env.Client, pods[i])
				pod := test.UnschedulablePod(podOpts)
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, newNode)
			}

			// Trigger a reconciliation run which should take into account the deleting node
			// consolidation shouldn't trigger additional actions
			result, err := disruptionController.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		})
	})
	Context("Reserved Capacity", func() {
		var reservedNodeClaim *v1.NodeClaim
		var reservedNode *corev1.Node
		var mostExpensiveReservationID string

		BeforeEach(func() {
			mostExpensiveReservationID = fmt.Sprintf("r-%s", mostExpensiveInstance.Name)
			mostExpensiveInstance.Requirements.Add(scheduling.NewRequirement(
				cloudprovider.ReservationIDLabel,
				corev1.NodeSelectorOpIn,
				mostExpensiveReservationID,
			))
			mostExpensiveInstance.Requirements.Get(v1.CapacityTypeLabelKey).Insert(v1.CapacityTypeReserved)
			mostExpensiveInstance.Offerings = append(mostExpensiveInstance.Offerings, &cloudprovider.Offering{
				Price:               mostExpensiveOffering.Price / 1_000_000.0,
				Available:           true,
				Compatible:          true,
				ReservationCapacity: 10,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:     v1.CapacityTypeReserved,
					corev1.LabelTopologyZone:    mostExpensiveOffering.Zone(),
					v1alpha1.LabelReservationID: mostExpensiveReservationID,
				}),
			})
			ExpectSingletonReconciled(ctx, pricingController)
			reservedNodeClaim, reservedNode = test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:              nodePool.Name,
						corev1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
						corev1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
						cloudprovider.ReservationIDLabel: mostExpensiveReservationID,
					},
				},
			})
			reservedNodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{ReservedCapacity: new(true)}}))
		})
		It("can consolidate from one reserved offering to another", func() {
			leastExpensiveReservationID := fmt.Sprintf("r-%s", leastExpensiveInstance.Name)
			leastExpensiveInstance.Requirements.Add(scheduling.NewRequirement(
				cloudprovider.ReservationIDLabel,
				corev1.NodeSelectorOpIn,
				leastExpensiveReservationID,
			))
			leastExpensiveInstance.Requirements.Get(v1.CapacityTypeLabelKey).Insert(v1.CapacityTypeReserved)
			leastExpensiveInstance.Offerings = append(leastExpensiveInstance.Offerings, &cloudprovider.Offering{
				Price:               leastExpensiveOffering.Price / 1_000_000.0,
				Available:           true,
				Compatible:          true,
				ReservationCapacity: 10,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:     v1.CapacityTypeReserved,
					corev1.LabelTopologyZone:    leastExpensiveOffering.Zone(),
					v1alpha1.LabelReservationID: leastExpensiveReservationID,
				}),
			})

			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         new(true),
						BlockOwnerDeletion: new(true),
					},
				},
			}})
			ExpectApplied(ctx, env.Client, rs, pod, reservedNode, reservedNodeClaim, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, reservedNode)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{reservedNode}, []*v1.NodeClaim{reservedNodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, reservedNodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, reservedNodeClaim)

			// should create a new nodeclaim as there is a cheaper one that can hold the pod
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodes).To(HaveLen(1))

			Expect(nodeClaims[0].Name).ToNot(Equal(reservedNodeClaim.Name))

			// We should have consolidated into the same instance type, just into reserved.
			Expect(nodes[0].Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, leastExpensiveInstance.Name))
			Expect(nodes[0].Labels).To(HaveKeyWithValue(v1.CapacityTypeLabelKey, v1.CapacityTypeReserved))
			Expect(nodes[0].Labels).To(HaveKeyWithValue(cloudprovider.ReservationIDLabel, leastExpensiveReservationID))

			// and delete the old one
			ExpectNotFound(ctx, env.Client, reservedNodeClaim, reservedNode)
		})
		DescribeTable(
			"can consolidate into reserved capacity for the same instance pool",
			func(initialCapacityType string) {
				if initialCapacityType == v1.CapacityTypeSpot {
					nodeClaim = spotNodeClaim
					node = spotNode
				}

				// create our RS so we can link a pod to it
				rs := test.ReplicaSet()
				ExpectApplied(ctx, env.Client, rs)
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

				pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         new(true),
							BlockOwnerDeletion: new(true),
						},
					},
				}})
				ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool)

				// bind pods to node
				ExpectManualBinding(ctx, env.Client, pod, node)

				// inform cluster state about nodes and nodeClaims
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
				ExpectSingletonReconciled(ctx, disruptionController)

				// Process the item so that the nodes can be deleted.
				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
				ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)

				// Cascade any deletion of the nodeclaim to the node
				ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

				// should create a new nodeclaim as there is a cheaper one that can hold the pod
				nodeClaims := ExpectNodeClaims(ctx, env.Client)
				nodes := ExpectNodes(ctx, env.Client)
				Expect(nodeClaims).To(HaveLen(1))
				Expect(nodes).To(HaveLen(1))

				Expect(nodeClaims[0].Name).ToNot(Equal(nodeClaim.Name))

				// We should have consolidated into the same instance type, just into reserved.
				Expect(nodes[0].Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, mostExpensiveInstance.Name))
				Expect(nodes[0].Labels).To(HaveKeyWithValue(v1.CapacityTypeLabelKey, v1.CapacityTypeReserved))
				Expect(nodes[0].Labels).To(HaveKeyWithValue(cloudprovider.ReservationIDLabel, mostExpensiveReservationID))

				// and delete the old one
				ExpectNotFound(ctx, env.Client, nodeClaim, node)
			},
			Entry("from on-demand", v1.CapacityTypeOnDemand),
			Entry("from spot", v1.CapacityTypeSpot),
		)
	})
	Context("Preferences", func() {
		It("should consolidate a node through deletion when ignoring preferences", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{PreferencePolicy: lo.ToPtr(options.PreferencePolicyIgnore)}))
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
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
						corev1.ResourceCPU:  resource.MustParse("5"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			for _, nc := range nodeClaims {
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "foo"},
				},
				PodAntiPreferences: []corev1.WeightedPodAffinityTerm{
					{
						Weight: 1,
						PodAffinityTerm: corev1.PodAffinityTerm{
							TopologyKey: corev1.LabelHostname,
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "foo",
								},
							},
						},
					},
				},
			})

			ExpectApplied(ctx, env.Client, pods[0], pods[1], pods[2], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[1])

			// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaims[1], nodes[1])
		})
		It("should consolidate a node through replacement when ignoring preferences", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{PreferencePolicy: lo.ToPtr(options.PreferencePolicyIgnore)}))
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "foo"},
				},
				NodePreferences: []corev1.NodeSelectorRequirement{
					{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{mostExpensiveInstance.Name},
					},
				},
			})
			ExpectApplied(ctx, env.Client, pod, node, nodeClaim, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectSingletonReconciled(ctx, disruptionController)

			// Process the item so that the nodes can be deleted.
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// should create a new nodeclaim as there is a cheaper one that can hold the pod
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodes).To(HaveLen(1))

			// Expect that the new nodeclaim does not request the most expensive instance type
			Expect(nodeClaims[0].Name).ToNot(Equal(nodeClaim.Name))
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Has(corev1.LabelInstanceTypeStable)).To(BeTrue())
			Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Has(mostExpensiveInstance.Name)).To(BeFalse())

			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
	})
	Context("MinValuesPolicy", func() {
		var nodePoolWithMinValues *v1.NodePool

		BeforeEach(func() {
			// Create a nodepool with instance type minValues requirement
			nodePoolWithMinValues = test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Weight: new(int32(100)),
					Template: v1.NodeClaimTemplate{
						Spec: v1.NodeClaimTemplateSpec{
							Requirements: []v1.NodeSelectorRequirementWithMinValues{
								{
									Key:      corev1.LabelInstanceTypeStable,
									Operator: corev1.NodeSelectorOpIn,
									Values: lo.Map(cloudProvider.InstanceTypes, func(it *cloudprovider.InstanceType, _ int) string {
										return it.Name
									}),
									MinValues: new(3),
								},
							},
						},
					},
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
						Budgets: []v1.Budget{{
							Nodes: "100%",
						}},
						ConsolidateAfter: v1.MustParseNillableDuration("0s"),
					},
				},
			})

			// Update instance types to ensure that min values won't be satisfied.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{leastExpensiveInstance, mostExpensiveInstance}
		})

		It("should not consolidate a node by relaxing min values when policy is set to BestEffort", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{MinValuesPolicy: lo.ToPtr(options.MinValuesPolicyBestEffort)}))
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePoolWithMinValues.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveInstance.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveInstance.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("5"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			for _, nc := range nodeClaims {
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}
			pods := test.Pods(1, test.PodOptions{})

			ExpectApplied(ctx, env.Client, pods[0], nodeClaims[0], nodes[0], nodePoolWithMinValues)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0]}, []*v1.NodeClaim{nodeClaims[0]})
			result, err := disruptionController.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Validate that nothing changed.
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodeClaims(ctx, env.Client)[0].Name).To(Equal(nodeClaims[0].Name))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)[0].Name).To(Equal(nodes[0].Name))

			// Expect Unconsolidatable events to be fired for min values violation.
			Expect(lo.Filter(recorder.Events(), func(e events.Event, _ int) bool {
				return e.Reason == events.Unconsolidatable && strings.Contains(e.Message, "minValues requirement is not met for label(s) (label(s)=[node.kubernetes.io/instance-type])")
			})).To(HaveLen(2))
		})
	})
	Context("Buffer Pods", func() {
		It("should not empty-consolidate a node with only buffer pods", func() {
			// Node has no real pods but has buffer pods — should NOT be deleted.
			// Single/multi consolidation skips it (reschedulablePods==0).
			// Emptiness would catch it, but HasBufferPods blocks.
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			cluster.UpdateBufferPodCounts(map[string]int{
				node.Spec.ProviderID: 5,
			})

			ExpectSingletonReconciled(ctx, disruptionController)

			// Node should still exist
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
		})

		It("should allow consolidation of a node with real pods and buffer pods to a cheaper instance", func() {
			// An expensive node with a small real pod + buffer pods. The consolidation
			// simulation already includes virtual pods in pending (via GetPendingPods).
			// A cheaper replacement that fits both real + virtual pods is legitimate.
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// Node hosts buffer pods (but also has a real pod making it a consolidation candidate)
			cluster.UpdateBufferPodCounts(map[string]int{
				node.Spec.ProviderID: 2,
			})

			ExpectSingletonReconciled(ctx, disruptionController)

			// The node is on the most expensive instance type with only 1 CPU pod.
			// Consolidation should propose a cheaper replacement.
			cmds := queue.GetCommands()
			Expect(len(cmds)).To(BeNumerically(">=", 1))
		})

		It("should skip buffer-only nodes in single-node consolidation but protect via emptiness", func() {
			// Two nodes: node1 has only buffer pods, node2 has a real pod.
			// node1 should not appear in any consolidation command.
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
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

			// Only node2 gets a real pod
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], nodeClaims[1], nodes[0], nodes[1], pod)
			ExpectManualBinding(ctx, env.Client, pod, nodes[1])
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			// node1 has only buffer pods, node2 has real pod + buffer pods
			cluster.UpdateBufferPodCounts(map[string]int{
				nodes[0].Spec.ProviderID: 4,
				nodes[1].Spec.ProviderID: 2,
			})

			ExpectSingletonReconciled(ctx, disruptionController)

			// node1 (buffer-only) should never be in a consolidation command
			cmds := queue.GetCommands()
			for _, cmd := range cmds {
				for _, c := range cmd.Candidates {
					Expect(c.Name()).ToNot(Equal(nodes[0].Name),
						"buffer-only node should not be a consolidation candidate")
				}
			}
			// node1 should still exist (protected by emptiness buffer check)
			ExpectExists(ctx, env.Client, nodeClaims[0])
		})

		It("should consolidate a buffer node once buffer pods are cleared", func() {
			// Node previously had buffer pods, now they're cleared — it becomes
			// eligible for empty consolidation.
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// First pass: buffer pods present → protected
			cluster.UpdateBufferPodCounts(map[string]int{
				node.Spec.ProviderID: 3,
			})
			ExpectSingletonReconciled(ctx, disruptionController)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))

			// Second pass: buffer deleted → counts cleared
			cluster.UpdateBufferPodCounts(map[string]int{})
			ExpectSingletonReconciled(ctx, disruptionController)
			ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})
	})
	Context("Static NodePool", func() {
		It("should not consolidate static NodePool nodes", func() {
			staticNp := test.StaticNodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Replicas:   new(int64(2)), // Static nodepool with 2 desired replica
					Disruption: v1.Disruption{},
				},
			})

			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:        staticNp.Name,
						v1.NodeInitializedLabelKey: "true",
					},
				},
				Status: v1.NodeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})
			// This is not possible as we dont set ConditionTypeConsolidatable on static NodeClaims, incase user sets it
			for _, nc := range nodeClaims {
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}

			ExpectApplied(ctx, env.Client, staticNp, nodeClaims[0], nodeClaims[1], nodes[0], nodes[1])
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})

			ExpectSingletonReconciled(ctx, disruptionController)

			// Should not consolidate static NodeClaims
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))
		})
	})
})

// replicaSetPod returns a pod owned by a new ReplicaSet, so consolidation may reschedule it.
func replicaSetPod() *corev1.Pod {
	rs := test.ReplicaSet()
	ExpectApplied(ctx, env.Client, rs)
	Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
	return test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"app": "test"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion:         "apps/v1",
			Kind:               "ReplicaSet",
			Name:               rs.Name,
			UID:                rs.UID,
			Controller:         new(true),
			BlockOwnerDeletion: new(true),
		}},
	}})
}

// fixedPricer prices every NodeClaim at the same hourly price.
type fixedPricer float64

func (p fixedPricer) NodeClaimPrice(context.Context, *v1.NodeClaim, *cloudprovider.InstanceType) (float64, bool) {
	return float64(p), true
}

// hookedRecorder forwards every event to the wrapped Recorder after running onPublish against it, letting a spec change
// the cluster at a point a disruption pass reports through an event, such as between two candidates' simulations.
type hookedRecorder struct {
	events.Recorder
	onPublish func(events.Event)
}

func (h *hookedRecorder) Publish(evts ...events.Event) {
	for _, evt := range evts {
		h.onPublish(evt)
	}
	h.Recorder.Publish(evts...)
}
