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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	karpenterevents "sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

type nodePoolGetErrorClient struct {
	client.Client
	err error
}

func (c *nodePoolGetErrorClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*v1.NodePool); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type podListErrorClient struct {
	client.Client
	err error
}

func (c *podListErrorClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		return c.err
	}
	return c.Client.List(ctx, list, opts...)
}

type terminationTimestampPatchErrorClient struct {
	client.Client
	err      error
	failNext bool
}

func (c *terminationTimestampPatchErrorClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if nodeClaim, ok := obj.(*v1.NodeClaim); ok &&
		c.failNext &&
		nodeClaim.Annotations[v1.NodeClaimTerminationTimestampAnnotationKey] != "" {
		c.failNext = false
		return c.err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// These tests exercise end-user behavior of voluntary node repair (Node Repair Resiliency design). Each It maps to an
// invariant (INV-S*) from the design. The disruption controller is built with only the Repair method so the behavior
// under test is isolated from consolidation/drift.

var _ = Describe("Repair", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	var repairController *disruption.Controller
	var repair *disruption.Repair

	labels := func() map[string]string {
		return map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}
	}

	// initNode applies + initializes a node/nodeclaim and syncs cluster state. Initialization overwrites
	// Status.Conditions with Ready=True, so unhealthy conditions must be stamped AFTER this via markUnhealthy.
	initNode := func(nc *v1.NodeClaim, n *corev1.Node) {
		ExpectApplied(ctx, env.Client, nc, n)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{n}, []*v1.NodeClaim{nc})
	}

	// bindReschedulablePod places a ReplicaSet-owned (reschedulable) pod on the node so pre-spin sizing has workload
	// demand to protect.
	bindReschedulablePod := func(n *corev1.Node) {
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"repair-test": "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
			}},
		}})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, n)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
	}

	// bindBlockingPod places a do-not-disrupt pod on the node. Such a pod blocks eviction, so the node is only a
	// disruption candidate when the drain is bounded by a hard deadline (repair's RepairPolicy TGP, or the NodeClaim TGP).
	bindBlockingPod := func(n *corev1.Node) {
		pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}}})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, n)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
	}

	// markUnhealthy appends a condition matching a RepairPolicy at the current fake-clock time, re-applies the node,
	// and re-syncs cluster state. Must be called AFTER initNode.
	markUnhealthyWithReason := func(n *corev1.Node, condType corev1.NodeConditionType, reason string) {
		n = ExpectExists(ctx, env.Client, n)
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
			Type:               condType,
			Status:             corev1.ConditionFalse,
			Reason:             reason,
			LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
		})
		ExpectApplied(ctx, env.Client, n)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
	}
	markUnhealthy := func(n *corev1.Node, condType corev1.NodeConditionType) {
		markUnhealthyWithReason(n, condType, "")
	}

	// newRepairController builds an isolated repair-only disruption controller. Repair caches RepairPolicies() at
	// construction, so specs that override cloudProvider.RepairPolicy must call this again to pick up the new policies.
	newRepairController := func() {
		repair = disruption.NewRepair(disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue))
		repairController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
			disruption.WithMethods(repair))
	}

	BeforeEach(func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true)}}))
		// Single default policy: BadNode/False, 30m toleration (the fake cloud provider default).
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		}
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		ExpectApplied(ctx, env.Client, nodePool)
		// Repair runs as an isolated method so tests assert repair behavior only.
		newRepairController()
	})

	// INV-S6 / INV-S9: repair pre-spins a replacement (replace-then-terminate) and only after the toleration elapses.
	It("should pre-spin a replacement and terminate the unhealthy node only after the replacement is healthy", func() {
		initNode(nodeClaim, node)
		bindReschedulablePod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute) // past toleration

		ExpectSingletonReconciled(ctx, repairController)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		// Pre-spin: the command carries a replacement (replace-then-terminate), it is NOT a bare delete.
		Expect(cmds[0].Decision()).To(Equal(disruption.ReplaceDecision))
		Expect(cmds[0].Replacements).To(HaveLen(1))

		Expect(ExpectExists(ctx, env.Client, nodeClaim).DeletionTimestamp.IsZero()).To(BeTrue())
		metricLabels := map[string]string{
			"condition":     "bad_node",
			"nodepool":      nodePool.Name,
			"capacity_type": v1.CapacityTypeOnDemand,
			"image_id":      cmds[0].Candidates[0].NodeClaim.Status.ImageID,
		}
		_, found := FindMetricWithLabelValues("karpenter_nodeclaims_unhealthy_disrupted_total", metricLabels)
		Expect(found).To(BeFalse())

		ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		ExpectMetricCounterValue(disruption.NodeClaimsUnhealthyDisruptedTotal, 1, metricLabels)
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)
		ExpectNotFound(ctx, env.Client, nodeClaim)
	})

	It("should delete an empty unhealthy node without pre-spinning a replacement", func() {
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Decision()).To(Equal(disruption.DeleteDecision))
		Expect(cmds[0].Replacements).To(BeEmpty())
	})

	// INV-S9: repair never fires before the policy toleration elapses.
	It("should not repair before the toleration duration elapses", func() {
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(10 * time.Minute) // still within the 30m toleration

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	It("should use a matching reason-specific policy instead of the fallback", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{
				ConditionType:      "BadNode",
				ConditionStatus:    corev1.ConditionFalse,
				ReasonRegex:        `^FastFailure$`,
				TolerationDuration: 10 * time.Minute,
				Action:             cloudprovider.ReplaceNode,
			},
			{
				ConditionType:      "BadNode",
				ConditionStatus:    corev1.ConditionFalse,
				TolerationDuration: 30 * time.Minute,
				Action:             cloudprovider.ReplaceNode,
			},
		}
		newRepairController()
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "FastFailure")
		env.Clock.Step(11 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	It("should preserve condition age across a reason-only change", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{
				ConditionType:      "BadNode",
				ConditionStatus:    corev1.ConditionFalse,
				ReasonRegex:        `^ImmediateReason$`,
				TolerationDuration: 30 * time.Minute,
				Action:             cloudprovider.ReplaceNode,
			},
			{
				ConditionType:      "BadNode",
				ConditionStatus:    corev1.ConditionFalse,
				TolerationDuration: 2 * time.Hour,
				Action:             cloudprovider.ReplaceNode,
			},
		}
		newRepairController()
		initNode(nodeClaim, node)

		storedNode := ExpectExists(ctx, env.Client, node)
		storedNode.Status.Conditions = append(storedNode.Status.Conditions, corev1.NodeCondition{
			Type:               "BadNode",
			Status:             corev1.ConditionFalse,
			Reason:             "UnknownReason",
			LastTransitionTime: metav1.NewTime(env.Clock.Now().Add(-time.Hour)),
		})
		ExpectApplied(ctx, env.Client, storedNode)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(storedNode))

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(BeEmpty())

		storedNode = ExpectExists(ctx, env.Client, node)
		for i := range storedNode.Status.Conditions {
			if storedNode.Status.Conditions[i].Type == "BadNode" {
				storedNode.Status.Conditions[i].Reason = "ImmediateReason"
			}
		}
		ExpectApplied(ctx, env.Client, storedNode)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(storedNode))

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	It("should suppress an eligible fallback while a matching reason-specific policy is waiting", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{
				ConditionType:      "BadNode",
				ConditionStatus:    corev1.ConditionFalse,
				ReasonRegex:        `FastFailure`,
				TolerationDuration: 30 * time.Minute,
				Action:             cloudprovider.ReplaceNode,
			},
			{
				ConditionType:   "BadNode",
				ConditionStatus: corev1.ConditionFalse,
				Action:          cloudprovider.ReplaceNode,
			},
		}
		newRepairController()
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "PrefixFastFailureSuffix")
		env.Clock.Step(10 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	It("should use the condition fallback for an unknown reason", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{
				ConditionType:      "BadNode",
				ConditionStatus:    corev1.ConditionFalse,
				ReasonRegex:        `^KnownFailure$`,
				TolerationDuration: 10 * time.Minute,
				Action:             cloudprovider.ReplaceNode,
			},
			{
				ConditionType:      "BadNode",
				ConditionStatus:    corev1.ConditionFalse,
				TolerationDuration: 30 * time.Minute,
				Action:             cloudprovider.ReplaceNode,
			},
		}
		newRepairController()
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "UnknownFailure")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	// INV-S7: a replacement that does not initialize (bad AMI / partitioned zone) never leads to terminating the
	// original.
	It("should never terminate the original when the replacement fails to come up healthy", func() {
		initNode(nodeClaim, node)
		bindReschedulablePod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))

		// Do NOT make the replacement ready — simulate a replacement that never becomes healthy. Reconciling the queue
		// leaves the replacement uninitialized, so the candidate is never deleted.
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).DeletionTimestamp.IsZero()).To(BeTrue())
	})

	// INV-S8: do-not-repair blocks repair; do-not-disrupt does NOT (no behavior change for that annotation).
	It("should not repair a node carrying the do-not-repair annotation", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	It("should still repair a node carrying only the do-not-disrupt annotation", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		// do-not-disrupt does not block repair: a command is still produced.
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	// do-not-repair blocks only on the literal value "true"; any other value does not block repair.
	It("should still repair a node whose do-not-repair annotation is not \"true\"", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "false"})
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	// INV-S1 / INV-S5: repair is paced by the disruption budget — a cohort is disrupted a fraction at a time, not all
	// at once. The default 10% budget permits one concurrent repair.
	It("should pace repair by the disruption budget", func() {
		const count = 10
		nodeClaims, nodes := test.NodeClaimsAndNodes(count, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		markUnhealthy(nodes[0], "BadNode")
		markUnhealthy(nodes[1], "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	It("should not repair when the disruption budget is zero", func() {
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0"}}
		ExpectApplied(ctx, env.Client, nodePool)
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(BeEmpty())
	})

	It("should stop repairing a NodePool when more than 20% of its nodes are unhealthy", func() {
		const count = 10
		nodeClaims, nodes := test.NodeClaimsAndNodes(count, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		// Only the first condition is eligible. The breaker must also count the two fresh conditions because correlated
		// failure protection begins when a node becomes unhealthy, not after its repair toleration elapses.
		markUnhealthy(nodes[0], "BadNode")
		env.Clock.Step(31 * time.Minute)
		markUnhealthy(nodes[1], "BadNode")
		markUnhealthy(nodes[2], "BadNode")

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(BeEmpty())
		blockedEvents := lo.Filter(recorder.Events(), func(event karpenterevents.Event, _ int) bool {
			return event.Reason == karpenterevents.NodeRepairBlocked
		})
		Expect(blockedEvents).To(HaveLen(3))
		Expect(lo.Map(blockedEvents, func(event karpenterevents.Event, _ int) string {
			return string(event.InvolvedObject.(metav1.Object).GetUID())
		})).To(ConsistOf(string(nodes[0].UID), string(nodeClaims[0].UID), string(nodePool.UID)))
		for _, event := range blockedEvents {
			Expect(event.Type).To(Equal(corev1.EventTypeWarning))
		}
	})

	It("should round the repair circuit-breaker threshold up for small NodePools", func() {
		const count = 3
		nodeClaims, nodes := test.NodeClaimsAndNodes(count, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		markUnhealthy(nodes[0], "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	// INV-S4: ordering is prioritizable — a higher-priority fault repairs before a lower-priority one in the same pass.
	It("should repair the higher-priority condition first", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10, Action: cloudprovider.ReplaceNode},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 90, Action: cloudprovider.ReplaceNode},
		}
		newRepairController()
		// Include healthy nodes to exercise ordering within a mixed NodePool.
		healthyClaims, healthyNodes := test.NodeClaimsAndNodes(8, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range healthyNodes {
			initNode(healthyClaims[i], healthyNodes[i])
		}
		lowClaim, lowNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		highClaim, highNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		initNode(lowClaim, lowNode)
		initNode(highClaim, highNode)
		markUnhealthy(lowNode, "LowPriority")
		markUnhealthy(highNode, "HighPriority")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		// The budget allows one; ordering must pick the high-priority node.
		Expect(cmds[0].Candidates[0].Node.Name).To(Equal(highNode.Name))
	})

	It("should not score a node using a reason-specific policy that does not match", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Critical$", TolerationDuration: 30 * time.Minute, Priority: 90, Action: cloudprovider.ReplaceNode},
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10, Action: cloudprovider.ReplaceNode},
			{ConditionType: "MediumPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 50, Action: cloudprovider.ReplaceNode},
		}
		newRepairController()
		healthyClaims, healthyNodes := test.NodeClaimsAndNodes(8, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range healthyNodes {
			initNode(healthyClaims[i], healthyNodes[i])
		}
		fallbackClaim, fallbackNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		mediumClaim, mediumNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		initNode(fallbackClaim, fallbackNode)
		initNode(mediumClaim, mediumNode)
		markUnhealthyWithReason(fallbackNode, "Diagnostic", "Unknown")
		markUnhealthy(mediumNode, "MediumPriority")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Candidates[0].Node.Name).To(Equal(mediumNode.Name))
	})

	It("should not score a node using a matching policy whose toleration has not elapsed", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^failure$", TolerationDuration: time.Hour, Priority: 100, Action: cloudprovider.ReplaceNode},
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "failure", Priority: 0, Action: cloudprovider.ReplaceNode},
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, Priority: 0, Action: cloudprovider.ReplaceNode},
			{ConditionType: "MediumPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", Priority: 50, Action: cloudprovider.ReplaceNode},
		}
		newRepairController()
		healthyClaims, healthyNodes := test.NodeClaimsAndNodes(8, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range healthyNodes {
			initNode(healthyClaims[i], healthyNodes[i])
		}
		waitingClaim, waitingNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		mediumClaim, mediumNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		initNode(waitingClaim, waitingNode)
		initNode(mediumClaim, mediumNode)
		markUnhealthyWithReason(waitingNode, "Diagnostic", "failure")
		markUnhealthy(mediumNode, "MediumPriority")

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Candidates[0].Node.Name).To(Equal(mediumNode.Name))
	})

	It("should resolve the governing condition using only policies matching its reason", func() {
		diagnosticGracePeriod := 10 * time.Minute
		mediumGracePeriod := 5 * time.Minute
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Critical$", TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(time.Duration(0)), Priority: 90, Action: cloudprovider.ReplaceNode},
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: &diagnosticGracePeriod, Priority: 10, Action: cloudprovider.ReplaceNode},
			{ConditionType: "MediumPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, TerminationGracePeriod: &mediumGracePeriod, Priority: 50, Action: cloudprovider.ReplaceNode},
		}
		newRepairController()
		healthyClaims, healthyNodes := test.NodeClaimsAndNodes(4, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range healthyNodes {
			initNode(healthyClaims[i], healthyNodes[i])
		}
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "Diagnostic", "Unknown")
		markUnhealthy(node, "MediumPriority")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Candidates[0].TerminationGracePeriod).NotTo(BeNil())
		Expect(*cmds[0].Candidates[0].TerminationGracePeriod).To(Equal(mediumGracePeriod))
	})

	// INV-S4: a node's score is the argmax of rank+age/τ over ALL its matching conditions, not the score of its
	// highest-priority condition. Node A has a fresh high-priority condition (just past toleration) and a long-starving
	// low-priority one; node B has a moderately-aged high-priority condition. Scoring A off only its high-priority
	// (fresh) condition would rank it below B and repair B first; the argmax lifts A above B on its starving condition.
	It("should order a node by its most urgent condition, not just its highest-priority one", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10, Action: cloudprovider.ReplaceNode},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 90, Action: cloudprovider.ReplaceNode},
		}
		newRepairController()
		// Healthy peers ensure candidate ordering is evaluated in a realistically populated NodePool.
		healthyClaims, healthyNodes := test.NodeClaimsAndNodes(8, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range healthyNodes {
			initNode(healthyClaims[i], healthyNodes[i])
		}
		aClaim, aNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		bClaim, bNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		initNode(aClaim, aNode)
		initNode(bClaim, bNode)

		markUnhealthy(aNode, "LowPriority")  // A's low-priority condition starts starving now
		env.Clock.Step(60 * time.Minute)     //
		markUnhealthy(bNode, "HighPriority") // B's high-priority condition is moderately aged
		env.Clock.Step(87 * time.Minute)     //
		markUnhealthy(aNode, "HighPriority") // A's high-priority condition is fresh (just past toleration)
		env.Clock.Step(33 * time.Minute)     // now: A.low age=5τ (score 5), A.high age≈0.1τ (score 1.1), B.high age=3τ (score 4)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		// argmax(A)=5 (its starving low-priority condition) > score(B)=4; scoring A off its fresh high-priority
		// condition (1.1) would have picked B.
		Expect(cmds[0].Candidates[0].Node.Name).To(Equal(aNode.Name))
	})

	// INV-S10: after any required replacements are ready, the drain deadline is stamped immediately before the delete
	// request (not at command-computation time), so replacement-launch latency can't erode the window. A forceful (0)
	// policy stamps an immediate deadline, so repair is never the unbounded hang.
	It("should stamp a forceful (immediate) termination deadline before requesting deletion", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(time.Duration(0)), Action: cloudprovider.ReplaceNode},
		}
		newRepairController()
		nodeClaim.Finalizers = append(nodeClaim.Finalizers, "karpenter.sh/test-finalizer") // survive Delete so we can read the stamp
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		// Not stamped at command-computation time...
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).ToNot(HaveKey(v1.NodeClaimTerminationTimestampAnnotationKey))
		// ...stamped immediately before the queue requests candidate deletion.
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).To(
			HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Format(time.RFC3339)))
	})

	It("should retry a failed termination-deadline patch without losing the deadline", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(time.Duration(0)), Action: cloudprovider.ReplaceNode},
		}
		injectedErr := errors.New("injected termination timestamp patch failure")
		failingClient := &terminationTimestampPatchErrorClient{Client: env.Client, err: injectedErr, failNext: true}
		failingQueue := disruption.NewQueue(failingClient, recorder, cluster, env.Clock, prov)
		failingRepair := disruption.NewRepair(disruption.MakeConsolidation(env.Clock, cluster, failingClient, prov, cloudProvider, recorder, failingQueue))
		failingController := disruption.NewController(ctx, env.Clock, failingClient, prov, cloudProvider, recorder, cluster, failingQueue, clusterCost,
			disruption.WithMethods(failingRepair))

		nodeClaim.Finalizers = append(nodeClaim.Finalizers, "karpenter.sh/test-finalizer")
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, failingController)
		cmds := failingQueue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])

		result := ExpectObjectReconciled(ctx, failingClient, failingQueue, cmds[0].Candidates[0].NodeClaim)
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(failingClient.failNext).To(BeFalse())
		Expect(cmds[0].Candidates[0].NodeClaim.Annotations).ToNot(HaveKey(v1.NodeClaimTerminationTimestampAnnotationKey))
		current := ExpectExists(ctx, env.Client, nodeClaim)
		Expect(current.Annotations).ToNot(HaveKey(v1.NodeClaimTerminationTimestampAnnotationKey))
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue())

		ExpectObjectReconciled(ctx, failingClient, failingQueue, cmds[0].Candidates[0].NodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).To(
			HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Format(time.RFC3339)))
	})

	// INV-S10: when both the policy and the NodeClaim bound the drain, the smaller (most forceful) wins.
	It("should stamp min(policy TGP, NodeClaim TGP) before requesting deletion", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(20 * time.Minute), Action: cloudprovider.ReplaceNode},
		}
		newRepairController()
		nodeClaim.Spec.TerminationGracePeriod = &metav1.Duration{Duration: 5 * time.Minute}
		nodeClaim.Finalizers = append(nodeClaim.Finalizers, "karpenter.sh/test-finalizer") // survive Delete so we can read the stamp
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		// min(20m, 5m) -> 5m, stamped immediately before the delete request.
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).To(
			HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Add(5*time.Minute).Format(time.RFC3339)))
	})

	It("should act on and meter the highest-priority eligible condition", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10, TerminationGracePeriod: lo.ToPtr(15 * time.Minute), Action: cloudprovider.ReplaceNode},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 90, TerminationGracePeriod: lo.ToPtr(5 * time.Minute), Action: cloudprovider.ReplaceNode},
		}
		newRepairController()
		nodeClaim.Status.ImageID = "ami-test-1234"
		nodeClaim.Finalizers = append(nodeClaim.Finalizers, "karpenter.sh/test-finalizer")
		initNode(nodeClaim, node)
		markUnhealthy(node, "LowPriority")
		markUnhealthy(node, "HighPriority")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).To(
			HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Add(5*time.Minute).Format(time.RFC3339)))
		ExpectMetricCounterValue(disruption.NodeClaimsUnhealthyDisruptedTotal, 1, map[string]string{
			disruption.RepairCondition.Name: "high_priority",
			metrics.NodePoolLabel:           nodePool.Name,
			metrics.CapacityTypeLabel:       v1.CapacityTypeOnDemand,
			disruption.ImageID.Name:         "ami-test-1234",
			metrics.TerminationModeLabel:    metrics.TerminationModeEventual,
		})
	})

	// The deadline is stamped only after required replacement readiness, so a replacement that never becomes healthy
	// leaves the original both un-terminated AND un-stamped (the bounded policy proves it would stamp if termination ran).
	It("should not stamp a termination deadline when the replacement never becomes healthy", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(10 * time.Minute), Action: cloudprovider.ReplaceNode},
		}
		newRepairController()
		initNode(nodeClaim, node)
		bindReschedulablePod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		// Do NOT make the replacement ready; reconciling the queue must neither terminate nor stamp the original.
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		nc := ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nc.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(nc.Annotations).ToNot(HaveKey(v1.NodeClaimTerminationTimestampAnnotationKey))
	})

	It("should not bypass a blocking pod without a bounded drain", func() {
		initNode(nodeClaim, node)
		bindBlockingPod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
		Expect(recorder.DetectedEvent("repair requires a termination grace period to bypass blocking pods")).To(BeTrue())
	})

	It("should size replacement capacity for a blocking pod when the drain is bounded", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{
				ConditionType:          "BadNode",
				ConditionStatus:        corev1.ConditionFalse,
				TolerationDuration:     30 * time.Minute,
				TerminationGracePeriod: lo.ToPtr(5 * time.Minute),
				Action:                 cloudprovider.ReplaceNode,
			},
		}
		newRepairController()
		initNode(nodeClaim, node)
		bindBlockingPod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	It("should repair a static NodePool with a one-for-one replacement from the same pool", func() {
		nodePool = test.StaticNodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Replicas: lo.ToPtr[int64](5),
				Disruption: v1.Disruption{
					Budgets: []v1.Budget{{Nodes: "100%"}},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		nodeClaims, nodes := test.NodeClaimsAndNodes(5, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		markUnhealthy(nodes[0], "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Candidates[0].NodePool.Name).To(Equal(nodePool.Name))
		Expect(cmds[0].Replacements).To(HaveLen(1))
		Expect(cmds[0].Replacements[0].NodePoolName).To(Equal(nodePool.Name))
		Expect(cmds[0].Replacements[0].IsStaticNodeClaim).To(BeTrue())
	})

	It("should honor a static NodePool node limit before reserving replacement capacity", func() {
		nodePool = test.StaticNodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Replicas: lo.ToPtr[int64](5),
				Limits:   v1.Limits{resources.Node: resource.MustParse("5")},
				Disruption: v1.Disruption{
					Budgets: []v1.Budget{{Nodes: "100%"}},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		nodeClaims, nodes := test.NodeClaimsAndNodes(5, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		markUnhealthy(nodes[0], "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(BeEmpty())
	})

	It("should release a static replacement reservation when command admission loses a race", func() {
		nodePool = test.StaticNodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Replicas: lo.ToPtr[int64](5),
				Limits:   v1.Limits{resources.Node: resource.MustParse("6")},
				Disruption: v1.Disruption{
					Budgets: []v1.Budget{{Nodes: "100%"}},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		nodeClaims, nodes := test.NodeClaimsAndNodes(5, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		markUnhealthy(nodes[0], "BadNode")
		env.Clock.Step(31 * time.Minute)
		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, repair.ShouldDisrupt, disruption.RepairDisruptionClass, queue)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))
		commands, err := repair.ComputeCommands(ctx, map[string]int{nodePool.Name: 1}, candidates...)
		Expect(err).NotTo(HaveOccurred())
		Expect(commands).To(HaveLen(1))

		queue.Lock()
		queue.ProviderIDToCommand[candidates[0].ProviderID()] = &commands[0]
		queue.Unlock()
		Expect(queue.StartCommand(ctx, &commands[0])).To(MatchError("candidate is being disrupted"))
		queue.Lock()
		delete(queue.ProviderIDToCommand, candidates[0].ProviderID())
		queue.Unlock()

		Expect(cluster.NodePoolState.ReserveNodeCount(nodePool.Name, 6, 1)).To(Equal(int64(1)))
		cluster.NodePoolState.ReleaseNodeCount(nodePool.Name, 1)
	})

	It("should reject a stale candidate that recovered before command admission", func() {
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)
		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, repair.ShouldDisrupt, disruption.RepairDisruptionClass, queue)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))

		current := ExpectExists(ctx, env.Client, node)
		current.Status.Conditions = lo.Reject(current.Status.Conditions, func(condition corev1.NodeCondition, _ int) bool {
			return condition.Type == "BadNode"
		})
		ExpectApplied(ctx, env.Client, current)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(current))

		commands, err := repair.ComputeCommands(ctx, map[string]int{nodePool.Name: 1}, candidates...)
		Expect(err).NotTo(HaveOccurred())
		Expect(commands).To(BeEmpty())
	})

	It("should reject a stale candidate when a PDB becomes blocking before command admission", func() {
		initNode(nodeClaim, node)
		bindReschedulablePod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)
		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, repair.ShouldDisrupt, disruption.RepairDisruptionClass, queue)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))

		budget := test.PodDisruptionBudget(test.PDBOptions{
			Labels:         map[string]string{"repair-test": "true"},
			MaxUnavailable: fromInt(0),
			Status: &policyv1.PodDisruptionBudgetStatus{
				ObservedGeneration: 1,
				DisruptionsAllowed: 0,
				CurrentHealthy:     1,
				DesiredHealthy:     1,
				ExpectedPods:       1,
			},
		})
		ExpectApplied(ctx, env.Client, budget)

		commands, err := repair.ComputeCommands(ctx, map[string]int{nodePool.Name: 1}, candidates...)
		Expect(err).NotTo(HaveOccurred())
		Expect(commands).To(BeEmpty())
	})

	It("should reject stale scheduling results when a candidate pod changes before command admission", func() {
		initNode(nodeClaim, node)
		bindReschedulablePod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)
		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, repair.ShouldDisrupt, disruption.RepairDisruptionClass, queue)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))

		// This pod was not part of the original scheduling result and must force a fresh pass.
		bindReschedulablePod(node)

		commands, err := repair.ComputeCommands(ctx, map[string]int{nodePool.Name: 1}, candidates...)
		Expect(err).NotTo(HaveOccurred())
		Expect(commands).To(BeEmpty())
	})

	It("should propagate errors from command admission", func() {
		initNode(nodeClaim, node)
		bindReschedulablePod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)
		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, repair.ShouldDisrupt, disruption.RepairDisruptionClass, queue)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))

		injectedErr := errors.New("injected NodePool read failure")
		failingRepair := disruption.NewRepair(disruption.MakeConsolidation(
			env.Clock,
			cluster,
			&nodePoolGetErrorClient{Client: env.Client, err: injectedErr},
			prov,
			cloudProvider,
			recorder,
			queue,
		))

		commands, err := failingRepair.ComputeCommands(ctx, map[string]int{nodePool.Name: 1}, candidates...)
		Expect(commands).To(BeEmpty())
		Expect(err).To(MatchError(ContainSubstring(injectedErr.Error())))
	})

	It("should propagate pod list errors from command admission revalidation", func() {
		nodePool = test.StaticNodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Replicas: lo.ToPtr[int64](1),
				Disruption: v1.Disruption{
					Budgets: []v1.Budget{{Nodes: "100%"}},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)
		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, repair.ShouldDisrupt, disruption.RepairDisruptionClass, queue)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))

		injectedErr := errors.New("injected pod list failure")
		failingRepair := disruption.NewRepair(disruption.MakeConsolidation(
			env.Clock,
			cluster,
			&podListErrorClient{Client: env.Client, err: injectedErr},
			prov,
			cloudProvider,
			recorder,
			queue,
		))

		commands, err := failingRepair.ComputeCommands(ctx, map[string]int{nodePool.Name: 1}, candidates...)
		Expect(commands).To(BeEmpty())
		Expect(err).To(MatchError(ContainSubstring(injectedErr.Error())))
	})

	It("should explicitly exclude standalone NodeClaims that have no safe replacement template", func() {
		standaloneClaim, standaloneNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
			corev1.LabelTopologyZone: "test-zone-1a",
		}}})
		initNode(standaloneClaim, standaloneNode)
		markUnhealthy(standaloneNode, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(BeEmpty())
		Expect(recorder.DetectedEvent("repair requires a NodePool to construct and budget a safe replacement")).To(BeTrue())
	})

	// A node whose unhealthy condition does not match any RepairPolicy is left alone.
	It("should not repair a node whose condition does not match any policy", func() {
		initNode(nodeClaim, node)
		markUnhealthy(node, "SomeOtherCondition")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	// Feature-gate off: repair does nothing even for an unhealthy node past toleration.
	It("should not repair when the NodeRepair feature gate is disabled", func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(false)}}))
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
	})

})
