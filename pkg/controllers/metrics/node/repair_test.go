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
	"maps"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	prometheusmodel "github.com/prometheus/client_model/go"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
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
	healthyDiskCondition := corev1.NodeCondition{Type: "BadDisk", Status: corev1.ConditionTrue}

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
	nodeClaimIn := func(nodePool string) *v1.NodeClaim {
		return test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool}}})
	}
	nodePoolNodeClaim := func() *v1.NodeClaim { return nodeClaimIn("default") }
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
	healthyNode := func(nodeClaim *v1.NodeClaim) *corev1.Node {
		return unhealthyNode(nodeClaim, healthyNodeCondition)
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
	// seriesOf returns the metric's series in the registry.
	seriesOf := func(name string) []*prometheusmodel.Metric {
		family, _ := lo.Find(lo.Must(crmetrics.Registry.Gather()), func(f *prometheusmodel.MetricFamily) bool { return f.GetName() == name })
		return family.GetMetric()
	}
	labelsOf := func(m *prometheusmodel.Metric) map[string]string {
		return lo.SliceToMap(m.GetLabel(), func(l *prometheusmodel.LabelPair) (string, string) { return l.GetName(), l.GetValue() })
	}
	// onlyLabels checks that there is exactly one series and returns its labels.
	onlyLabels := func(series []*prometheusmodel.Metric) map[string]string {
		GinkgoHelper()
		Expect(series).To(HaveLen(1))
		return labelsOf(series[0])
	}
	pastDueTimeSeries := func() []*prometheusmodel.Metric {
		return seriesOf(ExpectMetricName(node.RepairPastDueTimeSeconds.(*opmetrics.PrometheusGauge)))
	}
	pastDueDurationSeries := func() []*prometheusmodel.Metric {
		return seriesOf(ExpectMetricName(node.RepairPastDueDurationSeconds.(*opmetrics.PrometheusHistogram)))
	}
	// pastDueTime is the part of a RepairPastDueTimeSeconds row that the specs check.
	type pastDueTime struct {
		nodeName  string
		nodePool  string
		condition string
		pastDue   time.Duration
	}
	// pastDueTimes returns the rows that the last pass reported.
	pastDueTimes := func() []pastDueTime {
		return lo.Map(pastDueTimeSeries(), func(m *prometheusmodel.Metric, _ int) pastDueTime {
			labels := labelsOf(m)
			return pastDueTime{nodeName: labels["node_name"], nodePool: labels["nodepool"], condition: labels["condition"], pastDue: time.Duration(m.GetGauge().GetValue() * float64(time.Second))}
		})
	}
	// pastDueReasons returns each row's blocked reason, by node name.
	pastDueReasons := func() map[string]string {
		return lo.SliceToMap(pastDueTimeSeries(), func(m *prometheusmodel.Metric) (string, string) {
			return labelsOf(m)["node_name"], labelsOf(m)["blocked_reason"]
		})
	}
	// holdBack sets why repair's latest pass held back each node, as the repair disruption method would.
	holdBack := func(reasons map[*corev1.Node]opmetrics.Value) {
		repairCluster.UpdateRepairBlockedReasons(lo.MapEntries(reasons, func(n *corev1.Node, reason opmetrics.Value) (string, string) {
			return n.Spec.ProviderID, reason.Name
		}))
	}
	// expectPastDueIn runs a pass and checks how many nodes are past due under each condition in the nodepool, which are
	// the rows with that condition. It also checks that each managed node in a NodePool has one row, and that every row
	// without a condition is 0.
	expectPastDueIn := func(nodePool string, counts map[string]int) {
		GinkgoHelper()
		ExpectSingletonReconciled(repairCtx, repairMetricsController)
		rows := pastDueTimes()
		var managed []string
		for _, n := range repairCluster.DeepCopyNodes() {
			if n.Node != nil && n.Managed() && n.Labels()[v1.NodePoolLabelKey] != "" {
				managed = append(managed, n.Node.Name)
			}
		}
		Expect(lo.Map(rows, func(row pastDueTime, _ int) string { return row.nodeName })).To(ConsistOf(managed), "each managed node in a NodePool should have one row")
		for _, row := range rows {
			if row.condition == "" {
				Expect(row.pastDue).To(BeZero(), row.nodeName+" isn't past due, so it should report 0")
			}
		}
		// A row has a blocked reason exactly when it has a condition, so both are empty while the node isn't past due.
		for _, m := range pastDueTimeSeries() {
			labels := labelsOf(m)
			Expect(labels["blocked_reason"] == "").To(Equal(labels["condition"] == ""), "row for "+labels["node_name"])
		}
		for condition, count := range counts {
			Expect(lo.CountBy(rows, func(row pastDueTime) bool {
				return row.nodePool == nodePool && row.condition == condition
			})).To(Equal(count), "past-due nodes under "+condition)
		}
	}
	// expectPastDue runs a pass and checks the past-due nodes in the default nodepool, which nodePoolNodeClaim uses.
	expectPastDue := func(counts map[string]int) {
		GinkgoHelper()
		expectPastDueIn("default", counts)
	}
	// expectPastDueAfter checks that one node counts under the condition after wait, but not a minute before.
	expectPastDueAfter := func(wait time.Duration, condition string) {
		GinkgoHelper()
		env.Clock.Step(wait - time.Minute)
		expectPastDue(map[string]int{condition: 0})
		env.Clock.Step(2 * time.Minute)
		expectPastDue(map[string]int{condition: 1})
	}
	// expectRecordedWith checks how many stretches the histogram recorded under the labels, and their total time. It adds
	// up the matching series, as `sum by` the labels would.
	expectRecordedWith := func(labels map[string]string, count int, total time.Duration) {
		GinkgoHelper()
		series := lo.Filter(pastDueDurationSeries(), func(m *prometheusmodel.Metric, _ int) bool {
			return maps.Equal(lo.PickByKeys(labelsOf(m), lo.Keys(labels)), labels)
		})
		Expect(lo.SumBy(series, func(m *prometheusmodel.Metric) uint64 { return m.GetHistogram().GetSampleCount() })).To(Equal(uint64(count)), "stretches recorded under %v", labels)
		Expect(lo.SumBy(series, func(m *prometheusmodel.Metric) float64 { return m.GetHistogram().GetSampleSum() })).To(BeNumerically("~", total.Seconds()), "time recorded under %v", labels)
	}
	// expectRecordedIn checks the histogram under the nodepool and condition.
	expectRecordedIn := func(nodePool, condition string, count int, total time.Duration) {
		GinkgoHelper()
		expectRecordedWith(map[string]string{"nodepool": nodePool, "condition": condition}, count, total)
	}
	// expectRecorded checks the histogram for the default nodepool.
	expectRecorded := func(condition string, count int, total time.Duration) {
		GinkgoHelper()
		expectRecordedIn("default", condition, count, total)
	}
	// expectPastDueTimes checks the rows the last pass reported. It runs no pass itself, so a value that is wrong for only
	// one pass still fails.
	expectPastDueTimes := func(want ...pastDueTime) {
		GinkgoHelper()
		Expect(pastDueTimes()).To(ConsistOf(want))
	}
	expectNoSeries := func() {
		GinkgoHelper()
		ExpectSingletonReconciled(repairCtx, repairMetricsController)
		Expect(pastDueTimeSeries()).To(BeEmpty())
		Expect(pastDueDurationSeries()).To(BeEmpty())
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
			node.TotalDaemonLimits, node.SystemOverhead, node.Lifetime, node.ClusterUtilization, node.RepairPastDueTimeSeconds} {
			gauge.Reset()
		}
		node.RepairPastDueDurationSeconds.Reset()
	})

	Context("Repair", func() {
		BeforeEach(func() {
			startRepair(nodeRepair, badNode, fatalDisk)
		})
		It("should report 0, with an empty condition, for each managed node that isn't past due", func() {
			Expect(ExpectMetricName(node.RepairPastDueDurationSeconds.(*opmetrics.PrometheusHistogram))).To(Equal("karpenter_nodes_repair_past_due_duration_seconds"))
			Expect(ExpectMetricName(node.RepairPastDueTimeSeconds.(*opmetrics.PrometheusGauge))).To(Equal("karpenter_nodes_repair_past_due_time_seconds"))
			// With no nodes, there is nothing to report.
			expectNoSeries()
			healthy := healthyNode(nodePoolNodeClaim())
			unhealthy := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0, "bad_disk": 0})
			expectPastDueTimes(pastDueTime{healthy.Name, "default", "", 0}, pastDueTime{unhealthy.Name, "default", "", 0})
			Expect(pastDueDurationSeries()).To(BeEmpty())
		})
		It("should not report a count of past-due nodes, since the values above 0 give it", func() {
			unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			healthyNode(nodePoolNodeClaim())
			env.Clock.Step(2 * time.Hour)
			expectPastDue(map[string]int{"bad_node": 1})
			Expect(lo.CountBy(pastDueTimes(), func(row pastDueTime) bool { return row.pastDue > 0 })).To(Equal(1))
			Expect(seriesOf("karpenter_nodes_repair_past_due")).To(BeEmpty())
		})
		It("should label both metrics like the other node metrics, leaving node_name off the histogram", func() {
			// It has no instance type label, so cluster cost doesn't look up a NodePool that the spec doesn't create.
			n := unhealthyNode(test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey:        "default",
				corev1.LabelTopologyZone:   "test-zone-1",
				corev1.LabelTopologyRegion: "test-region",
				corev1.LabelArchStable:     "arm64",
				v1.CapacityTypeLabelKey:    v1.CapacityTypeOnDemand,
			}}}), badNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0})
			lifetime, found := FindMetricWithLabelValues(ExpectMetricName(node.Lifetime.(*opmetrics.PrometheusGauge)), map[string]string{"node_name": n.Name})
			Expect(found).To(BeTrue())
			nodeLabels := labelsOf(lifetime)
			Expect(nodeLabels).To(And(
				HaveKeyWithValue("managed", "true"),
				HaveKeyWithValue("nodepool", "default"),
				HaveKeyWithValue("zone", "test-zone-1"),
				HaveKeyWithValue("region", "test-region"),
				HaveKeyWithValue("arch", "arm64"),
				HaveKeyWithValue("capacity_type", v1.CapacityTypeOnDemand),
			))
			Expect(onlyLabels(pastDueTimeSeries())).To(Equal(lo.Assign(nodeLabels, map[string]string{"condition": "", "blocked_reason": ""})))

			// No repair pass runs in this suite, so a past-due node's reason is unknown unless a spec sets one.
			env.Clock.Step(56 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			Expect(onlyLabels(pastDueTimeSeries())).To(Equal(lo.Assign(nodeLabels, map[string]string{"condition": "bad_node", "blocked_reason": "unknown"})))

			applyCondition(n, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0})
			Expect(onlyLabels(pastDueDurationSeries())).To(Equal(lo.Assign(lo.OmitByKeys(nodeLabels, []string{"node_name"}), map[string]string{"condition": "bad_node", "blocked_reason": "unknown"})))
		})
		It("should label a past-due node with why repair's latest pass held it back, or unknown", func() {
			held := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			// Step the clock before each new node, since applyCondition moves the clock up to a new node's creation time,
			// which would add time to the earlier nodes.
			env.Clock.Step(time.Minute)
			unexplained := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			env.Clock.Step(29 * time.Minute)
			early := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			// At 60m, the first two nodes are past due, and the last isn't until 85m.
			env.Clock.Step(30 * time.Minute)
			holdBack(map[*corev1.Node]opmetrics.Value{held: disruption.RepairBlockedBudget, early: disruption.RepairBlockedBreaker})
			expectPastDue(map[string]int{"bad_node": 2})
			// A node that isn't past due has an empty blocked_reason, even when repair holds it back.
			Expect(pastDueReasons()).To(Equal(map[string]string{held.Name: "budget", unexplained.Name: "unknown", early.Name: ""}))
		})
		It("should label a past-due node replacing and then deleting once repair acts on it, and record the reason it had longest", func() {
			nodeClaim := nodePoolNodeClaim()
			n := unhealthyNode(nodeClaim, badNodeCondition)
			holdBack(map[*corev1.Node]opmetrics.Value{n: disruption.RepairBlockedBudget})
			// The budget holds the node back from when it is past due at 55m until 65m.
			env.Clock.Step(55 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			Expect(pastDueReasons()).To(HaveKeyWithValue(n.Name, "budget"))

			// The disruption queue marks the node while its replacement launches, which beats repair's older reason.
			env.Clock.Step(10 * time.Minute)
			repairCluster.MarkForDeletion(n.Spec.ProviderID)
			expectPastDue(map[string]int{"bad_node": 1})
			Expect(pastDueReasons()).To(HaveKeyWithValue(n.Name, "replacing"))

			// The queue deletes the node at 67m.
			env.Clock.Step(2 * time.Minute)
			ExpectDeletionTimestampSet(repairCtx, env.Client, nodeClaim, n)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			expectPastDue(map[string]int{"bad_node": 1})
			Expect(pastDueReasons()).To(HaveKeyWithValue(n.Name, "deleting"))

			// It's gone at 68m: past due for 13m, 10m of them held back by the budget.
			env.Clock.Step(time.Minute)
			ExpectFinalizersRemoved(repairCtx, env.Client, nodeClaim, n)
			ExpectNotFound(repairCtx, env.Client, nodeClaim, n)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			ExpectSingletonReconciled(repairCtx, repairMetricsController)
			expectRecordedWith(map[string]string{"condition": "bad_node", "blocked_reason": "budget"}, 1, 13*time.Minute)
			Expect(pastDueDurationSeries()).To(HaveLen(1))
		})
		It("should replace a past-due node's row when its reason changes, and keep counting its time", func() {
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			holdBack(map[*corev1.Node]opmetrics.Value{n: disruption.RepairBlockedBudget})
			env.Clock.Step(60 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			Expect(pastDueReasons()).To(Equal(map[string]string{n.Name: "budget"}))
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_node", 5 * time.Minute})

			// The breaker holds the node back now. The node keeps one row, and its time still counts from 55m.
			holdBack(map[*corev1.Node]opmetrics.Value{n: disruption.RepairBlockedBreaker})
			env.Clock.Step(10 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			Expect(pastDueReasons()).To(Equal(map[string]string{n.Name: "breaker"}))
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_node", 15 * time.Minute})
			expectRecorded("bad_node", 0, 0)
		})
		// expectRecordedReason holds the node back for first, then for second, and checks the reason the stretch is
		// recorded under. A nil reason means repair gave none.
		expectRecordedReason := func(first opmetrics.Value, firstFor time.Duration, second *opmetrics.Value, secondFor time.Duration, want string) {
			GinkgoHelper()
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			holdBack(map[*corev1.Node]opmetrics.Value{n: first})
			// The node is past due from 55m.
			env.Clock.Step(55 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			holdBack(lo.Ternary(second == nil, nil, map[*corev1.Node]opmetrics.Value{n: lo.FromPtr(second)}))
			env.Clock.Step(firstFor)
			expectPastDue(map[string]int{"bad_node": 1})
			env.Clock.Step(secondFor)
			applyCondition(n, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0})
			expectRecordedWith(map[string]string{"condition": "bad_node", "blocked_reason": want}, 1, firstFor+secondFor)
			Expect(pastDueDurationSeries()).To(HaveLen(1))
		}
		It("should record a stretch under the reason it had for the most time, not the one the label lists first", func() {
			// The label lists breaker before budget, so only the times can pick budget.
			expectRecordedReason(disruption.RepairBlockedBreaker, 2*time.Minute, &disruption.RepairBlockedBudget, 8*time.Minute, "budget")
		})
		It("should record a stretch as unknown when repair gave no reason for most of it", func() {
			expectRecordedReason(disruption.RepairBlockedBudget, 2*time.Minute, nil, 8*time.Minute, "unknown")
		})
		DescribeTable("should record a stretch under the reason the label lists first when two reasons tie",
			func(first, second opmetrics.Value) {
				// The label lists breaker before budget, whichever came first.
				expectRecordedReason(first, 5*time.Minute, &second, 5*time.Minute, "breaker")
			},
			Entry("when budget comes first", disruption.RepairBlockedBudget, disruption.RepairBlockedBreaker),
			Entry("when breaker comes first", disruption.RepairBlockedBreaker, disruption.RepairBlockedBudget),
		)
		It("should record the time before a restart as unknown, since no pass saw it", func() {
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			// The node is past due from 55m, but this controller first runs at 75m, as after a restart.
			holdBack(map[*corev1.Node]opmetrics.Value{n: disruption.RepairBlockedBudget})
			env.Clock.Step(75 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			// The budget holds it back for the 10 minutes this controller sees.
			env.Clock.Step(10 * time.Minute)
			applyCondition(n, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0})
			expectRecordedWith(map[string]string{"condition": "bad_node", "blocked_reason": "unknown"}, 1, 30*time.Minute)
			Expect(pastDueDurationSeries()).To(HaveLen(1))
		})
		It("should record a stretch under the node's labels from the last pass in which it was past due", func() {
			reserved := map[string]string{v1.NodePoolLabelKey: "default", v1.CapacityTypeLabelKey: v1.CapacityTypeReserved}
			setCapacityType := func(n *corev1.Node, capacityType string) {
				n = ExpectExists(repairCtx, env.Client, n)
				n.Labels[v1.CapacityTypeLabelKey] = capacityType
				ExpectApplied(repairCtx, env.Client, n)
				ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			}
			moved := unhealthyNode(test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: reserved}}), badNodeCondition)
			// The other node becomes unhealthy a minute later, so the two are past due from 55m and 56m. Stepping the clock
			// first also keeps the other node's creation from moving the clock and adding a second to the first node's time.
			env.Clock.Step(time.Minute)
			ended := unhealthyNode(test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: reserved}}), badNodeCondition)
			env.Clock.Step(56 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 2})

			// The first node's capacity type changes while it is past due, as when its capacity reservation ends. Its row
			// moves to the new value.
			setCapacityType(moved, v1.CapacityTypeOnDemand)
			env.Clock.Step(time.Minute)
			expectPastDue(map[string]int{"bad_node": 2})
			Expect(pastDueTimeSeries()).To(HaveLen(2))
			Expect(lo.SliceToMap(pastDueTimeSeries(), func(m *prometheusmodel.Metric) (string, string) {
				return labelsOf(m)["node_name"], labelsOf(m)["capacity_type"]
			})).To(Equal(map[string]string{moved.Name: v1.CapacityTypeOnDemand, ended.Name: v1.CapacityTypeReserved}))

			// Both recover at 59m. The other node's capacity type changes after the 58m pass, just before it recovers, so its
			// stretch is still recorded under reserved, the value that pass saw.
			env.Clock.Step(time.Minute)
			applyCondition(moved, healthyNodeCondition)
			setCapacityType(ended, v1.CapacityTypeOnDemand)
			applyCondition(ended, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0})
			expectRecordedWith(map[string]string{"capacity_type": v1.CapacityTypeOnDemand}, 1, 4*time.Minute)
			expectRecordedWith(map[string]string{"capacity_type": v1.CapacityTypeReserved}, 1, 3*time.Minute)
		})
		It("should report and record each node under its own nodepool", func() {
			healthy := healthyNode(nodePoolNodeClaim())
			otherNodeClaim := nodeClaimIn("other")
			disk := unhealthyNode(otherNodeClaim, fatalDiskCondition)
			// The other nodepool's node is past due from 30m, and the default nodepool's node stays healthy.
			env.Clock.Step(31 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 0, "bad_disk": 0})
			expectPastDueIn("other", map[string]int{"bad_node": 0, "bad_disk": 1})
			expectPastDueTimes(pastDueTime{healthy.Name, "default", "", 0}, pastDueTime{disk.Name, "other", "bad_disk", time.Minute})

			applyCondition(disk, healthyDiskCondition)
			expectPastDueIn("other", map[string]int{"bad_disk": 0})
			expectRecordedIn("other", "bad_disk", 1, time.Minute)
			expectRecorded("bad_disk", 0, 0)
			expectPastDueTimes(pastDueTime{healthy.Name, "default", "", 0}, pastDueTime{disk.Name, "other", "", 0})

			// Once the other nodepool's node is gone, its 0 row goes too, and nothing more is recorded.
			ExpectDeleted(repairCtx, env.Client, otherNodeClaim, disk)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(otherNodeClaim))
			ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(disk))
			expectPastDue(map[string]int{"bad_node": 0, "bad_disk": 0})
			expectPastDueTimes(pastDueTime{healthy.Name, "default", "", 0})
			expectRecordedIn("other", "bad_disk", 1, time.Minute)
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
		It("should report how long each node has been past due while it is past due", func() {
			first := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			env.Clock.Step(10 * time.Minute)
			disk := unhealthyNode(nodePoolNodeClaim(), fatalDiskCondition)
			later := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)

			// At 60m, the first node has been past due since 55m and the disk node since 40m. The later node is past due
			// from 65m, so it still reports 0.
			env.Clock.Step(50 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1, "bad_disk": 1})
			expectPastDueTimes(
				pastDueTime{first.Name, "default", "bad_node", 5 * time.Minute},
				pastDueTime{disk.Name, "default", "bad_disk", 20 * time.Minute},
				pastDueTime{later.Name, "default", "", 0},
			)
			// At 70m, the times have grown and the later node is past due too.
			env.Clock.Step(10 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 2, "bad_disk": 1})
			expectPastDueTimes(
				pastDueTime{first.Name, "default", "bad_node", 15 * time.Minute},
				pastDueTime{disk.Name, "default", "bad_disk", 30 * time.Minute},
				pastDueTime{later.Name, "default", "bad_node", 5 * time.Minute},
			)
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
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_disk", time.Minute})
			// Repair now acts on the replace for BadNode, but the node stays past due from 25m, so nothing is recorded.
			env.Clock.Step(5 * time.Minute)
			expectPastDue(map[string]int{"bad_disk": 0, "bad_node": 1})
			expectRecorded("bad_disk", 0, 0)
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_node", 6 * time.Minute})

			// Both recover at 40m, so the whole stretch is recorded under the condition repair acted on last.
			env.Clock.Step(9 * time.Minute)
			applyCondition(n, healthyNodeCondition)
			applyCondition(n, healthyDiskCondition)
			expectPastDue(map[string]int{"bad_disk": 0, "bad_node": 0})
			expectRecorded("bad_node", 1, 15*time.Minute)
			expectRecorded("bad_disk", 0, 0)
			expectPastDueTimes(pastDueTime{n.Name, "default", "", 0})
		})
		It("should record the whole stretch when another condition keeps the node past due", func() {
			n := unhealthyNode(nodePoolNodeClaim(), fatalDiskCondition)
			applyCondition(n, badNodeCondition)
			// BadDisk makes the node past due from 30m.
			env.Clock.Step(31 * time.Minute)
			expectPastDue(map[string]int{"bad_disk": 1, "bad_node": 0})
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_disk", time.Minute})

			// BadDisk recovers at 60m, but BadNode has kept the node past due since 55m.
			env.Clock.Step(29 * time.Minute)
			applyCondition(n, healthyDiskCondition)
			expectPastDue(map[string]int{"bad_disk": 0, "bad_node": 1})
			expectRecorded("bad_node", 0, 0)
			// Its time still counts from 30m.
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_node", 30 * time.Minute})

			// The node was past due from 30m until BadNode recovers at 70m.
			env.Clock.Step(10 * time.Minute)
			applyCondition(n, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_disk": 0, "bad_node": 0})
			expectRecorded("bad_node", 1, 40*time.Minute)
			expectRecorded("bad_disk", 0, 0)
			expectPastDueTimes(pastDueTime{n.Name, "default", "", 0})
		})
		It("should not record or report the time do-not-repair blocked repair", func() {
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			// The node is past due from 55m until do-not-repair blocks repair at 65m. Then it reports 0.
			env.Clock.Step(60 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			env.Clock.Step(5 * time.Minute)
			setDoNotRepair(n, true)
			expectPastDue(map[string]int{"bad_node": 0})
			expectPastDueTimes(pastDueTime{n.Name, "default", "", 0})
			expectRecorded("bad_node", 1, 10*time.Minute)

			// Removing do-not-repair at 100m starts a new stretch, which ends when the node recovers at 110m.
			env.Clock.Step(35 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 0})
			setDoNotRepair(n, false)
			expectPastDue(map[string]int{"bad_node": 1})
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_node", 0})
			env.Clock.Step(10 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_node", 10 * time.Minute})
			applyCondition(n, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0})
			expectPastDueTimes(pastDueTime{n.Name, "default", "", 0})
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
		It("should keep the deadline from the first eligible policy when one with a shorter drain bound becomes eligible later", func() {
			forcefulDisk := fatalDisk
			forcefulDisk.TolerationDuration = 35 * time.Minute
			forcefulDisk.TerminationGracePeriod = new(time.Duration(0))
			startRepair(nodeRepair, badNode, forcefulDisk)
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			applyCondition(n, fatalDiskCondition)
			// BadNode's policy is eligible first, at 30m, with a 10m drain bound, so the node is past due from 55m. BadDisk's
			// policy alone would give 50m.
			expectPastDueAfter(55*time.Minute, "bad_node")
		})
		DescribeTable("should keep the first eligible policy's drain bound when a policy without one is eligible too",
			func(unboundedToleration time.Duration, condition string) {
				unboundedDisk := fatalDisk
				unboundedDisk.TolerationDuration = unboundedToleration
				unboundedDisk.TerminationGracePeriod = nil
				startRepair(nodeRepair, badNode, unboundedDisk)
				n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
				applyCondition(n, fatalDiskCondition)
				// BadNode's policy is eligible from 30m, and repair keeps its 10m drain bound, so the node is past due from 55m.
				expectPastDueAfter(55*time.Minute, condition)
			},
			// Policies with the same action, priority and eligibility select the lower condition type.
			Entry("when both become eligible together", 30*time.Minute, "bad_disk"),
			Entry("when the one without a bound becomes eligible later", 35*time.Minute, "bad_node"),
		)
		It("should report 0 for a node while it reboots, and should restart the toleration once the reboot ends", func() {
			nodeClaim := nodePoolNodeClaim()
			n := unhealthyNode(nodeClaim, badNodeCondition)
			env.Clock.Step(40 * time.Minute)
			setRebooting(nodeClaim, true)

			// The reboot runs past when the node would otherwise be past due, but the reboot controller bounds it.
			env.Clock.Step(time.Hour)
			expectPastDue(map[string]int{"bad_node": 0})
			expectPastDueTimes(pastDueTime{n.Name, "default", "", 0})

			// Once the reboot ends, the node is past due 55m later, not 55m after the condition started.
			setRebooting(nodeClaim, false)
			expectPastDueAfter(55*time.Minute, "bad_node")
		})
		It("should count a rebooting node once its NodeClaim is being deleted, as after a failed reboot", func() {
			nodeClaim := nodePoolNodeClaim()
			n := unhealthyNode(nodeClaim, badNodeCondition)
			env.Clock.Step(40 * time.Minute)
			setRebooting(nodeClaim, true)
			env.Clock.Step(time.Hour)
			expectPastDue(map[string]int{"bad_node": 0})

			// When a reboot fails, the reboot controller deletes the NodeClaim and leaves the reboot condition as it is.
			ExpectDeletionTimestampSet(repairCtx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			expectPastDue(map[string]int{"bad_node": 1})
			// The reboot held the node back, so its time starts at this pass.
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_node", 0})
		})
		DescribeTable("should record how long a node was past due, and report 0 for it or drop its row, once it stops being past due",
			func(stop func(*corev1.Node, *v1.NodeClaim), gone bool) {
				nodeClaim := nodePoolNodeClaim()
				n := unhealthyNode(nodeClaim, badNodeCondition)
				// Another node becomes unhealthy a minute later and stays past due throughout. Stepping the clock first keeps
				// its creation from moving the clock and adding a second to this node's time.
				env.Clock.Step(time.Minute)
				other := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
				// The node is past due from 55m, and the other node from 56m. Nothing is recorded while they stay past due.
				env.Clock.Step(59 * time.Minute)
				expectPastDue(map[string]int{"bad_node": 2})
				env.Clock.Step(5 * time.Minute)
				expectPastDue(map[string]int{"bad_node": 2})
				expectRecorded("bad_node", 0, 0)
				expectPastDueTimes(pastDueTime{n.Name, "default", "bad_node", 10 * time.Minute}, pastDueTime{other.Name, "default", "bad_node", 9 * time.Minute})

				// It stops being past due at 65m. It reports 0 again unless it is gone, and the other node keeps its row.
				stop(n, nodeClaim)
				expectPastDue(map[string]int{"bad_node": 1})
				expectRecorded("bad_node", 1, 10*time.Minute)
				rows := []pastDueTime{{other.Name, "default", "bad_node", 9 * time.Minute}}
				if !gone {
					rows = append(rows, pastDueTime{n.Name, "default", "", 0})
				}
				expectPastDueTimes(rows...)

				// It's recorded once, not on every pass.
				env.Clock.Step(time.Minute)
				expectPastDue(map[string]int{"bad_node": 1})
				expectRecorded("bad_node", 1, 10*time.Minute)
			},
			Entry("when it recovers", func(n *corev1.Node, _ *v1.NodeClaim) {
				applyCondition(n, healthyNodeCondition)
			}, false),
			Entry("when repair reboots it", func(_ *corev1.Node, nodeClaim *v1.NodeClaim) {
				setRebooting(nodeClaim, true)
			}, false),
			Entry("when it is gone", func(n *corev1.Node, nodeClaim *v1.NodeClaim) {
				ExpectDeleted(repairCtx, env.Client, nodeClaim, n)
				ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
				ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			}, true),
			Entry("when it gets do-not-repair", func(n *corev1.Node, _ *v1.NodeClaim) {
				setDoNotRepair(n, true)
			}, false),
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
		It("should not report a negative time when the clock steps back", func() {
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			// do-not-repair holds the node back until 60m, so its stretch starts then, not at 55m.
			setDoNotRepair(n, true)
			env.Clock.Step(60 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 0})
			setDoNotRepair(n, false)
			env.Clock.Step(time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_node", time.Minute})

			// The clock steps back to 56m. The node is still past due, but its stretch starts later.
			env.Clock.Step(-5 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			expectPastDueTimes(pastDueTime{n.Name, "default", "bad_node", 0})
		})
		It("should report 0 for a node that recovers before it is past due, and record nothing", func() {
			n := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			env.Clock.Step(54 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 0})
			expectPastDueTimes(pastDueTime{n.Name, "default", "", 0})

			applyCondition(n, healthyNodeCondition)
			expectPastDue(map[string]int{"bad_node": 0})
			expectPastDueTimes(pastDueTime{n.Name, "default", "", 0})
			expectRecorded("bad_node", 0, 0)
		})
		It("should report 0 for a node with do-not-repair", func() {
			overdue := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			// Step the clock first, so creating the other node can't move it and add a second to the first node's time.
			env.Clock.Step(time.Minute)
			vetoed := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			setDoNotRepair(vetoed, true)

			env.Clock.Step(119 * time.Minute)
			expectPastDue(map[string]int{"bad_node": 1})
			expectPastDueTimes(pastDueTime{overdue.Name, "default", "bad_node", 65 * time.Minute}, pastDueTime{vetoed.Name, "default", "", 0})
		})
		It("should not report a node without a NodePool", func() {
			inNodePool := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			unhealthyNode(test.NodeClaim(), badNodeCondition)

			env.Clock.Step(2 * time.Hour)
			expectPastDue(map[string]int{"bad_node": 1})
			// Repair doesn't act on a node without a NodePool, so it gets no row.
			Expect(onlyLabels(pastDueTimeSeries())).To(HaveKeyWithValue("node_name", inNodePool.Name))
		})
		It("should not report a node without a NodeClaim", func() {
			managed := unhealthyNode(nodePoolNodeClaim(), badNodeCondition)
			// It has a NodePool label, so only the missing NodeClaim keeps it from getting a row. Cluster state only tracks a
			// node like this once it has a provider ID and is initialized.
			n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: "unmanaged"}}, ProviderID: test.RandomProviderID()})
			ExpectApplied(repairCtx, env.Client, n)
			ExpectMakeNodesInitialized(repairCtx, env.Client, env.Clock, n)
			applyCondition(n, badNodeCondition)

			env.Clock.Step(2 * time.Hour)
			expectPastDue(map[string]int{"bad_node": 1})
			// Repair doesn't act on a node that Karpenter doesn't manage, so it gets no row.
			Expect(onlyLabels(pastDueTimeSeries())).To(HaveKeyWithValue("node_name", managed.Name))
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
			ExpectSingletonReconciled(repairCtx, repairMetricsController)
			// Its row is gone with it.
			expectPastDueTimes()
			// It was past due from 55m until it was gone at 57m.
			expectRecorded("bad_node", 1, 2*time.Minute)
		})
		It("should report 0 for a deleting node that isn't past due, until it is gone", func() {
			nodeClaim := nodePoolNodeClaim()
			n := unhealthyNode(nodeClaim, badNodeCondition)
			ExpectDeletionTimestampSet(repairCtx, env.Client, nodeClaim, n)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			expectPastDue(map[string]int{"bad_node": 0})
			expectPastDueTimes(pastDueTime{n.Name, "default", "", 0})

			ExpectFinalizersRemoved(repairCtx, env.Client, nodeClaim, n)
			ExpectNotFound(repairCtx, env.Client, nodeClaim, n)
			ExpectReconcileSucceeded(repairCtx, repairNodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(repairCtx, repairNodeController, client.ObjectKeyFromObject(n))
			expectPastDue(map[string]int{"bad_node": 0})
			expectPastDueTimes()
			expectRecorded("bad_node", 0, 0)
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
