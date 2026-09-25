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

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// These tests exercise end-user behavior of voluntary node repair (Node Repair Resiliency design). Each It maps to an
// invariant (INV-S*) from the design. The disruption controller is built with only the Repair method so the behavior
// under test is isolated from consolidation/drift.

var _ = Describe("Repair", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	var repairController *disruption.Controller

	labels := func() map[string]string {
		return map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}
	}

	// initNode applies + initializes a node/nodeclaim and syncs cluster state. Initialization overwrites
	// Status.Conditions with Ready=True, so unhealthy conditions must be stamped AFTER this via markUnhealthy.
	initNode := func(nc *v1.NodeClaim, n *corev1.Node) {
		ExpectApplied(ctx, env.Client, nc, n)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{n}, []*v1.NodeClaim{nc})
	}

	// bindReschedulablePod places a ReplicaSet-owned (reschedulable) pod on the node so pre-spin has a workload to
	// protect — the scheduling simulation then produces a replacement NodeClaim (a replace-then-terminate command).
	// An empty node correctly yields a delete-only command instead, so pre-spin tests must have a pod.
	bindReschedulablePod := func(n *corev1.Node) {
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
		}}}})
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
	markUnhealthy := func(n *corev1.Node, condType corev1.NodeConditionType) {
		n = ExpectExists(ctx, env.Client, n)
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
			Type:               condType,
			Status:             corev1.ConditionFalse,
			LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
		})
		ExpectApplied(ctx, env.Client, n)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
	}

	// newRepairController builds an isolated repair-only disruption controller. Repair caches RepairPolicies() at
	// construction, so specs that override cloudProvider.RepairPolicy must call this again to pick up the new policies.
	newRepairController := func() {
		repairController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
			disruption.WithMethods(disruption.NewRepair(disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue))))
	}

	BeforeEach(func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true)}}))
		// Single default policy: BadNode/False, 30m toleration (the fake cloud provider default).
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute},
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

		ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)
		ExpectNotFound(ctx, env.Client, nodeClaim)
	})

	// INV-S9: repair never fires before the policy toleration elapses.
	It("should not repair before the toleration duration elapses", func() {
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(10 * time.Minute) // still within the 30m toleration

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	// INV-S7: a replacement that boots unhealthy (bad AMI / partitioned zone) never leads to terminating the original —
	// pre-spin is the circuit breaker for the bad-component loop.
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

	// INV-S1 / INV-S5: repair rides the shared disruption budget. A 0-node budget blocks repair entirely — this is the
	// spec that actually pins the budget gate (asserting HaveLen(1) with a non-zero budget is tautological because
	// ComputeCommands returns at most one command per pass regardless of budget).
	It("should not repair when the NodePool disruption budget is zero", func() {
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0"}}
		ExpectApplied(ctx, env.Client, nodePool)
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	// The circuit breaker halts repair for a NodePool when more than 20% of its nodes are unhealthy — a correlated
	// failure (bad AMI, AZ outage) shouldn't be amplified by mass-replacing nodes into the same fault.
	It("should stop repairing a NodePool when more than the breaker threshold of nodes are unhealthy", func() {
		const count = 10
		nodeClaims, nodes := test.NodeClaimsAndNodes(count, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		// 3 of 10 unhealthy (30%) exceeds the 20% breaker -> repair is halted for the pool.
		markUnhealthy(nodes[0], "BadNode")
		markUnhealthy(nodes[1], "BadNode")
		markUnhealthy(nodes[2], "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	// The breaker threshold rounds UP: a 3-node pool tolerates 1 unhealthy node (ceil(0.2*3)=1), so repair proceeds.
	// A floor would give 0 and wrongly trip on the first unhealthy node.
	It("should not trip the breaker for one unhealthy node in a small pool (round-up)", func() {
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
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 90},
		}
		newRepairController()
		// 8 healthy nodes so the 2 unhealthy below stay at/under the 20% circuit breaker (2 of 10).
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

	// INV-S4: a node's score is the argmax of rank+age/τ over ALL its matching conditions, not the score of its
	// highest-priority condition. Node A has a fresh high-priority condition (just past toleration) and a long-starving
	// low-priority one; node B has a moderately-aged high-priority condition. Scoring A off only its high-priority
	// (fresh) condition would rank it below B and repair B first; the argmax lifts A above B on its starving condition.
	It("should order a node by its most urgent condition, not just its highest-priority one", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 90},
		}
		newRepairController()
		// 8 healthy nodes keep the 2 unhealthy below the 20% breaker.
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

	// Eligibility: matchRepairPolicy considers only ELIGIBLE (past-toleration) conditions, so a node with a fresh
	// (not-yet-eligible) highest-priority condition still repairs on an overdue lower-priority one — and the ACTION is
	// governed by that eligible condition. Here LowPriority (eligible, 15m TGP) governs, not HighPriority (fresh, 5m).
	It("should repair and act on the eligible lower-priority condition when the higher-priority one is not yet eligible", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10, TerminationGracePeriod: lo.ToPtr(15 * time.Minute)},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 90, TerminationGracePeriod: lo.ToPtr(5 * time.Minute)},
		}
		newRepairController()
		nodeClaim.Finalizers = append(nodeClaim.Finalizers, "karpenter.sh/test-finalizer")
		initNode(nodeClaim, node)
		markUnhealthy(node, "LowPriority")  // eligible after the step
		env.Clock.Step(31 * time.Minute)    //
		markUnhealthy(node, "HighPriority") // fresh: still inside its 30m toleration at reconcile

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		// The eligible LowPriority governs the drain bound (15m), not the ineligible HighPriority (5m).
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).To(
			HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Add(15*time.Minute).Format(time.RFC3339)))
	})

	// Pure inter-node aging/τ overtake (one condition per node, differing rank): node A (low rank, aged 5τ) outscores
	// node B (high rank, aged 0.5τ) because 0+5 > 1+0.5. Dropping the age/τ term flips it (0 < 1) and B would win.
	It("should let a long-starving lower-priority node overtake a fresher higher-priority node", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 90},
		}
		newRepairController()
		// 8 healthy nodes keep the 2 unhealthy at/under the 20% breaker.
		healthyClaims, healthyNodes := test.NodeClaimsAndNodes(8, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range healthyNodes {
			initNode(healthyClaims[i], healthyNodes[i])
		}
		aClaim, aNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		bClaim, bNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		initNode(aClaim, aNode)
		initNode(bClaim, bNode)
		markUnhealthy(aNode, "LowPriority")  // aged 5τ below
		env.Clock.Step(135 * time.Minute)    //
		markUnhealthy(bNode, "HighPriority") // aged 0.5τ below
		env.Clock.Step(45 * time.Minute)     // now: A.low age=150m=5τ (score 5), B.high age=15m=0.5τ (score 1.5)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Candidates[0].Node.Name).To(Equal(aNode.Name))
	})

	// INV-S10: the drain deadline is stamped at actual deletion time (not command-computation time), so pre-spin latency
	// can't erode the window — mirroring how the lifecycle controller stamps DeletionTimestamp+TGP for other reasons.
	// A forceful (0) policy stamps an immediate deadline, so repair is never the unbounded hang.
	It("should stamp a forceful (immediate) termination deadline at deletion for a forceful policy", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(time.Duration(0))},
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
		// ...stamped when the queue terminates the candidate, anchored to that moment (forceful 0 -> now).
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).To(
			HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Format(time.RFC3339)))
	})

	// INV-S10: when both the policy and the NodeClaim bound the drain, the smaller (most forceful) wins.
	It("should stamp min(policy TGP, NodeClaim TGP) at deletion", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(20 * time.Minute)},
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
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		// min(20m, 5m) -> 5m, stamped from the deletion moment.
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).To(
			HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Add(5*time.Minute).Format(time.RFC3339)))
	})

	// The repair ACTION (drain bound + per-condition metric) uses the highest-priority ELIGIBLE condition — distinct
	// from how score orders nodes. Two eligible conditions on one node: HighPriority (5m TGP) governs over LowPriority
	// (15m), and the unhealthy-disrupted metric is emitted once at termination, labeled by condition/nodepool/ct/image.
	It("should act on and meter the highest-priority eligible condition", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10, TerminationGracePeriod: lo.ToPtr(15 * time.Minute)},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 90, TerminationGracePeriod: lo.ToPtr(5 * time.Minute)},
		}
		newRepairController()
		nodeClaim.Status.ImageID = "ami-test-1234"
		nodeClaim.Finalizers = append(nodeClaim.Finalizers, "karpenter.sh/test-finalizer")
		initNode(nodeClaim, node)
		markUnhealthy(node, "LowPriority")
		markUnhealthy(node, "HighPriority")
		env.Clock.Step(31 * time.Minute) // both eligible

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		// HighPriority governs the action: its 5m TGP is stamped (not LowPriority's 15m)...
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).To(
			HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Add(5*time.Minute).Format(time.RFC3339)))
		// ...and the metric is emitted once at termination, labeled by the HighPriority condition, the node's image, and
		// the termination mode derived from the applied bound (5m > 0 -> eventual; NOT the NodeClaim's nil Spec.TGP).
		ExpectMetricCounterValue(disruption.NodeClaimsUnhealthyDisruptedTotal, 1, map[string]string{
			disruption.RepairCondition.Name: "high_priority",
			metrics.NodePoolLabel:           nodePool.Name,
			metrics.CapacityTypeLabel:       v1.CapacityTypeOnDemand,
			disruption.ImageID.Name:         "ami-test-1234",
			metrics.TerminationModeLabel:    metrics.TerminationModeEventual,
		})
	})

	// The unhealthy-disrupted metric is emitted at ACTUAL termination, not at command production — an abandoned command
	// (replacement never becomes healthy) must not increment it, or repeated attempts would over-count the same node.
	It("should not meter an abandoned repair (replacement never healthy)", func() {
		initNode(nodeClaim, node)
		bindReschedulablePod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		// Replacement is never made healthy, so the queue reconcile does not terminate — the metric must stay unrecorded.
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		name := ExpectMetricName(disruption.NodeClaimsUnhealthyDisruptedTotal.(*opmetrics.PrometheusCounter))
		_, ok := FindMetricWithLabelValues(name, map[string]string{
			disruption.RepairCondition.Name: "bad_node",
			metrics.NodePoolLabel:           nodePool.Name,
			metrics.CapacityTypeLabel:       v1.CapacityTypeOnDemand,
			disruption.ImageID.Name:         "",
		})
		Expect(ok).To(BeFalse())
	})

	// The deadline is stamped only at actual deletion, so a replacement that never becomes healthy leaves the original
	// both un-terminated AND un-stamped — the circuit breaker never starts the drain clock (bounded policy proves it
	// would stamp if termination ran).
	It("should not stamp a termination deadline when the replacement never becomes healthy", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(10 * time.Minute)},
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

	// INV-S8: repair is not discretionary — like it ignores node-level do-not-disrupt, a broken node is never stranded
	// by a pod that blocks eviction (PDB / pod do-not-disrupt). It repairs regardless of whether a drain bound is set;
	// the bound only governs HOW the drain proceeds (asserted by the stamp tests below), not WHETHER repair happens.
	It("should repair an unhealthy node even when a blocking pod would otherwise prevent disruption", func() {
		initNode(nodeClaim, node)
		bindBlockingPod(node) // do-not-disrupt pod: blocks eviction, would strand a discretionary disruption
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	// A node whose unhealthy condition does not match any RepairPolicy is left alone.
	It("should not repair a node whose condition does not match any policy", func() {
		initNode(nodeClaim, node)
		markUnhealthy(node, "SomeOtherCondition")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	// A condition of the matching TYPE but the wrong STATUS does not match (policy wants BadNode=False; node has
	// BadNode=True) — the node is left alone.
	It("should not repair a node whose matching condition has the wrong status", func() {
		initNode(nodeClaim, node)
		n := ExpectExists(ctx, env.Client, node)
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
			Type: "BadNode", Status: corev1.ConditionTrue, LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
		})
		ExpectApplied(ctx, env.Client, n)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
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
