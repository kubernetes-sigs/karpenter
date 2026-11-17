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
	var repairCluster *state.Cluster
	var repairNodeController *informer.NodeController
	var repairNodeClaimController *informer.NodeClaimController
	var repairMetricsController *node.Controller

	nodeRepair := test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: new(true)}}

	// badNode is the default policy: a node is past due after its 30m toleration, the 15m registration timeout and its
	// 10m drain bound.
	badNode := cloudprovider.RepairPolicy{
		ConditionType:          "BadNode",
		ConditionStatus:        corev1.ConditionFalse,
		TolerationDuration:     30 * time.Minute,
		TerminationGracePeriod: new(10 * time.Minute),
		Action:                 cloudprovider.ReplaceNode,
	}
	// fatalDisk only matches the Fatal reason: a node is past due after its 10m toleration, the 15m registration timeout
	// and its 5m drain bound.
	fatalDisk := cloudprovider.RepairPolicy{
		ConditionType:          "BadDisk",
		ConditionStatus:        corev1.ConditionFalse,
		ReasonRegex:            "^Fatal$",
		TolerationDuration:     10 * time.Minute,
		TerminationGracePeriod: new(5 * time.Minute),
		Action:                 cloudprovider.ReplaceNode,
	}
	badNodeCondition := corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionFalse}
	healthyNodeCondition := corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionTrue}
	fatalDiskCondition := corev1.NodeCondition{Type: "BadDisk", Status: corev1.ConditionFalse, Reason: "Fatal"}

	// startRepair builds cluster state and the metrics controller from the options and policies, as the operator does
	// once at startup.
	startRepair := func(fields test.OptionsFields, policies ...cloudprovider.RepairPolicy) {
		cloudProvider.RepairPolicy = policies
		repairCtx = options.ToContext(ctx, test.Options(fields))
		matcher := lo.Must(health.NewRepairPolicyMatcher(repairCtx, cloudProvider))
		repairCluster = state.NewCluster(env.Clock, env.Client, cloudProvider, state.WithRepairPolicyMatcher(matcher))
		repairNodeController = informer.NewNodeController(env.Client, repairCluster)
		repairNodeClaimController = informer.NewNodeClaimController(env.Client, cloudProvider, repairCluster, cost.NewClusterCost(repairCtx, cloudProvider, env.Client))
		repairMetricsController = node.NewController(env.Clock, repairCluster)
		// The metrics controller never sets cluster state's synced flag, so set it as another controller would.
		Expect(repairCluster.Synced(repairCtx)).To(BeTrue(), "startRepair needs an empty cluster to mark cluster state synced")
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
	// setDoNotRepair sets or removes do-not-repair on the node only, which is where repair reads it once the node
	// registers.
	setDoNotRepair := func(n *corev1.Node, veto bool) {
		n = ExpectExists(repairCtx, env.Client, n)
		if veto {
			n.Annotations = lo.Assign(n.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
		} else {
			delete(n.Annotations, v1.DoNotRepairAnnotationKey)
		}
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
	expectPastDue := func(counts map[string]int) {
		GinkgoHelper()
		ExpectSingletonReconciled(repairCtx, repairMetricsController)
		for condition, count := range counts {
			ExpectMetricGaugeValue(node.RepairPastDue, float64(count), map[string]string{"condition": condition})
		}
	}
	// expectPastDueAfter checks that one node counts under the condition after wait, but not a minute before.
	expectPastDueAfter := func(wait time.Duration, condition string) {
		GinkgoHelper()
		env.Clock.Step(wait - time.Minute)
		expectPastDue(map[string]int{condition: 0})
		env.Clock.Step(2 * time.Minute)
		expectPastDue(map[string]int{condition: 1})
	}
	// expectRecorded checks how many times the histogram recorded under the condition, and the total time recorded.
	expectRecorded := func(condition string, count int, total time.Duration) {
		GinkgoHelper()
		name := ExpectMetricName(node.RepairPastDueDurationSeconds.(*opmetrics.PrometheusHistogram))
		labels := map[string]string{"condition": condition}
		if count == 0 {
			_, found := FindMetricWithLabelValues(name, labels)
			Expect(found).To(BeFalse(), "nothing should be recorded under "+condition)
			return
		}
		ExpectMetricHistogramSampleCountValue(name, uint64(count), labels)
		metric, _ := FindMetricWithLabelValues(name, labels)
		Expect(metric.GetHistogram().GetSampleSum()).To(BeNumerically("~", total.Seconds()))
	}
	expectNoSeries := func() {
		GinkgoHelper()
		ExpectSingletonReconciled(repairCtx, repairMetricsController)
		for _, name := range []string{
			ExpectMetricName(node.RepairPastDue.(*opmetrics.PrometheusGauge)),
			ExpectMetricName(node.RepairPastDueDurationSeconds.(*opmetrics.PrometheusHistogram)),
		} {
			_, found := FindMetricWithLabelValues(name, map[string]string{})
			Expect(found).To(BeFalse(), name)
		}
	}

	BeforeEach(func() {
		// Cluster state only syncs once it has seen every NodeClaim and Node, so start from an empty cluster.
		ExpectCleanedUp(ctx, env.Client)
		// The API server keeps condition times to the second, so start on a whole second to keep durations exact.
		env.Clock.SetTime(time.Now().Truncate(time.Second))
	})
	AfterEach(func() {
		cloudProvider.RepairPolicy = nil
		ExpectCleanedUp(ctx, env.Client)
		// Each spec builds its own controller, so drop the series it left in the shared metrics.
		for _, gauge := range []opmetrics.GaugeMetric{node.Allocatable, node.TotalPodRequests, node.TotalPodLimits, node.TotalDaemonRequests,
			node.TotalDaemonLimits, node.SystemOverhead, node.Lifetime, node.ClusterUtilization, node.RepairPastDue} {
			gauge.Reset()
		}
		node.RepairPastDueDurationSeconds.Reset()
	})

	Context("Repair", func() {
		BeforeEach(func() {
			startRepair(nodeRepair, badNode, fatalDisk)
		})
		It("should report 0 for each repair condition while no node is past due", func() {
			Expect(ExpectMetricName(node.RepairPastDue.(*opmetrics.PrometheusGauge))).To(Equal("karpenter_nodes_repair_past_due"))
			Expect(ExpectMetricName(node.RepairPastDueDurationSeconds.(*opmetrics.PrometheusHistogram))).To(Equal("karpenter_nodes_repair_past_due_duration_seconds"))
			expectPastDue(map[string]int{"bad_node": 0, "bad_disk": 0})
		})
		It("should not emit any series until another controller marks cluster state synced", func() {
			// Clear the synced flag that startRepair set.
			repairCluster.SetSynced(false)
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			env.Clock.Step(2 * time.Hour)
			expectNoSeries()
			// A node that stops being past due before cluster state is marked synced isn't recorded.
			applyCondition(n, healthyNodeCondition)
			expectNoSeries()
			// Cluster state has every NodeClaim and Node, but the metrics controller doesn't set the flag itself.
			Expect(repairCluster.HasSynced()).To(BeFalse())

			Expect(repairCluster.Synced(repairCtx)).To(BeTrue())
			expectPastDue(map[string]int{"bad_node": 1, "bad_disk": 0})
			expectRecorded("bad_node", 0, 0)
		})
		It("should keep reporting while NodeClaims are still launching", func() {
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			env.Clock.Step(2 * time.Hour)
			expectPastDue(map[string]int{"bad_node": 1})
			// Neither NodeClaim has a node yet, and the sync check fails until the one with no provider ID launches.
			launched, launching := nodePoolNodeClaim(), nodePoolNodeClaim()
			launching.Status.ProviderID = ""
			ExpectApplied(repairCtx, env.Client, launched, launching)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(launched))
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(launching))
			Expect(repairCluster.Synced(repairCtx)).To(BeFalse())
			Expect(repairCluster.HasSynced()).To(BeTrue())
			expectPastDue(map[string]int{"bad_node": 1})
		})
		DescribeTable("should count a node once its condition outlasts the toleration, the registration timeout and the drain bound",
			func(policyBound, nodeClaimBound *time.Duration, drainBound time.Duration) {
				policy := badNode
				policy.TerminationGracePeriod = policyBound
				startRepair(nodeRepair, policy)
				nodeClaim := nodePoolNodeClaim()
				if nodeClaimBound != nil {
					nodeClaim.Spec.TerminationGracePeriod = &metav1.Duration{Duration: *nodeClaimBound}
				}
				unhealthyNode(nodeClaim, badNodeCondition)

				expectPastDueAfter(badNode.TolerationDuration+lifecycle.RegistrationTimeout+drainBound, "bad_node")
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
			expectPastDue(map[string]int{"bad_disk": 0, "bad_node": 0})
			env.Clock.Step(2 * time.Minute)
			expectPastDue(map[string]int{"bad_disk": 1, "bad_node": 0})
			env.Clock.Step(25 * time.Minute)
			expectPastDue(map[string]int{"bad_disk": 2, "bad_node": 1})
		})
		It("should count a node once, under the condition repair acts on", func() {
			// The reboot policy: a node is past due after its 5m toleration, the 15m registration timeout and its 5m drain
			// bound.
			startRepair(nodeRepair, badNode, cloudprovider.RepairPolicy{
				ConditionType:          "BadDisk",
				ConditionStatus:        corev1.ConditionFalse,
				ReasonRegex:            "^Transient$",
				TolerationDuration:     5 * time.Minute,
				TerminationGracePeriod: new(5 * time.Minute),
				Action:                 cloudprovider.RebootNode,
			})
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			applyCondition(n, corev1.NodeCondition{Type: "BadDisk", Status: corev1.ConditionFalse, Reason: "Transient"})

			env.Clock.Step(24 * time.Minute)
			expectPastDue(map[string]int{"bad_disk": 0, "bad_node": 0})
			// Repair acts on the reboot for BadDisk, which is past due.
			env.Clock.Step(2 * time.Minute)
			expectPastDue(map[string]int{"bad_disk": 1, "bad_node": 0})
			// Repair now acts on the replace for BadNode, but the node stays past due from 25m, so nothing is recorded.
			env.Clock.Step(5 * time.Minute)
			expectPastDue(map[string]int{"bad_disk": 0, "bad_node": 1})
			expectRecorded("bad_disk", 0, 0)

			// Both recover at 40m, so the whole stretch is recorded under the condition repair acted on last.
			env.Clock.Step(9 * time.Minute)
			applyCondition(n, healthyNodeCondition)
			applyCondition(n, corev1.NodeCondition{Type: "BadDisk", Status: corev1.ConditionTrue})
			expectPastDue(map[string]int{"bad_disk": 0, "bad_node": 0})
			expectRecorded("bad_node", 1, 15*time.Minute)
			expectRecorded("bad_disk", 0, 0)
		})
		It("should record the whole stretch when another condition keeps the node past due", func() {
			n := unhealthyNode(nodePoolNodeClaim(), fatalDiskCondition)
			applyCondition(n, badNodeCondition)
			// BadDisk makes the node past due from 30m.
			env.Clock.Step(31 * time.Minute)
			expectPastDue(map[string]int{"bad_disk": 1, "bad_node": 0})

			// BadDisk recovers at 60m, but BadNode has kept the node past due since 55m.
			env.Clock.Step(29 * time.Minute)
			applyCondition(n, corev1.NodeCondition{Type: "BadDisk", Status: corev1.ConditionTrue})
			expectPastDue(map[string]int{"bad_disk": 0, "bad_node": 1})
			expectRecorded("bad_node", 0, 0)

			// The node was past due from 30m until BadNode recovers at 70m.
			env.Clock.Step(10 * time.Minute)
			applyCondition(n, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_disk": 0, "bad_node": 0})
			expectRecorded("bad_node", 1, 40*time.Minute)
			expectRecorded("bad_disk", 0, 0)
		})
		It("should not record the time do-not-repair blocked repair", func() {
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			// The node is past due from 55m until do-not-repair blocks repair at 65m.
			env.Clock.Step(60 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			env.Clock.Step(5 * time.Minute)
			setDoNotRepair(n, true)
			expectPastDue(map[string]int{"bad_node": 0})
			expectRecorded("bad_node", 1, 10*time.Minute)

			// Removing do-not-repair at 100m starts a new stretch, which ends when the node recovers at 110m.
			env.Clock.Step(35 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 0})
			setDoNotRepair(n, false)
			expectPastDue(map[string]int{"bad_node": 1})
			env.Clock.Step(10 * time.Minute)
			applyCondition(n, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0})
			expectRecorded("bad_node", 2, 20*time.Minute)
		})
		It("should keep counting a node when a policy with a longer drain bound becomes eligible later", func() {
			// BadNode's policy has no drain bound, so the node is past due 10m + 15m after its condition starts.
			startRepair(nodeRepair,
				cloudprovider.RepairPolicy{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 10 * time.Minute, Action: cloudprovider.ReplaceNode},
				cloudprovider.RepairPolicy{ConditionType: "BadDisk", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Fatal$", TolerationDuration: 50 * time.Minute, TerminationGracePeriod: new(time.Hour), Action: cloudprovider.ReplaceNode},
			)
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			applyCondition(n, fatalDiskCondition)
			expectPastDueAfter(25*time.Minute, "bad_node")

			// BadDisk's policy becomes eligible at 50m. Its longer drain bound doesn't move BadNode's deadline.
			env.Clock.Step(25 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1, "bad_disk": 0})
			expectRecorded("bad_node", 0, 0)
		})
		It("should not count a node while it reboots, and should restart the toleration once the reboot ends", func() {
			nodeClaim := nodePoolNodeClaim()
			unhealthyNode(nodeClaim, badNodeCondition)
			env.Clock.Step(40 * time.Minute)
			setRebooting(nodeClaim, true)

			// The reboot runs past when the node would otherwise be past due, but the reboot controller bounds it.
			env.Clock.Step(time.Hour)
			expectPastDue(map[string]int{"bad_node": 0})

			// Once the reboot ends, the node is past due 55m later, not 55m after the condition started.
			setRebooting(nodeClaim, false)
			expectPastDueAfter(55*time.Minute, "bad_node")
		})
		It("should count a rebooting node once its NodeClaim is being deleted, as after a failed reboot", func() {
			nodeClaim := nodePoolNodeClaim()
			unhealthyNode(nodeClaim, badNodeCondition)
			env.Clock.Step(40 * time.Minute)
			setRebooting(nodeClaim, true)
			env.Clock.Step(time.Hour)
			expectPastDue(map[string]int{"bad_node": 0})

			// When a reboot fails, the reboot controller deletes the NodeClaim and leaves the reboot condition as it is.
			ExpectDeletionTimestampSet(repairCtx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			expectPastDue(map[string]int{"bad_node": 1})
		})
		DescribeTable("should record how long a node was past due once it stops being past due",
			func(stop func(*corev1.Node, *v1.NodeClaim)) {
				nodeClaim := nodePoolNodeClaim()
				n := unhealthyNode(nodeClaim, badNodeCondition)
				// The node is past due from 55m. Nothing is recorded while it stays past due.
				env.Clock.Step(60 * time.Minute)
				expectPastDue(map[string]int{"bad_node": 1})
				env.Clock.Step(5 * time.Minute)
				expectPastDue(map[string]int{"bad_node": 1})
				expectRecorded("bad_node", 0, 0)

				// It stops being past due at 65m.
				stop(n, nodeClaim)
				expectPastDue(map[string]int{"bad_node": 0})
				expectRecorded("bad_node", 1, 10*time.Minute)

				// It's recorded once, not on every pass.
				env.Clock.Step(time.Minute)
				expectPastDue(map[string]int{"bad_node": 0})
				expectRecorded("bad_node", 1, 10*time.Minute)
			},
			Entry("when it recovers", func(n *corev1.Node, _ *v1.NodeClaim) {
				applyCondition(n, healthyNodeCondition)
			}),
			Entry("when repair reboots it", func(_ *corev1.Node, nodeClaim *v1.NodeClaim) {
				setRebooting(nodeClaim, true)
			}),
			Entry("when it is gone", func(n *corev1.Node, nodeClaim *v1.NodeClaim) {
				ExpectDeleted(repairCtx, env.Client, nodeClaim, n)
				ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
				ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			}),
			Entry("when it gets do-not-repair", func(n *corev1.Node, _ *v1.NodeClaim) {
				setDoNotRepair(n, true)
			}),
		)
		It("should not record a negative time when the clock steps back", func() {
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			env.Clock.Step(60 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})

			// The node became past due at 55m, and the clock steps back to 50m as it recovers.
			env.Clock.Step(-10 * time.Minute)
			applyCondition(n, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0})
			expectRecorded("bad_node", 1, 0)
		})
		It("should not record a node that recovers before it is past due", func() {
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			env.Clock.Step(54 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 0})

			applyCondition(n, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0})
			expectRecorded("bad_node", 0, 0)
		})
		It("should ignore a node with do-not-repair", func() {
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			setDoNotRepair(unhealthyNode(nodePoolNodeClaim(), badNodeCondition), true)

			env.Clock.Step(2 * time.Hour)
			expectPastDue(map[string]int{"bad_node": 1})
		})
		It("should ignore a node without a NodePool", func() {
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			unhealthyNode(test.NodeClaim(), badNodeCondition)

			env.Clock.Step(2 * time.Hour)
			expectPastDue(map[string]int{"bad_node": 1})
		})
		It("should ignore a node without a NodeClaim", func() {
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			// It has a NodePool label, so only the missing NodeClaim keeps it from counting. Cluster state only tracks a node
			// like this once it has a provider ID and is initialized.
			n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: "default"}}, ProviderID: test.RandomProviderID()})
			ExpectApplied(repairCtx, env.Client, n)
			ExpectMakeNodesInitialized(repairCtx, env.Client, env.Clock, n)
			applyCondition(n, badNodeCondition)

			env.Clock.Step(2 * time.Hour)
			expectPastDue(map[string]int{"bad_node": 1})
		})
		It("should count a deleting node until it is gone", func() {
			nodeClaim := nodePoolNodeClaim()
			n := unhealthyNode(nodeClaim, badNodeCondition)
			env.Clock.Step(56 * time.Minute)
			ExpectDeletionTimestampSet(repairCtx, env.Client, nodeClaim, n)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			expectPastDue(map[string]int{"bad_node": 1})
			// Nothing is recorded while the node is still deleting.
			env.Clock.Step(time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			expectRecorded("bad_node", 0, 0)

			ExpectFinalizersRemoved(repairCtx, env.Client, nodeClaim, n)
			ExpectNotFound(repairCtx, env.Client, nodeClaim, n)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			expectPastDue(map[string]int{"bad_node": 0})
			// It was past due from 55m until it was gone at 57m.
			expectRecorded("bad_node", 1, 2*time.Minute)
		})
	})

	DescribeTable("should not emit any series while the repair disruption method doesn't run",
		func(fields test.OptionsFields) {
			startRepair(fields, badNode)
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			env.Clock.Step(2 * time.Hour)
			expectNoSeries()

			applyCondition(n, healthyNodeCondition)
			expectNoSeries()
		},
		Entry("when node repair is off", test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: new(false)}}),
		Entry("with --legacy-node-repair, which runs the legacy controller instead",
			test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: new(true)}, LegacyNodeRepair: new(true)}),
	)
})
