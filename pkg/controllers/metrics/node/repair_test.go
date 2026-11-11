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

package node_test

import (
	"context"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/metrics/node"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Repair Past Due", func() {
	var repairCtx context.Context
	var repairNodeController *informer.NodeController
	var repairNodeClaimController *informer.NodeClaimController
	var repairMetricsController *node.Controller

	voluntaryRepair := test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: new(true)}}
	legacyRepair := test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: new(true)}, LegacyNodeRepair: new(true)}

	// badNode is the default policy: a node is late after its 30m toleration, the 15m registration timeout and its 10m
	// drain bound.
	badNode := cloudprovider.RepairPolicy{
		ConditionType:          "BadNode",
		ConditionStatus:        corev1.ConditionFalse,
		TolerationDuration:     30 * time.Minute,
		TerminationGracePeriod: new(10 * time.Minute),
		Action:                 cloudprovider.ReplaceNode,
	}
	// fatalDisk only matches the Fatal reason: a node is late after its 10m toleration, the 15m registration timeout and
	// its 5m drain bound.
	fatalDisk := cloudprovider.RepairPolicy{
		ConditionType:          "BadDisk",
		ConditionStatus:        corev1.ConditionFalse,
		ReasonRegex:            "^Fatal$",
		TolerationDuration:     10 * time.Minute,
		TerminationGracePeriod: new(5 * time.Minute),
		Action:                 cloudprovider.ReplaceNode,
	}
	badNodeCondition := corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionFalse}
	fatalDiskCondition := corev1.NodeCondition{Type: "BadDisk", Status: corev1.ConditionFalse, Reason: "Fatal"}

	// startRepair builds cluster state and the metrics controller from the options and policies, as the operator does
	// once at startup.
	startRepair := func(fields test.OptionsFields, policies ...cloudprovider.RepairPolicy) {
		cloudProvider.RepairPolicy = policies
		repairCtx = options.ToContext(ctx, test.Options(fields))
		matcher := lo.Must(health.NewRepairPolicyMatcher(repairCtx, cloudProvider))
		repairCluster := state.NewCluster(env.Clock, env.Client, cloudProvider, state.WithRepairPolicyMatcher(matcher))
		repairNodeController = informer.NewNodeController(env.Client, repairCluster)
		repairNodeClaimController = informer.NewNodeClaimController(env.Client, cloudProvider, repairCluster, cost.NewClusterCost(repairCtx, cloudProvider, env.Client))
		repairMetricsController = node.NewController(repairCtx, env.Clock, cloudProvider, repairCluster)
	}
	nodePoolNodeClaim := func() *v1.NodeClaim {
		return test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: "default"}}})
	}
	// applyCondition sets the node's condition as of now and updates cluster state.
	applyCondition := func(n *corev1.Node, condition corev1.NodeCondition) *corev1.Node {
		n = ExpectExists(repairCtx, env.Client, n)
		// Repair counts a condition from no earlier than the node's creation, so keep the clock from starting before it.
		if env.Clock.Now().Before(n.CreationTimestamp.Time) {
			env.Clock.SetTime(n.CreationTimestamp.Time)
		}
		condition.LastTransitionTime = metav1.NewTime(env.Clock.Now())
		n.Status.Conditions = append(lo.Reject(n.Status.Conditions, func(c corev1.NodeCondition, _ int) bool {
			return c.Type == condition.Type
		}), condition)
		ExpectApplied(repairCtx, env.Client, n)
		ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
		return n
	}
	// unhealthyNode creates an initialized node for the NodeClaim, with the condition starting now.
	unhealthyNode := func(nodeClaim *v1.NodeClaim, condition corev1.NodeCondition) *corev1.Node {
		n := test.NodeClaimLinkedNode(nodeClaim)
		ExpectApplied(repairCtx, env.Client, nodeClaim, n)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(repairCtx, env.Client, env.Clock, repairNodeController, repairNodeClaimController, []*corev1.Node{n}, []*v1.NodeClaim{nodeClaim})
		return applyCondition(n, condition)
	}
	// unhealthyUnmanagedNode creates an initialized node with no NodeClaim, with the condition starting now. Cluster state
	// only tracks a NodePool node like this once it has a provider ID and is initialized.
	unhealthyUnmanagedNode := func(labels map[string]string, condition corev1.NodeCondition) *corev1.Node {
		n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Labels: labels}, ProviderID: test.RandomProviderID()})
		ExpectApplied(repairCtx, env.Client, n)
		ExpectMakeNodesInitialized(repairCtx, env.Client, env.Clock, n)
		return applyCondition(n, condition)
	}
	// doNotRepair sets do-not-repair on the node only, which is where repair reads it once the node registers.
	doNotRepair := func(n *corev1.Node) {
		n = ExpectExists(repairCtx, env.Client, n)
		n.Annotations = lo.Assign(n.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
		ExpectApplied(repairCtx, env.Client, n)
		ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
	}
	// setRebooting starts or finishes the NodeClaim's reboot as of now and updates cluster state.
	setRebooting := func(nodeClaim *v1.NodeClaim, running bool) {
		nodeClaim = ExpectExists(repairCtx, env.Client, nodeClaim)
		conditions := nodeClaim.StatusConditions(status.WithClock(env.Clock))
		if running {
			conditions.SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonRequested, "rebooting")
		} else {
			conditions.SetFalse(v1.ConditionTypeRebooting, v1.RebootReasonSucceeded, "rebooted")
		}
		ExpectApplied(repairCtx, env.Client, nodeClaim)
		ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
	}
	expectLateCounts := func(counts map[string]int) {
		GinkgoHelper()
		ExpectSingletonReconciled(repairCtx, repairMetricsController)
		for condition, count := range counts {
			ExpectMetricGaugeValue(node.RepairPastDue, float64(count), map[string]string{"condition": condition})
		}
	}
	// expectLateAfter checks that one node counts under the condition once late has passed, but not a minute before.
	expectLateAfter := func(late time.Duration, condition string) {
		GinkgoHelper()
		env.Clock.Step(late - time.Minute)
		expectLateCounts(map[string]int{condition: 0})
		env.Clock.Step(2 * time.Minute)
		expectLateCounts(map[string]int{condition: 1})
	}
	expectNoSeries := func() {
		GinkgoHelper()
		ExpectSingletonReconciled(repairCtx, repairMetricsController)
		_, found := FindMetricWithLabelValues(ExpectMetricName(node.RepairPastDue.(*opmetrics.PrometheusGauge)), map[string]string{})
		Expect(found).To(BeFalse())
	}

	BeforeEach(func() {
		// Cluster state only syncs once it has seen every NodeClaim and Node, so start from an empty cluster.
		ExpectCleanedUp(ctx, env.Client)
		env.Clock.SetTime(time.Now())
	})
	AfterEach(func() {
		cloudProvider.RepairPolicy = nil
		ExpectCleanedUp(ctx, env.Client)
		// Each spec builds its own controller, so drop the series it left in the shared gauges.
		for _, gauge := range []opmetrics.GaugeMetric{node.Allocatable, node.TotalPodRequests, node.TotalPodLimits, node.TotalDaemonRequests,
			node.TotalDaemonLimits, node.SystemOverhead, node.Lifetime, node.ClusterUtilization, node.RepairPastDue} {
			gauge.Reset()
		}
	})

	Context("Repair", func() {
		BeforeEach(func() {
			startRepair(voluntaryRepair, badNode, fatalDisk)
		})
		It("should report 0 for each repair condition while no node is late", func() {
			Expect(ExpectMetricName(node.RepairPastDue.(*opmetrics.PrometheusGauge))).To(Equal("karpenter_nodes_repair_past_due"))
			expectLateCounts(map[string]int{"bad_node": 0, "bad_disk": 0})
		})
		It("should not report the metric until cluster state syncs", func() {
			// Cluster state hasn't seen this NodeClaim yet, so it may be missing nodes.
			nodeClaim := nodePoolNodeClaim()
			ExpectApplied(repairCtx, env.Client, nodeClaim)
			expectNoSeries()

			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			expectLateCounts(map[string]int{"bad_node": 0, "bad_disk": 0})
		})
		DescribeTable("should count a node once its condition outlasts the toleration, the registration timeout and the drain bound",
			func(policyBound, nodeClaimBound *time.Duration, drainBound time.Duration) {
				policy := badNode
				policy.TerminationGracePeriod = policyBound
				startRepair(voluntaryRepair, policy)
				nodeClaim := nodePoolNodeClaim()
				if nodeClaimBound != nil {
					nodeClaim.Spec.TerminationGracePeriod = &metav1.Duration{Duration: *nodeClaimBound}
				}
				unhealthyNode(nodeClaim, badNodeCondition)

				expectLateAfter(badNode.TolerationDuration+lifecycle.RegistrationTimeout+drainBound, "bad_node")
			},
			Entry("using the policy's drain bound", new(10*time.Minute), nil, 10*time.Minute),
			Entry("using the policy's drain bound when the NodeClaim's is longer", new(5*time.Minute), new(time.Hour), 5*time.Minute),
			Entry("using a shorter NodeClaim termination grace period", new(10*time.Minute), new(time.Minute), time.Minute),
			Entry("using the NodeClaim termination grace period when the policy sets none", nil, new(5*time.Minute), 5*time.Minute),
			Entry("using no drain bound when nothing sets one", nil, nil, time.Duration(0)),
		)
		It("should use each condition's own policy, matched by reason", func() {
			unhealthyNode(nodePoolNodeClaim(), fatalDiskCondition)
			// A reason the specific policy doesn't match falls back to the default policy.
			unhealthyNode(nodePoolNodeClaim(), corev1.NodeCondition{Type: "BadDisk", Status: corev1.ConditionFalse, Reason: "Worn"})
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)

			env.Clock.Step(29 * time.Minute)
			expectLateCounts(map[string]int{"bad_disk": 0, "bad_node": 0})
			env.Clock.Step(2 * time.Minute)
			expectLateCounts(map[string]int{"bad_disk": 1, "bad_node": 0})
			env.Clock.Step(25 * time.Minute)
			expectLateCounts(map[string]int{"bad_disk": 2, "bad_node": 1})
		})
		It("should count a node once, under the condition whose repair is late", func() {
			startRepair(voluntaryRepair, badNode, cloudprovider.RepairPolicy{
				ConditionType:          "BadDisk",
				ConditionStatus:        corev1.ConditionFalse,
				ReasonRegex:            "^Transient$",
				TolerationDuration:     10 * time.Minute,
				TerminationGracePeriod: new(5 * time.Minute),
				Action:                 cloudprovider.RebootNode,
			})
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			applyCondition(n, corev1.NodeCondition{Type: "BadDisk", Status: corev1.ConditionFalse, Reason: "Transient"})

			// Repair now picks the replace for BadNode, but only the reboot for BadDisk is late.
			env.Clock.Step(31 * time.Minute)
			expectLateCounts(map[string]int{"bad_disk": 1, "bad_node": 0})
			// Both are late now, so the node counts once, under the replace that repair picks.
			env.Clock.Step(25 * time.Minute)
			expectLateCounts(map[string]int{"bad_disk": 0, "bad_node": 1})
		})
		It("should not count a node while it reboots, and should restart the toleration once the reboot ends", func() {
			nodeClaim := nodePoolNodeClaim()
			unhealthyNode(nodeClaim, badNodeCondition)
			env.Clock.Step(40 * time.Minute)
			setRebooting(nodeClaim, true)

			// The reboot runs past when the node would otherwise be late, but the reboot controller bounds it.
			env.Clock.Step(time.Hour)
			expectLateCounts(map[string]int{"bad_node": 0})

			// Once the reboot ends, the node is late 55m later, not 55m after the condition started.
			setRebooting(nodeClaim, false)
			expectLateAfter(55*time.Minute, "bad_node")
		})
		It("should ignore a node with do-not-repair", func() {
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			doNotRepair(unhealthyNode(nodePoolNodeClaim(), badNodeCondition))

			env.Clock.Step(2 * time.Hour)
			expectLateCounts(map[string]int{"bad_node": 1})
		})
		It("should ignore a node without a NodePool", func() {
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			unhealthyNode(test.NodeClaim(), badNodeCondition)

			env.Clock.Step(2 * time.Hour)
			expectLateCounts(map[string]int{"bad_node": 1})
		})
		It("should ignore a node without a NodeClaim", func() {
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			// It has a NodePool label, so only the missing NodeClaim keeps it from counting.
			unhealthyUnmanagedNode(map[string]string{v1.NodePoolLabelKey: "default"}, badNodeCondition)

			env.Clock.Step(2 * time.Hour)
			expectLateCounts(map[string]int{"bad_node": 1})
		})
		It("should count a deleting node until it is gone", func() {
			nodeClaim := nodePoolNodeClaim()
			n := unhealthyNode(nodeClaim, badNodeCondition)
			env.Clock.Step(56 * time.Minute)
			ExpectDeletionTimestampSet(repairCtx, env.Client, nodeClaim, n)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			expectLateCounts(map[string]int{"bad_node": 1})

			ExpectFinalizersRemoved(repairCtx, env.Client, nodeClaim, n)
			ExpectNotFound(repairCtx, env.Client, nodeClaim, n)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			expectLateCounts(map[string]int{"bad_node": 0})
		})
		It("should stop counting a node once it recovers", func() {
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			env.Clock.Step(56 * time.Minute)
			expectLateCounts(map[string]int{"bad_node": 1})

			applyCondition(n, corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionTrue})
			expectLateCounts(map[string]int{"bad_node": 0})
		})
	})

	Context("Legacy Repair", func() {
		BeforeEach(func() {
			startRepair(legacyRepair, badNode)
		})
		It("should count a node once its condition outlasts the toleration and the registration timeout", func() {
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)

			// The legacy controller doesn't wait on the drain, so the policy's drain bound doesn't add time.
			expectLateAfter(badNode.TolerationDuration+lifecycle.RegistrationTimeout, "bad_node")
		})
		It("should count nodes with do-not-repair or without a NodePool, since the legacy controller repairs them", func() {
			doNotRepair(unhealthyNode(nodePoolNodeClaim(), badNodeCondition))
			unhealthyNode(test.NodeClaim(), badNodeCondition)

			env.Clock.Step(46 * time.Minute)
			expectLateCounts(map[string]int{"bad_node": 2})
		})
		It("should ignore a node without a NodeClaim", func() {
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			unhealthyUnmanagedNode(nil, badNodeCondition)

			env.Clock.Step(2 * time.Hour)
			expectLateCounts(map[string]int{"bad_node": 1})
		})
		It("should not count a node before the registration timeout passes, even if its condition has no transition time", func() {
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			n.Status.Conditions = lo.Map(n.Status.Conditions, func(c corev1.NodeCondition, _ int) corev1.NodeCondition {
				if c.Type == badNodeCondition.Type {
					c.LastTransitionTime = metav1.Time{}
				}
				return c
			})
			ExpectApplied(repairCtx, env.Client, n)
			ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))

			// The toleration has long passed, but the legacy controller can only act once the node exists.
			expectLateAfter(lifecycle.RegistrationTimeout, "bad_node")
		})
		It("should use the replace policy whose toleration ends first", func() {
			startRepair(legacyRepair,
				cloudprovider.RepairPolicy{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Transient$", TolerationDuration: 10 * time.Minute, Action: cloudprovider.RebootNode},
				badNode,
				cloudprovider.RepairPolicy{ConditionType: "BadDisk", ConditionStatus: corev1.ConditionFalse, TolerationDuration: time.Hour, Action: cloudprovider.ReplaceNode},
			)
			n := unhealthyNode(nodePoolNodeClaim(), corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionFalse, Reason: "Transient"})
			applyCondition(n, corev1.NodeCondition{Type: "BadDisk", Status: corev1.ConditionFalse})

			// The legacy controller can't reboot, so the node isn't late until BadNode's replace policy is.
			env.Clock.Step(44 * time.Minute)
			expectLateCounts(map[string]int{"bad_node": 0, "bad_disk": 0})
			env.Clock.Step(2 * time.Minute)
			expectLateCounts(map[string]int{"bad_node": 1, "bad_disk": 0})
			// BadDisk is late too now, but the node still counts once.
			env.Clock.Step(30 * time.Minute)
			expectLateCounts(map[string]int{"bad_node": 1, "bad_disk": 0})
		})
		It("should only report conditions that the legacy controller can repair", func() {
			startRepair(legacyRepair, badNode, cloudprovider.RepairPolicy{
				ConditionType: "BadKernel", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 10 * time.Minute, Action: cloudprovider.RebootNode,
			})
			expectLateCounts(map[string]int{"bad_node": 0})
			_, found := FindMetricWithLabelValues(ExpectMetricName(node.RepairPastDue.(*opmetrics.PrometheusGauge)), map[string]string{"condition": "bad_kernel"})
			Expect(found).To(BeFalse())
		})
	})

	Context("Node Repair Off", func() {
		It("should not emit any series", func() {
			startRepair(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: new(false)}}, badNode)
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)

			env.Clock.Step(2 * time.Hour)
			expectNoSeries()
		})
	})
})
