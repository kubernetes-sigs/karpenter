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
	"reflect"
	"sort"
	"strings"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	metricsnode "sigs.k8s.io/karpenter/pkg/controllers/metrics/node"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	karpenterevents "sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/virtualpods"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

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

// listErrorClient fails lists of failList's type while err is set.
type listErrorClient struct {
	client.Client
	failList client.ObjectList
	err      error
}

func (c *listErrorClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.err != nil && reflect.TypeOf(list) == reflect.TypeOf(c.failList) {
		return c.err
	}
	return c.Client.List(ctx, list, opts...)
}

// patchErrorClient fails patches of failPatch's type while err is set. It doesn't fail status patches.
type patchErrorClient struct {
	client.Client
	failPatch client.Object
	err       error
}

func (c *patchErrorClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.err != nil && reflect.TypeOf(obj) == reflect.TypeOf(c.failPatch) {
		return c.err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// reasonsReader is a disruption method that never disrupts. It calls read for each candidate it sees, so a spec can
// check what cluster state and the gauge hold while a method after repair runs.
type reasonsReader struct {
	read func()
}

func (r *reasonsReader) ShouldDisrupt(context.Context, *disruption.Candidate) bool {
	r.read()
	return false
}

func (r *reasonsReader) ComputeCommands(context.Context, map[string]int, ...*disruption.Candidate) ([]disruption.Command, error) {
	return []disruption.Command{}, nil
}

func (r *reasonsReader) Reason() v1.DisruptionReason { return v1.DisruptionReasonUnderutilized }
func (r *reasonsReader) Class() string               { return disruption.GracefulDisruptionClass }
func (r *reasonsReader) ConsolidationType() string   { return "" }

// expectHeldBack checks that repair's latest pass held back the node for the reason, and that the labels list it. Only
// the held-back count's label lists do-not-repair, since a node with it is never past due.
func expectHeldBack(n *corev1.Node, reason opmetrics.Value) {
	GinkgoHelper()
	Expect(disruption.UnhealthyBlockedReason.Values).To(ContainElement(reason))
	if reason != disruption.RepairBlockedDoNotRepair {
		Expect(disruption.RepairBlockedReason.Values).To(ContainElement(reason))
	}
	Expect(cluster.RepairBlockedReasons()).To(HaveKeyWithValue(n.Spec.ProviderID, reason.Name), "reason for node %s", n.Name)
}

// expectNotHeldBack checks that repair's latest pass gave the nodes no reason.
func expectNotHeldBack(nodes ...*corev1.Node) {
	GinkgoHelper()
	for _, n := range nodes {
		Expect(cluster.RepairBlockedReasons()).ToNot(HaveKey(n.Spec.ProviderID), "reason for node %s", n.Name)
	}
}

func blockedLabels(np *v1.NodePool, reason opmetrics.Value) map[string]string {
	return map[string]string{metrics.NodePoolLabel: np.Name, disruption.UnhealthyBlockedReason.Name: reason.Name}
}

func blockedSeriesFound(labels map[string]string) bool {
	GinkgoHelper()
	_, found := FindMetricWithLabelValues(ExpectMetricName(disruption.NodeClaimsUnhealthyBlocked.(*opmetrics.PrometheusGauge)), labels)
	return found
}

// expectBlocked checks how many of the NodePool's nodes the gauge counts under the reason.
func expectBlocked(np *v1.NodePool, reason opmetrics.Value, count float64) {
	GinkgoHelper()
	ExpectMetricGaugeValue(disruption.NodeClaimsUnhealthyBlocked, count, blockedLabels(np, reason))
}

// expectNoneBlocked checks that the gauge has no series for the NodePool.
func expectNoneBlocked(np *v1.NodePool) {
	GinkgoHelper()
	Expect(blockedSeriesFound(map[string]string{metrics.NodePoolLabel: np.Name})).To(BeFalse(), "nodepool %s still has a blocked series", np.Name)
}

// tickingClock is a fake clock that moves on a millisecond every time it is read.
type tickingClock struct {
	*clocktesting.FakeClock
}

func (c tickingClock) Now() time.Time {
	c.Step(time.Millisecond)
	return c.FakeClock.Now()
}

func (c tickingClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

// These tests exercise end-user behavior of voluntary node repair. The disruption controller is built with only the
// Repair method so the behavior under test is isolated from consolidation/drift.

var _ = Describe("Repair", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	var repairController *disruption.Controller
	var repair *disruption.Repair

	labels := func() map[string]string {
		return map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}
	}
	labelsFor := func(np *v1.NodePool) map[string]string {
		return lo.Assign(labels(), map[string]string{v1.NodePoolLabelKey: np.Name})
	}

	// initNode applies + initializes a node/nodeclaim and syncs cluster state. Initialization overwrites
	// Status.Conditions with Ready=True, so unhealthy conditions must be stamped AFTER this via markUnhealthy.
	initNode := func(nc *v1.NodeClaim, n *corev1.Node) {
		ExpectApplied(ctx, env.Client, nc, n)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{n}, []*v1.NodeClaim{nc})
	}

	// bindReschedulablePod places a ReplicaSet-owned (reschedulable) pod on the node so pre-spin sizing has workload
	// demand to protect. Any overrides are applied on top.
	bindReschedulablePod := func(n *corev1.Node, overrides ...test.PodOptions) {
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		pod := test.Pod(append([]test.PodOptions{{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"repair-test": "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: new(true), BlockOwnerDeletion: new(true),
			}},
		}}}, overrides...)...)
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, n)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
	}

	// bindBlockingPod places a do-not-disrupt pod on the node. Such a pod blocks eviction, so the node is only a
	// disruption candidate when the drain is bounded by a hard deadline (repair's RepairPolicy TGP, or the NodeClaim TGP).
	bindBlockingPod := func(n *corev1.Node) *corev1.Pod {
		pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}}})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, n)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
		return pod
	}

	// markUnhealthy appends a condition matching a RepairPolicy at the current fake-clock time, re-applies the node,
	// and re-syncs cluster state. Must be called AFTER initNode.
	markUnhealthyWithReason := func(n *corev1.Node, condType corev1.NodeConditionType, reason string) {
		n = ExpectExists(ctx, env.Client, n)
		if env.Clock.Now().Before(n.CreationTimestamp.Time) {
			env.Clock.SetTime(n.CreationTimestamp.Time)
		}
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
	// markHealthy removes the condition and re-syncs cluster state.
	markHealthy := func(n *corev1.Node, condType corev1.NodeConditionType) {
		n = ExpectExists(ctx, env.Client, n)
		n.Status.Conditions = lo.Reject(n.Status.Conditions, func(c corev1.NodeCondition, _ int) bool { return c.Type == condType })
		ExpectApplied(ctx, env.Client, n)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
	}

	// newRepairControllerWith builds a repair-only disruption controller whose controller and repair method use
	// kubeClient, so a spec can make their API calls fail. The provisioner, queue and cluster state still use env.Client.
	newRepairControllerWith := func(kubeClient client.Client) (*disruption.Repair, *disruption.Controller) {
		r := disruption.NewRepair(disruption.MakeConsolidation(env.Clock, cluster, kubeClient, prov, cloudProvider, recorder, queue))
		return r, disruption.NewController(ctx, env.Clock, kubeClient, prov, cloudProvider, recorder, cluster, queue, clusterCost,
			disruption.WithMethods(r))
	}

	// newRepairController builds an isolated repair-only disruption controller around the suite's repair policy matcher,
	// so specs that change policies with useRepairPolicies must call this again.
	newRepairController := func() {
		repair, repairController = newRepairControllerWith(env.Client)
	}

	BeforeEach(func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: new(true)}}))
		// Single default policy: BadNode/False, 30m toleration (the fake cloud provider default).
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		})
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		ExpectApplied(ctx, env.Client, nodePool)
		// Repair runs as an isolated method so tests assert repair behavior only.
		newRepairController()
	})

	// Repair pre-spins a replacement (replace-then-terminate) and only after the toleration elapses.
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

	It("should commit an in-place reboot (no replacement, no termination) for a RebootNode policy", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "RebootMe", TolerationDuration: 10 * time.Minute, Action: cloudprovider.RebootNode, TerminationGracePeriod: lo.ToPtr(5 * time.Minute)},
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		})
		newRepairController()
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "RebootMe")
		env.Clock.Step(11 * time.Minute)

		// A committed reboot stops the pass and requeues immediately.
		result := ExpectSingletonReconciled(ctx, repairController)
		Expect(result.RequeueAfter).To(Equal(singleton.RequeueImmediately))

		// Reboot is in-place, so nothing is queued.
		Expect(queue.GetCommands()).To(BeEmpty())

		// The NodeClaim is handed off to the reboot controller
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting)
		Expect(cond.IsTrue()).To(BeTrue())
		Expect(cond.Reason).To(Equal(v1.RebootReasonRequested))
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.RebootTerminationGracePeriodAnnotationKey, "5m0s"))
		Expect(nodeClaim.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDisruptionReason).Reason).To(Equal(string(v1.DisruptionReasonUnhealthy)))
		ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, 1, map[string]string{
			"decision":          string(disruption.RebootDecision),
			metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonUnhealthy)),
		})
		ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
			metrics.ReasonLabel:   strings.ToLower(string(v1.DisruptionReasonUnhealthy)),
			metrics.NodePoolLabel: nodePool.Name,
		})
		ExpectMetricCounterValue(disruption.NodeClaimsUnhealthyDisruptedTotal, 1, map[string]string{
			"condition":                  "bad_node",
			metrics.NodePoolLabel:        nodePool.Name,
			metrics.TerminationModeLabel: metrics.TerminationModeEventual,
		})

		// The next pass must not recommit the reboot or clear its DisruptionReason.
		result = ExpectSingletonReconciled(ctx, repairController)
		Expect(result.RequeueAfter).ToNot(Equal(singleton.RequeueImmediately))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDisruptionReason).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonRequested))
	})

	It("should leave the reboot TGP annotation unset (unbounded) when neither policy nor NodeClaim sets one", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "RebootMe", TolerationDuration: 10 * time.Minute, Action: cloudprovider.RebootNode},
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		})
		newRepairController()
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "RebootMe")
		env.Clock.Step(11 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonRequested))
		// nil policy TGP + nil NodeClaim TGP -> unbounded graceful drain: the annotation is left unset (not "0s").
		Expect(nodeClaim.Annotations).ToNot(HaveKey(v1.RebootTerminationGracePeriodAnnotationKey))
	})

	It("should inherit the NodeClaim's TGP as the reboot bound when the policy sets none", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "RebootMe", TolerationDuration: 10 * time.Minute, Action: cloudprovider.RebootNode},
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		})
		newRepairController()
		nodeClaim.Spec.TerminationGracePeriod = &metav1.Duration{Duration: 7 * time.Minute}
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "RebootMe")
		env.Clock.Step(11 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonRequested))
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.RebootTerminationGracePeriodAnnotationKey, "7m0s"))
	})

	It("should remove a stale reboot TGP annotation when the new reboot is unbounded", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "RebootMe", TolerationDuration: 10 * time.Minute, Action: cloudprovider.RebootNode},
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		})
		newRepairController()
		nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.RebootTerminationGracePeriodAnnotationKey: "5m0s"})
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "RebootMe")
		env.Clock.Step(11 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonRequested))
		Expect(nodeClaim.Annotations).ToNot(HaveKey(v1.RebootTerminationGracePeriodAnnotationKey))
	})

	Context("reboot history", func() {
		BeforeEach(func() {
			useRepairPolicies([]cloudprovider.RepairPolicy{
				{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "RebootMe", TolerationDuration: 10 * time.Minute, Action: cloudprovider.RebootNode},
				// Required default fallback; its toleration outlasts these specs so only history escalates to replace.
				{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 48 * time.Hour, Action: cloudprovider.ReplaceNode},
			})
			newRepairController()
			initNode(nodeClaim, node)
			markUnhealthyWithReason(node, "BadNode", "RebootMe")
			env.Clock.Step(11 * time.Minute)
		})

		// rebootAndSucceed commits a reboot through repair, then completes it successfully while the fault persists,
		// so the node is eligible for repair again.
		rebootAndSucceed := func() {
			GinkgoHelper()
			// Commits on the first pass, even right after a previous reboot finished: the commit re-reads the
			// NodeClaim, so the sweep clearing the finished reboot's DisruptionReason in the same pass can't conflict.
			result := ExpectSingletonReconciled(ctx, repairController)
			Expect(result.RequeueAfter).To(Equal(singleton.RequeueImmediately))
			Expect(queue.GetCommands()).To(BeEmpty())
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonRequested))
			nodeClaim.StatusConditions().SetFalse(v1.ConditionTypeRebooting, v1.RebootReasonSucceeded, "node rebooted and rejoined the cluster")
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))
			node = ExpectExists(ctx, env.Client, node)
			for i := range node.Status.Conditions {
				if node.Status.Conditions[i].Type == "BadNode" {
					node.Status.Conditions[i].LastTransitionTime = metav1.Time{Time: env.Clock.Now()}
				}
			}
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
			env.Clock.Step(11 * time.Minute)
		}

		It("should escalate to replacement when the fault recurs after two successful reboots", func() {
			rebootAndSucceed()
			rebootAndSucceed()

			// A third occurrence within the window replaces the node instead of rebooting it again.
			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(HaveLen(1))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonSucceeded))
		})

		It("should reboot again once earlier reboots age out of the window", func() {
			rebootAndSucceed()
			rebootAndSucceed()

			// The history window runs on the repair clock, so reboots older than 24h no longer count.
			env.Clock.Step(25 * time.Hour)
			rebootAndSucceed()
		})
	})

	// Repair never fires before the policy toleration elapses.
	It("should not repair before the toleration duration elapses", func() {
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(10 * time.Minute) // still within the 30m toleration

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	It("should use a matching reason-specific policy instead of the fallback", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
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
		})
		newRepairController()
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "FastFailure")
		env.Clock.Step(11 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	It("should preserve condition age across a reason-only change", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
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
		})
		newRepairController()
		initNode(nodeClaim, node)
		env.Clock.Step(time.Hour)

		storedNode := ExpectExists(ctx, env.Client, node)
		storedNode.Status.Conditions = append(storedNode.Status.Conditions, corev1.NodeCondition{
			Type:               "BadNode",
			Status:             corev1.ConditionFalse,
			Reason:             "UnknownReason",
			LastTransitionTime: storedNode.CreationTimestamp,
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
		useRepairPolicies([]cloudprovider.RepairPolicy{
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
		})
		newRepairController()
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "PrefixFastFailureSuffix")
		env.Clock.Step(10 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	It("should use the condition fallback for an unknown reason", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
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
		})
		newRepairController()
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "UnknownFailure")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(1))
	})

	// do-not-repair blocks repair; do-not-disrupt does NOT (no behavior change for that annotation).
	It("should not repair a node carrying the do-not-repair annotation", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
		expectBlocked(nodePool, disruption.RepairBlockedDoNotRepair, 1)
		// The veto is reported on both the Node and the NodeClaim, and again on the next pass, so it stays visible.
		vetoEvents := lo.Filter(recorder.Events(), func(e karpenterevents.Event, _ int) bool {
			return e.Reason == karpenterevents.DisruptionBlocked && e.Message == `repair is blocked through the "karpenter.sh/do-not-repair" annotation`
		})
		Expect(lo.Map(vetoEvents, func(e karpenterevents.Event, _ int) string {
			return string(e.InvolvedObject.(metav1.Object).GetUID())
		})).To(ConsistOf(string(node.UID), string(nodeClaim.UID)))
		recorder.Reset()
		ExpectSingletonReconciled(ctx, repairController)
		Expect(recorder.DetectedEvent(`repair is blocked through the "karpenter.sh/do-not-repair" annotation`)).To(BeTrue())
	})

	It("should not reboot a node carrying the do-not-repair annotation", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "RebootMe", TolerationDuration: 10 * time.Minute, Action: cloudprovider.RebootNode},
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		})
		newRepairController()
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
		initNode(nodeClaim, node)
		markUnhealthyWithReason(node, "BadNode", "RebootMe")
		env.Clock.Step(11 * time.Minute) // only the reboot policy is eligible

		ExpectSingletonReconciled(ctx, repairController)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeRebooting).IsTrue()).To(BeFalse())
		Expect(recorder.DetectedEvent(`repair is blocked through the "karpenter.sh/do-not-repair" annotation`)).To(BeTrue())
	})

	It("should report the do-not-repair veto ahead of an unbounded drain", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
		initNode(nodeClaim, node)
		bindBlockingPod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
		Expect(recorder.DetectedEvent(`repair is blocked through the "karpenter.sh/do-not-repair" annotation`)).To(BeTrue())
		Expect(recorder.DetectedEvent("repair requires a termination grace period to bypass blocking pods")).To(BeFalse())
		// The veto comes first, so the node counts under do-not-repair, not blocking-pods.
		expectHeldBack(node, disruption.RepairBlockedDoNotRepair)
		expectBlocked(nodePool, disruption.RepairBlockedDoNotRepair, 1)
	})

	It("should not report the do-not-repair veto while the node is still within its policy's toleration", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(29 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
		Expect(recorder.Calls(karpenterevents.DisruptionBlocked)).To(Equal(0))
		expectNoneBlocked(nodePool)
	})

	It("should still repair a node carrying only the do-not-disrupt annotation", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		// do-not-disrupt does not block repair: a command is still produced.
		Expect(queue.GetCommands()).To(HaveLen(1))
		expectNotHeldBack(node)
		expectNoneBlocked(nodePool)
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

	It("should not repair when an Unhealthy disruption budget is zero", func() {
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0", Reasons: []v1.DisruptionReason{v1.DisruptionReasonUnhealthy}}}
		ExpectApplied(ctx, env.Client, nodePool)
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(BeEmpty())
		expectHeldBack(node, disruption.RepairBlockedBudget)
		expectBlocked(nodePool, disruption.RepairBlockedBudget, 1)
	})

	It("should repair when a disruption budget without reasons is zero", func() {
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0"}}
		ExpectApplied(ctx, env.Client, nodePool)
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(HaveLen(1))
		expectNoneBlocked(nodePool)
	})

	It("should repair with the full policy decision once a blocking budget lifts", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(5 * time.Minute), Action: cloudprovider.ReplaceNode},
		})
		newRepairController()
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0", Reasons: []v1.DisruptionReason{v1.DisruptionReasonUnhealthy}}}
		ExpectApplied(ctx, env.Client, nodePool)
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(BeEmpty())
		expectHeldBack(node, disruption.RepairBlockedBudget)
		expectBlocked(nodePool, disruption.RepairBlockedBudget, 1)

		// The second pass reaches the same decision, which must still carry the policy's action and drain bound.
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		expectNotHeldBack(node)
		expectNoneBlocked(nodePool)
		Expect(*cmds[0].Candidates[0].TerminationGracePeriod).To(Equal(5 * time.Minute))
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
		expectHeldBack(nodes[0], disruption.RepairBlockedBreaker)
		// The other two are still within their toleration, so they aren't repair candidates and get no reason.
		expectNotHeldBack(nodes[1], nodes[2])
		expectBlocked(nodePool, disruption.RepairBlockedBreaker, 1)
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

	It("should count a node toward the breaker for any policy's condition, whatever its reason", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
			{ConditionType: "KernelFault", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Transient$", TolerationDuration: 30 * time.Minute, Action: cloudprovider.RebootNode},
		})
		newRepairController()
		const count = 10
		nodeClaims, nodes := test.NodeClaimsAndNodes(count, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		markUnhealthy(nodes[0], "BadNode")
		env.Clock.Step(31 * time.Minute)
		// The legacy controller wouldn't count these, since it ignores reboot policies.
		markUnhealthyWithReason(nodes[1], "KernelFault", "Other")
		markUnhealthyWithReason(nodes[2], "KernelFault", "Other")

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(BeEmpty())
		blockedEvents := lo.Filter(recorder.Events(), func(event karpenterevents.Event, _ int) bool {
			return event.Reason == karpenterevents.NodeRepairBlocked
		})
		Expect(lo.Map(blockedEvents, func(event karpenterevents.Event, _ int) string {
			return string(event.InvolvedObject.(metav1.Object).GetUID())
		})).To(ConsistOf(string(nodes[0].UID), string(nodeClaims[0].UID), string(nodePool.UID)))
	})

	It("should count nodes that are still registering toward the breaker, though the breaker metrics leave them out", func() {
		const count = 5
		nodeClaims, nodes := test.NodeClaimsAndNodes(count, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		// 5 nodes allow 1 unhealthy.
		markUnhealthy(nodes[0], "BadNode")
		env.Clock.Step(31 * time.Minute)
		// Once cluster state has synced, a node it leaves out doesn't hold up the disruption controller.
		Expect(cluster.Synced(ctx)).To(BeTrue())
		// Cluster state leaves out these two unhealthy nodes while they register. One has no provider ID, and the other
		// isn't initialized and has no instance type label. Repair lists and counts them, so 3 of 7 trips its breaker.
		for _, providerID := range []string{"", test.RandomProviderID()} {
			registering := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Labels: labels()}, ProviderID: providerID})
			ExpectApplied(ctx, env.Client, registering)
			markUnhealthy(registering, "BadNode")
		}
		// The breaker metrics count from cluster state, which has only the 5 initialized nodes.
		Expect(cluster.RepairBreakers()[nodePool.Name]).To(Equal(health.Breaker{Nodes: 5, Unhealthy: 1}))

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(BeEmpty())
		blockedEvents := lo.Filter(recorder.Events(), func(event karpenterevents.Event, _ int) bool {
			return event.Reason == karpenterevents.NodeRepairBlocked
		})
		Expect(lo.Map(blockedEvents, func(event karpenterevents.Event, _ int) string {
			return string(event.InvolvedObject.(metav1.Object).GetUID())
		})).To(ConsistOf(string(nodes[0].UID), string(nodeClaims[0].UID), string(nodePool.UID)))
		// The gauge follows repair's own breaker check, not the breaker metrics.
		expectBlocked(nodePool, disruption.RepairBlockedBreaker, 1)
	})

	It("should stop repair only in the NodePool whose breaker trips", func() {
		other := test.NodePool()
		ExpectApplied(ctx, env.Client, other)
		nodeClaims, nodes := test.NodeClaimsAndNodes(10, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		otherNodeClaims, otherNodes := test.NodeClaimsAndNodes(5, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: lo.Assign(labels(), map[string]string{v1.NodePoolLabelKey: other.Name}),
		}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		for i := range otherNodes {
			initNode(otherNodeClaims[i], otherNodes[i])
		}
		// 3 of 10 trips nodePool's breaker, and 1 of 5 is within other's limit.
		for i := range 3 {
			markUnhealthy(nodes[i], "BadNode")
		}
		markUnhealthy(otherNodes[0], "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Candidates[0].Name()).To(Equal(otherNodes[0].Name))
		blockedEvents := lo.Filter(recorder.Events(), func(event karpenterevents.Event, _ int) bool {
			return event.Reason == karpenterevents.NodeRepairBlocked
		})
		blockedUIDs := lo.Map(blockedEvents, func(event karpenterevents.Event, _ int) string {
			return string(event.InvolvedObject.(metav1.Object).GetUID())
		})
		Expect(blockedUIDs).To(ContainElement(string(nodePool.UID)))
		Expect(blockedUIDs).ToNot(ContainElement(string(other.UID)))
	})

	It("should log a NodePool that the breaker blocks once, not on every pass", func() {
		logCtx, blockedLogs := CaptureLogs(ctx, "blocking node repair")
		const count = 10
		nodeClaims, nodes := test.NodeClaimsAndNodes(count, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		for i := range 3 {
			markUnhealthy(nodes[i], "BadNode")
		}
		env.Clock.Step(31 * time.Minute)

		// Every pass blocks all three nodes, but the NodePool is logged once.
		ExpectSingletonReconciled(logCtx, repairController)
		ExpectSingletonReconciled(logCtx, repairController)
		Expect(queue.GetCommands()).To(BeEmpty())
		Expect(blockedLogs()).To(HaveLen(1))
		Expect(blockedLogs()[0]).To(ContainSubstring(nodePool.Name))
		Expect(blockedLogs()[0]).To(ContainSubstring(`"nodes"=10 "unhealthy-nodes"=3`))

		env.Clock.Step(health.BlockedRepeatInterval)
		ExpectSingletonReconciled(logCtx, repairController)
		Expect(blockedLogs()).To(HaveLen(2))
	})

	// Ordering is prioritizable — a higher-priority fault repairs before a lower-priority one in the same pass.
	It("should repair the higher-priority condition first", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10, Action: cloudprovider.ReplaceNode},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 90, Action: cloudprovider.ReplaceNode},
		})
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

	// Two nodes that broke at the same moment have equal repair scores, so repair should pick the one whose name sorts
	// first. Repair checks each node at a slightly different time, so it must score both at one time before choosing.
	// Otherwise the node checked last would look more overdue and win.
	It("should break a tie by node name between candidates resolved at different times", func() {
		// Repair stops when more than 20% of a pool's nodes are unhealthy, so break only 2 of 10 nodes.
		nodeClaims, nodes := test.NodeClaimsAndNodes(10, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
		}
		// Break both nodes at the same moment, after all nodes exist, so they're equally overdue.
		env.Clock.SetTime(time.Now())
		markUnhealthy(nodes[0], "BadNode")
		markUnhealthy(nodes[1], "BadNode")
		// Wait past the 30 minute toleration, so repair can act on both.
		env.Clock.Step(31 * time.Minute)

		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, repair.ShouldDisrupt, disruption.RepairDisruptionClass, queue)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(2))
		// candidates[0] has the name that sorts first, so it should win the tie.
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name() < candidates[j].Name() })
		// Check the winner, then the other node one second later. If repair kept the score from when each node was
		// checked, the node checked later would look more overdue and win.
		Expect(repair.ShouldDisrupt(ctx, candidates[0])).To(BeTrue())
		env.Clock.Step(time.Second)
		Expect(repair.ShouldDisrupt(ctx, candidates[1])).To(BeTrue())

		// Pass the other node first, so repair has to compare the two to pick the winner.
		cmds, err := repair.ComputeCommands(ctx, map[string]int{nodePool.Name: 1}, candidates[1], candidates[0])
		Expect(err).NotTo(HaveOccurred())
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Candidates[0].Name()).To(Equal(candidates[0].Name()))
	})

	// Repair breaks score ties on disruption cost, and a node's cost depends on how much of its lifetime is left. So
	// every node's cost must be worked out as of the same time. Otherwise two identical nodes would get slightly
	// different costs, and the node checked last would look cheaper and win.
	It("should build every candidate of a pass as of one time", func() {
		// Give the nodes a lifetime (expireAfter), so their cost changes as time passes.
		nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}, Spec: v1.NodeClaimSpec{ExpireAfter: v1.MustParseNillableDuration("720h")}})
		for i := range nodes {
			initNode(nodeClaims[i], nodes[i])
			// A pod gives the node a cost above zero. With no pods, both costs would be zero whatever the time.
			bindReschedulablePod(nodes[i])
			markUnhealthy(nodes[i], "BadNode")
		}
		// Wait past the 30 minute toleration, so repair can act on both.
		env.Clock.Step(31 * time.Minute)
		// Make both nodes the same age, so at any one moment they have the same time left and the same cost.
		stored := ExpectExists(ctx, env.Client, nodeClaims[1])
		stored.CreationTimestamp = ExpectExists(ctx, env.Client, nodeClaims[0]).CreationTimestamp
		cluster.UpdateNodeClaim(stored)
		// Use a clock that moves forward every time it's read, like a real clock does.
		clk := tickingClock{FakeClock: clocktesting.NewFakeClock(env.Clock.Now())}

		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, clk, cloudProvider, repair.ShouldDisrupt, disruption.RepairDisruptionClass, queue)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(2))
		// Both nodes must get the same cost, even though the clock moved on between them.
		Expect(candidates[0].DisruptionCost).To(BeNumerically(">", 0))
		Expect(candidates[0].DisruptionCost).To(Equal(candidates[1].DisruptionCost))
	})

	It("should not score a node using a reason-specific policy that does not match", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Critical$", TolerationDuration: 30 * time.Minute, Priority: 90, Action: cloudprovider.ReplaceNode},
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10, Action: cloudprovider.ReplaceNode},
			{ConditionType: "MediumPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 50, Action: cloudprovider.ReplaceNode},
		})
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
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^failure$", TolerationDuration: time.Hour, Priority: 100, Action: cloudprovider.ReplaceNode},
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "failure", Priority: 0, Action: cloudprovider.ReplaceNode},
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, Priority: 0, Action: cloudprovider.ReplaceNode},
			{ConditionType: "MediumPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", Priority: 50, Action: cloudprovider.ReplaceNode},
		})
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
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Critical$", TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(time.Duration(0)), Priority: 90, Action: cloudprovider.ReplaceNode},
			{ConditionType: "Diagnostic", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: &diagnosticGracePeriod, Priority: 10, Action: cloudprovider.ReplaceNode},
			{ConditionType: "MediumPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, TerminationGracePeriod: &mediumGracePeriod, Priority: 50, Action: cloudprovider.ReplaceNode},
		})
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

	// A node's score is the argmax of rank+age/τ over ALL its matching conditions, not the score of its
	// highest-priority condition. Node A has a fresh high-priority condition (just past toleration) and a long-starving
	// low-priority one; node B has a moderately-aged high-priority condition. Scoring A off only its high-priority
	// (fresh) condition would rank it below B and repair B first; the argmax lifts A above B on its starving condition.
	It("should order a node by its most urgent condition, not just its highest-priority one", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10, Action: cloudprovider.ReplaceNode},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 90, Action: cloudprovider.ReplaceNode},
		})
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

	// After any required replacements are ready, the drain deadline is stamped immediately before the delete
	// request (not at command-computation time), so replacement-launch latency can't erode the window. A forceful (0)
	// policy stamps an immediate deadline, so repair is never the unbounded hang.
	It("should stamp a forceful (immediate) termination deadline before requesting deletion", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(time.Duration(0)), Action: cloudprovider.ReplaceNode},
		})
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

	It("should preserve an existing earlier termination deadline", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(10 * time.Minute), Action: cloudprovider.ReplaceNode},
		})
		newRepairController()
		nodeClaim.Finalizers = append(nodeClaim.Finalizers, "karpenter.sh/test-finalizer")
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])

		deadline := env.Clock.Now().Add(5 * time.Minute)
		current := ExpectExists(ctx, env.Client, nodeClaim)
		current.Annotations = lo.Assign(current.Annotations, map[string]string{
			v1.NodeClaimTerminationTimestampAnnotationKey: deadline.Format(time.RFC3339),
		})
		Expect(env.Client.Update(ctx, current)).To(Succeed())

		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).To(
			HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, deadline.Format(time.RFC3339)))
	})

	It("should retry a failed termination-deadline patch without losing the deadline", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(time.Duration(0)), Action: cloudprovider.ReplaceNode},
		})
		injectedErr := errors.New("injected termination timestamp patch failure")
		failingClient := &terminationTimestampPatchErrorClient{Client: env.Client, err: injectedErr, failNext: true}
		failingQueue := disruption.NewQueue(failingClient, recorder, cluster, env.Clock, prov, nodePoolBackoff)
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

	// When both the policy and the NodeClaim bound the drain, the smaller (most forceful) wins.
	It("should stamp min(policy TGP, NodeClaim TGP) before requesting deletion", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(20 * time.Minute), Action: cloudprovider.ReplaceNode},
		})
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
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "LowPriority", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Priority: 10, TerminationGracePeriod: lo.ToPtr(15 * time.Minute), Action: cloudprovider.ReplaceNode},
			{ConditionType: "HighPriority", ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Priority: 90, TerminationGracePeriod: lo.ToPtr(5 * time.Minute), Action: cloudprovider.ReplaceNode},
		})
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
	It("should neither terminate nor stamp a deadline when the replacement never becomes healthy", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(10 * time.Minute), Action: cloudprovider.ReplaceNode},
		})
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
		expectHeldBack(node, disruption.RepairBlockedBlockingPods)
		expectBlocked(nodePool, disruption.RepairBlockedBlockingPods, 1)
	})

	It("should bypass a blocking pod when only the NodeClaim bounds the drain, without stamping a deadline", func() {
		nodeClaim.Spec.TerminationGracePeriod = &metav1.Duration{Duration: 5 * time.Minute}
		nodeClaim.Finalizers = append(nodeClaim.Finalizers, "karpenter.sh/test-finalizer") // survive Delete so we can read the annotations
		initNode(nodeClaim, node)
		bindBlockingPod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(recorder.DetectedEvent("repair requires a termination grace period to bypass blocking pods")).To(BeFalse())
		// The policy sets no bound, so the queue stamps no deadline.
		Expect(cmds[0].Candidates[0].TerminationGracePeriod).To(BeNil())
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
		ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
		current := ExpectExists(ctx, env.Client, nodeClaim)
		Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(current.Annotations).ToNot(HaveKey(v1.NodeClaimTerminationTimestampAnnotationKey))
	})

	It("should bypass a blocking pod when a forceful policy bounds the drain at zero", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(time.Duration(0)), Action: cloudprovider.ReplaceNode},
		})
		newRepairController()
		initNode(nodeClaim, node)
		bindBlockingPod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(recorder.DetectedEvent("repair requires a termination grace period to bypass blocking pods")).To(BeFalse())
		Expect(cmds[0].Candidates[0].TerminationGracePeriod).To(HaveValue(BeZero()))
	})

	It("should size replacement capacity for a blocking pod when the drain is bounded", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{
				ConditionType:          "BadNode",
				ConditionStatus:        corev1.ConditionFalse,
				TolerationDuration:     30 * time.Minute,
				TerminationGracePeriod: lo.ToPtr(5 * time.Minute),
				Action:                 cloudprovider.ReplaceNode,
			},
		})
		newRepairController()
		initNode(nodeClaim, node)
		blockingPod := bindBlockingPod(node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Decision()).To(Equal(disruption.ReplaceDecision))
		Expect(cmds[0].Replacements).To(HaveLen(1))
		Expect(cmds[0].Results.NewNodeClaims).To(HaveLen(1))
		Expect(lo.Map(cmds[0].Results.NewNodeClaims[0].Pods, func(p *corev1.Pod, _ int) string {
			return p.Name
		})).To(ContainElement(blockingPod.Name))
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
		expectHeldBack(nodes[0], disruption.RepairBlockedNoReplacement)
		expectBlocked(nodePool, disruption.RepairBlockedNoReplacement, 1)
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

	It("should propagate replacement simulation errors", func() {
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)
		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, repair.ShouldDisrupt, disruption.RepairDisruptionClass, queue)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))

		injectedErr := errors.New("injected pending pod list failure")
		failingClient := &listErrorClient{Client: env.Client, failList: &corev1.PodList{}, err: injectedErr}
		failingProvisioner := provisioning.NewProvisioner(
			failingClient,
			recorder,
			cloudProvider,
			cluster,
			env.Clock,
			deviceallocation.NewController(failingClient),
			virtualpods.NewVirtualPodCache(failingClient),
			store,
		)
		failingRepair := disruption.NewRepair(disruption.MakeConsolidation(
			env.Clock,
			cluster,
			failingClient,
			failingProvisioner,
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
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: new(false)}}))
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	It("should not repair when --legacy-node-repair is set", func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{LegacyNodeRepair: new(true), FeatureGates: test.FeatureGates{NodeRepair: new(true)}}))
		initNode(nodeClaim, node)
		markUnhealthy(node, "BadNode")
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	Context("uninitialized nodes", func() {
		// registerNode applies a node/nodeclaim that registered but never initialized and syncs cluster state. Cluster
		// state skips an uninitialized node until registration syncs its instance type label.
		registerNode := func(nc *v1.NodeClaim, n *corev1.Node) {
			n.Labels = lo.Assign(n.Labels, map[string]string{v1.NodeRegisteredLabelKey: "true", corev1.LabelInstanceTypeStable: "default-instance-type"})
			nc.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
			nc.StatusConditions().SetTrue(v1.ConditionTypeRegistered)
			ExpectApplied(ctx, env.Client, nc, n)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nc))
		}

		It("should repair an unhealthy node that never initialized", func() {
			registerNode(nodeClaim, node)
			markUnhealthy(node, "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.DeleteDecision))

			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)
			ExpectNotFound(ctx, env.Client, nodeClaim)
			ExpectMetricCounterValue(disruption.NodeClaimsUnhealthyDisruptedTotal, 1, map[string]string{
				"condition":           "bad_node",
				metrics.NodePoolLabel: nodePool.Name,
			})
		})

		It("should not repair a node that never registered", func() {
			node.Labels = lo.Assign(node.Labels, map[string]string{corev1.LabelInstanceTypeStable: "default-instance-type"})
			nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
			ExpectApplied(ctx, env.Client, nodeClaim, node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))
			markUnhealthy(node, "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(BeEmpty())
			Expect(recorder.DetectedEvent("Node isn't initialized nor registered")).To(BeTrue())
		})

		It("should not repair before the toleration duration elapses", func() {
			registerNode(nodeClaim, node)
			markUnhealthy(node, "BadNode")
			env.Clock.Step(29 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(BeEmpty())
		})

		It("should repair without disruption budget", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0", Reasons: []v1.DisruptionReason{v1.DisruptionReasonUnhealthy}}}
			ExpectApplied(ctx, env.Client, nodePool)
			registerNode(nodeClaim, node)
			markUnhealthy(node, "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(HaveLen(1))
			expectNotHeldBack(node)
			expectNoneBlocked(nodePool)
		})

		It("should not consume disruption budget for initialized nodes", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "1", Reasons: []v1.DisruptionReason{v1.DisruptionReasonUnhealthy}}}
			ExpectApplied(ctx, env.Client, nodePool)
			registerNode(nodeClaim, node)
			markUnhealthy(node, "BadNode")
			env.Clock.Step(31 * time.Minute)
			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(HaveLen(1))

			// Five healthy nodes keep the second repair under the 20% circuit breaker.
			nodeClaims, nodes := test.NodeClaimsAndNodes(5, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
			for i := range nodes {
				initNode(nodeClaims[i], nodes[i])
			}
			markUnhealthy(nodes[0], "BadNode")
			env.Clock.Step(31 * time.Minute)
			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(HaveLen(2))
		})

		It("should measure the toleration from when the last reboot finished", func() {
			registerNode(nodeClaim, node)
			markUnhealthy(node, "BadNode")
			env.Clock.Step(40 * time.Minute)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			nodeClaim.StatusConditions(status.WithClock(env.Clock)).SetFalse(v1.ConditionTypeRebooting, v1.RebootReasonSucceeded, "node rebooted and rejoined the cluster")
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))

			env.Clock.Step(29 * time.Minute)
			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(BeEmpty())

			env.Clock.Step(2 * time.Minute)
			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(HaveLen(1))
		})

		It("should not repair a rebooting node", func() {
			registerNode(nodeClaim, node)
			markUnhealthy(node, "BadNode")
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonIssued, "rebooting")
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(BeEmpty())
		})

		It("should repair a node nominated for pending pods", func() {
			registerNode(nodeClaim, node)
			markUnhealthy(node, "BadNode")
			env.Clock.Step(31 * time.Minute)
			cluster.NominateNodeForPod(ctx, node.Spec.ProviderID)

			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(HaveLen(1))
		})

		It("should not repair a node carrying the do-not-repair annotation", func() {
			node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
			registerNode(nodeClaim, node)
			markUnhealthy(node, "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(BeEmpty())
		})

		It("should stop repairing a NodePool when more than 20% of its nodes are unhealthy", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
			for i := range nodes {
				registerNode(nodeClaims[i], nodes[i])
			}
			markUnhealthy(nodes[0], "BadNode")
			markUnhealthy(nodes[1], "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(BeEmpty())
			expectHeldBack(nodes[0], disruption.RepairBlockedBreaker)
			expectHeldBack(nodes[1], disruption.RepairBlockedBreaker)
			expectBlocked(nodePool, disruption.RepairBlockedBreaker, 2)
		})

		// registerRoomyNode registers a node that never initializes but has room for the candidate's pods.
		registerRoomyNode := func() {
			nc, n := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: labels()},
				Status: v1.NodeClaimStatus{
					Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("32"), corev1.ResourcePods: resource.MustParse("100")},
				},
			})
			registerNode(nc, n)
		}

		It("should give waiting-on-initialization when the node's pods would move to a node that is still initializing", func() {
			initNode(nodeClaim, node)
			bindReschedulablePod(node)
			registerRoomyNode()
			markUnhealthy(node, "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(BeEmpty())
			_, waited := lo.Find(recorder.Events(), func(e karpenterevents.Event) bool {
				return strings.Contains(e.Message, "would schedule against uninitialized nodeclaim")
			})
			Expect(waited).To(BeTrue())
			expectHeldBack(node, disruption.RepairBlockedWaitingOnInitialization)
			expectBlocked(nodePool, disruption.RepairBlockedWaitingOnInitialization, 1)
		})

		It("should still give waiting-on-initialization when an unrelated pending pod can't be scheduled", func() {
			initNode(nodeClaim, node)
			bindReschedulablePod(node)
			registerRoomyNode()
			// The pending pod isn't on the candidate node, so it can't change the reason.
			ExpectApplied(ctx, env.Client, test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{corev1.LabelTopologyZone: "test-zone-missing"}}))
			markUnhealthy(node, "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(BeEmpty())
			expectHeldBack(node, disruption.RepairBlockedWaitingOnInitialization)
			expectBlocked(nodePool, disruption.RepairBlockedWaitingOnInitialization, 1)
		})

		It("should give no-replacement when one pod can't be scheduled anywhere, even if others wait for an initializing node", func() {
			initNode(nodeClaim, node)
			bindReschedulablePod(node)
			// No node can take a pod pinned to a zone that doesn't exist, so waiting alone doesn't explain the block.
			bindReschedulablePod(node, test.PodOptions{NodeSelector: map[string]string{corev1.LabelTopologyZone: "test-zone-missing"}})
			registerRoomyNode()
			markUnhealthy(node, "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			Expect(queue.GetCommands()).To(BeEmpty())
			expectHeldBack(node, disruption.RepairBlockedNoReplacement)
			expectBlocked(nodePool, disruption.RepairBlockedNoReplacement, 1)
		})
	})

	Context("held-back reasons", func() {
		// holdBackOnBudget makes the suite's node unhealthy past its toleration in a nodepool whose budget allows no
		// repairs, and checks that a run of dc gives it the budget reason and counts it.
		holdBackOnBudget := func(dc *disruption.Controller) {
			GinkgoHelper()
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0", Reasons: []v1.DisruptionReason{v1.DisruptionReasonUnhealthy}}}
			ExpectApplied(ctx, env.Client, nodePool)
			initNode(nodeClaim, node)
			markUnhealthy(node, "BadNode")
			env.Clock.Step(31 * time.Minute)
			ExpectSingletonReconciled(ctx, dc)
			expectHeldBack(node, disruption.RepairBlockedBudget)
			expectBlocked(nodePool, disruption.RepairBlockedBudget, 1)
		}

		It("should give the breaker reason when the nodepool is also out of budget", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0", Reasons: []v1.DisruptionReason{v1.DisruptionReasonUnhealthy}}}
			ExpectApplied(ctx, env.Client, nodePool)
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
			for i := range nodes {
				initNode(nodeClaims[i], nodes[i])
			}
			// Two unhealthy nodes out of three trip the 20% breaker.
			markUnhealthy(nodes[0], "BadNode")
			markUnhealthy(nodes[1], "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			// Repair's budget is zero too, so the breaker reason shows the breaker is checked first.
			ExpectMetricGaugeValue(disruption.NodePoolAllowedDisruptions, 0, map[string]string{
				metrics.NodePoolLabel: nodePool.Name,
				metrics.ReasonLabel:   strings.ToLower(string(v1.DisruptionReasonUnhealthy)),
			})
			expectHeldBack(nodes[0], disruption.RepairBlockedBreaker)
			expectHeldBack(nodes[1], disruption.RepairBlockedBreaker)
			expectNotHeldBack(nodes[2])
			expectBlocked(nodePool, disruption.RepairBlockedBreaker, 2)
		})

		It("should count a vetoed node under do-not-repair even when its nodepool's breaker is tripped", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
			nodes[0].Annotations = lo.Assign(nodes[0].Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
			for i := range nodes {
				initNode(nodeClaims[i], nodes[i])
			}
			// Two unhealthy nodes out of three trip the 20% breaker.
			markUnhealthy(nodes[0], "BadNode")
			markUnhealthy(nodes[1], "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			expectHeldBack(nodes[0], disruption.RepairBlockedDoNotRepair)
			expectHeldBack(nodes[1], disruption.RepairBlockedBreaker)
			expectBlocked(nodePool, disruption.RepairBlockedDoNotRepair, 1)
			expectBlocked(nodePool, disruption.RepairBlockedBreaker, 1)
		})

		It("should give breaker and budget reasons even when repair acts on another nodepool first", func() {
			otherNodePool := test.NodePool()
			budgetNodePool := test.NodePool()
			budgetNodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0", Reasons: []v1.DisruptionReason{v1.DisruptionReasonUnhealthy}}}
			ExpectApplied(ctx, env.Client, otherNodePool, budgetNodePool)
			otherNodeClaim, otherNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(otherNodePool)}})
			initNode(otherNodeClaim, otherNode)
			// The other node has been unhealthy longer, so it scores higher and repair acts on it first.
			markUnhealthy(otherNode, "BadNode")
			env.Clock.Step(10 * time.Minute)
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
			for i := range nodes {
				initNode(nodeClaims[i], nodes[i])
			}
			markUnhealthy(nodes[0], "BadNode")
			markUnhealthy(nodes[1], "BadNode")
			budgetNodeClaim, budgetNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(budgetNodePool)}})
			initNode(budgetNodeClaim, budgetNode)
			markUnhealthy(budgetNode, "BadNode")
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repairController)
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Candidates[0].NodePool.Name).To(Equal(otherNodePool.Name))
			expectHeldBack(nodes[0], disruption.RepairBlockedBreaker)
			expectHeldBack(nodes[1], disruption.RepairBlockedBreaker)
			expectHeldBack(budgetNode, disruption.RepairBlockedBudget)
			expectNotHeldBack(otherNode, nodes[2])
			expectBlocked(nodePool, disruption.RepairBlockedBreaker, 2)
			expectBlocked(budgetNodePool, disruption.RepairBlockedBudget, 1)
			expectNoneBlocked(otherNodePool)
			// Repair checks every candidate against the breaker first, so nodes ranked after the one it acts on get the event.
			breakerEvents := lo.Filter(recorder.Events(), func(e karpenterevents.Event, _ int) bool { return e.Reason == karpenterevents.NodeRepairBlocked })
			Expect(lo.Map(breakerEvents, func(e karpenterevents.Event, _ int) string {
				return string(e.InvolvedObject.(metav1.Object).GetUID())
			})).To(ContainElements(string(nodes[0].UID), string(nodes[1].UID)))
		})

		It("should drop a node's reason once it recovers", func() {
			holdBackOnBudget(repairController)

			markHealthy(node, "BadNode")
			ExpectSingletonReconciled(ctx, repairController)
			expectNotHeldBack(node)
			expectNoneBlocked(nodePool)
		})

		It("should give a node its new reason when another reason now holds it back", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0", Reasons: []v1.DisruptionReason{v1.DisruptionReasonUnhealthy}}}
			ExpectApplied(ctx, env.Client, nodePool)
			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
			for i := range nodes {
				initNode(nodeClaims[i], nodes[i])
			}
			// One unhealthy node of three is within the breaker's limit, so the budget holds it back.
			markUnhealthy(nodes[0], "BadNode")
			env.Clock.Step(31 * time.Minute)
			ExpectSingletonReconciled(ctx, repairController)
			expectHeldBack(nodes[0], disruption.RepairBlockedBudget)
			expectBlocked(nodePool, disruption.RepairBlockedBudget, 1)

			// A second unhealthy node trips the breaker, which now holds the first node back instead.
			markUnhealthy(nodes[1], "BadNode")
			ExpectSingletonReconciled(ctx, repairController)
			expectHeldBack(nodes[0], disruption.RepairBlockedBreaker)
			expectBlocked(nodePool, disruption.RepairBlockedBreaker, 1)
			Expect(blockedSeriesFound(blockedLabels(nodePool, disruption.RepairBlockedBudget))).To(BeFalse())
		})

		It("should give reasons only for the nodes a failed repair pass checked", func() {
			// The blocked nodes get their own nodepool, so they don't trip the breaker of the node the budget holds back.
			blockedNodePool := test.NodePool()
			ExpectApplied(ctx, env.Client, blockedNodePool)
			addNodeWithBlockingPod := func() *corev1.Node {
				nc, n := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(blockedNodePool)}})
				initNode(nc, n)
				bindBlockingPod(n)
				markUnhealthy(n, "BadNode")
				return n
			}
			failingClient := &listErrorClient{Client: env.Client}
			_, failingController := newRepairControllerWith(failingClient)
			first := addNodeWithBlockingPod()
			holdBackOnBudget(failingController)
			expectHeldBack(first, disruption.RepairBlockedBlockingPods)
			expectBlocked(blockedNodePool, disruption.RepairBlockedBlockingPods, 1)

			// The second node is new, so its reason can only come from the failed pass below.
			second := addNodeWithBlockingPod()
			env.Clock.Step(31 * time.Minute)
			// Repair lists Nodes for the breaker after it checks blocking pods and before it checks budgets, so failing that
			// list stops the pass between the two.
			injectedErr := errors.New("injected list failure")
			failingClient.failList, failingClient.err = &corev1.NodeList{}, injectedErr
			err := ExpectSingletonReconcileFailed(ctx, failingController)
			Expect(err).To(MatchError(injectedErr))
			Expect(err).To(MatchError(ContainSubstring("computing disruption decision")))
			expectHeldBack(first, disruption.RepairBlockedBlockingPods)
			expectHeldBack(second, disruption.RepairBlockedBlockingPods)
			expectNotHeldBack(node)
			expectBlocked(blockedNodePool, disruption.RepairBlockedBlockingPods, 2)
			expectNoneBlocked(nodePool)

			// NodePool lists fail now, so the pass fails while gathering candidates and gives no reasons at all.
			failingClient.failList = &v1.NodePoolList{}
			err = ExpectSingletonReconcileFailed(ctx, failingController)
			Expect(err).To(MatchError(injectedErr))
			Expect(err).To(MatchError(ContainSubstring("determining candidates")))
			Expect(cluster.RepairBlockedReasons()).To(BeEmpty())
			expectNoneBlocked(nodePool)
			expectNoneBlocked(blockedNodePool)
		})

		It("should drop a node's budget reason once the budget lifts, even when the pass then fails", func() {
			holdBackOnBudget(repairController)

			// With the budget lifted and the nodepool NotReady, the replacement simulation finds no nodepools and the pass
			// fails. The budget no longer holds the node back, so its reason is dropped.
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
			nodePool.StatusConditions().SetFalse(v1.ConditionTypeValidationSucceeded, "NotReady", "NotReady")
			ExpectApplied(ctx, env.Client, nodePool)
			err := ExpectSingletonReconcileFailed(ctx, repairController)
			Expect(err).To(MatchError(provisioning.ErrNodePoolsNotFound))
			Expect(err).To(MatchError(ContainSubstring("Node=" + node.Name)))
			expectNotHeldBack(node)
			expectNoneBlocked(nodePool)
		})

		It("should keep the reasons given before a later candidate fails the pass", func() {
			// A static nodepool at its node limit gives its node no-replacement, without a simulation.
			staticNodePool := test.StaticNodePool(v1.NodePool{Spec: v1.NodePoolSpec{
				Replicas: lo.ToPtr[int64](1),
				Limits:   v1.Limits{resources.Node: resource.MustParse("1")},
			}})
			budgetNodePool := test.NodePool()
			budgetNodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0", Reasons: []v1.DisruptionReason{v1.DisruptionReasonUnhealthy}}}
			breakerNodePool := test.NodePool()
			// Every dynamic nodepool is NotReady, so the replacement simulation for nodePool's node finds no nodepools and the
			// pass fails.
			for _, np := range []*v1.NodePool{nodePool, budgetNodePool, breakerNodePool} {
				np.StatusConditions().SetFalse(v1.ConditionTypeValidationSucceeded, "NotReady", "NotReady")
			}
			ExpectApplied(ctx, env.Client, staticNodePool, nodePool, budgetNodePool, breakerNodePool)
			staticNodeClaim, staticNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(staticNodePool)}})
			budgetNodeClaim, budgetNode := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(budgetNodePool)}})
			breakerNodeClaims, breakerNodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(breakerNodePool)}})
			initNode(staticNodeClaim, staticNode)
			initNode(budgetNodeClaim, budgetNode)
			initNode(nodeClaim, node)
			for i := range breakerNodes {
				initNode(breakerNodeClaims[i], breakerNodes[i])
			}
			// The static node has been unhealthy longest, so repair checks it before nodePool's node.
			markUnhealthy(staticNode, "BadNode")
			env.Clock.Step(10 * time.Minute)
			markUnhealthy(node, "BadNode")
			markUnhealthy(budgetNode, "BadNode")
			// Two unhealthy nodes out of three trip the 20% breaker.
			markUnhealthy(breakerNodes[0], "BadNode")
			markUnhealthy(breakerNodes[1], "BadNode")
			env.Clock.Step(31 * time.Minute)

			Expect(ExpectSingletonReconcileFailed(ctx, repairController)).To(MatchError(provisioning.ErrNodePoolsNotFound))
			expectHeldBack(staticNode, disruption.RepairBlockedNoReplacement)
			expectHeldBack(budgetNode, disruption.RepairBlockedBudget)
			expectHeldBack(breakerNodes[0], disruption.RepairBlockedBreaker)
			expectHeldBack(breakerNodes[1], disruption.RepairBlockedBreaker)
			expectNotHeldBack(node)
			expectBlocked(staticNodePool, disruption.RepairBlockedNoReplacement, 1)
			expectBlocked(budgetNodePool, disruption.RepairBlockedBudget, 1)
			expectBlocked(breakerNodePool, disruption.RepairBlockedBreaker, 2)
			expectNoneBlocked(nodePool)
		})

		It("should save each pass's reasons and counts before the methods after repair run", func() {
			// seen and counted record, each time the later method sees a candidate, whether cluster state has a reason for
			// node and whether the gauge counts it under budget.
			var seen, counted []bool
			reader := &reasonsReader{read: func() {
				_, found := cluster.RepairBlockedReasons()[node.Spec.ProviderID]
				seen = append(seen, found)
				counted = append(counted, blockedSeriesFound(blockedLabels(nodePool, disruption.RepairBlockedBudget)))
			}}
			dc := disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
				disruption.WithMethods(repair, reader))
			holdBackOnBudget(dc)
			Expect(seen).To(Equal([]bool{true}))
			Expect(counted).To(Equal([]bool{true}))

			// Once the node recovers, the later method already sees its reason and count dropped.
			markHealthy(node, "BadNode")
			ExpectSingletonReconciled(ctx, dc)
			Expect(seen).To(Equal([]bool{true, false}))
			Expect(counted).To(Equal([]bool{true, false}))
		})

		It("should give no reasons while cluster state isn't synced, since repair doesn't run", func() {
			holdBackOnBudget(repairController)

			// A NodeClaim without a provider ID, as during any launch, leaves cluster state unsynced.
			launching := test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels()}})
			launching.Status.ProviderID = ""
			ExpectApplied(ctx, env.Client, launching)
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(launching))
			Expect(cluster.Synced(ctx)).To(BeFalse())
			ExpectSingletonReconciled(ctx, repairController)
			Expect(cluster.RepairBlockedReasons()).To(BeEmpty())
			expectNoneBlocked(nodePool)

			// Once cluster state syncs, the next pass gives the node its reason again.
			launching = ExpectExists(ctx, env.Client, launching)
			launching.Status.ProviderID = test.RandomProviderID()
			ExpectApplied(ctx, env.Client, launching)
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(launching))
			Expect(cluster.Synced(ctx)).To(BeTrue())
			ExpectSingletonReconciled(ctx, repairController)
			expectHeldBack(node, disruption.RepairBlockedBudget)
			expectBlocked(nodePool, disruption.RepairBlockedBudget, 1)
		})

		It("should give no reasons while a leftover disruption taint can't be removed, since repair doesn't run", func() {
			patchClient := &patchErrorClient{Client: env.Client, failPatch: &corev1.Node{}}
			_, patchController := newRepairControllerWith(patchClient)
			holdBackOnBudget(patchController)

			// The patch that removes a leftover disruption taint gets a conflict, so Reconcile requeues without an error
			// before repair runs.
			node = ExpectExists(ctx, env.Client, node)
			node.Spec.Taints = append(node.Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
			patchClient.err = apierrors.NewConflict(corev1.Resource("nodes"), node.Name, errors.New("injected conflict"))
			ExpectSingletonReconciled(ctx, patchController)
			Expect(cluster.RepairBlockedReasons()).To(BeEmpty())
			expectNoneBlocked(nodePool)

			// Once the patch succeeds, the next run removes the taint and gives the node its reason again.
			patchClient.err = nil
			ExpectSingletonReconciled(ctx, patchController)
			expectHeldBack(node, disruption.RepairBlockedBudget)
			expectBlocked(nodePool, disruption.RepairBlockedBudget, 1)
		})

		It("should give a past-due node repair's reason, unless the node is queued or deleting", func() {
			initNode(nodeClaim, node)
			budget := disruption.RepairBlockedBudget.Name
			Expect(disruption.RepairPastDueReason(ExpectStateNodeExists(cluster, node), budget)).To(Equal(budget))
			// Without a reason from repair, the node's reason is unknown.
			Expect(disruption.RepairPastDueReason(ExpectStateNodeExists(cluster, node), "")).To(Equal(disruption.RepairBlockedUnknown.Name))

			// The disruption queue marks the node while it handles it, which beats repair's older reason.
			cluster.MarkForDeletion(node.Spec.ProviderID)
			Expect(disruption.RepairPastDueReason(ExpectStateNodeExists(cluster, node), budget)).To(Equal(disruption.RepairBlockedReplacing.Name))

			// Deletion beats both.
			ExpectDeletionTimestampSet(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))
			Expect(disruption.RepairPastDueReason(ExpectStateNodeExists(cluster, node), budget)).To(Equal(disruption.RepairBlockedDeleting.Name))
		})

		It("should not give a past-due node do-not-repair, which repair can have recorded before the annotation was removed", func() {
			initNode(nodeClaim, node)
			Expect(disruption.RepairPastDueReason(ExpectStateNodeExists(cluster, node), disruption.RepairBlockedDoNotRepair.Name)).To(Equal(disruption.RepairBlockedUnknown.Name))
		})

		It("should name the node when its reboot fails the pass, and still requeue on a conflict", func() {
			useRepairPolicies([]cloudprovider.RepairPolicy{
				// The drain bound makes the reboot patch the NodeClaim's annotations, which is the patch that fails.
				{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "RebootMe", TolerationDuration: 10 * time.Minute, TerminationGracePeriod: lo.ToPtr(5 * time.Minute), Action: cloudprovider.RebootNode},
				{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
			})
			failingClient := &patchErrorClient{Client: env.Client, failPatch: &v1.NodeClaim{}}
			_, failingController := newRepairControllerWith(failingClient)
			initNode(nodeClaim, node)
			markUnhealthyWithReason(node, "BadNode", "RebootMe")
			env.Clock.Step(11 * time.Minute)

			// A conflict still makes the run retry without an error.
			failingClient.err = apierrors.NewConflict(schema.GroupResource{Group: apis.Group, Resource: "nodeclaims"}, nodeClaim.Name, errors.New("injected conflict"))
			result, err := failingController.Reconcile(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeTrue())

			injectedErr := errors.New("injected nodeclaim patch failure")
			failingClient.err = injectedErr
			err = ExpectSingletonReconcileFailed(ctx, failingController)
			Expect(err).To(MatchError(injectedErr))
			Expect(err).To(MatchError(ContainSubstring("Node=" + node.Name)))
			Expect(err).To(MatchError(ContainSubstring("NodeClaim=" + nodeClaim.Name)))
			Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeRebooting).IsTrue()).To(BeFalse())
		})

		It("should label the node's past-due time with the reason repair's latest pass gave", func() {
			metricsController := metricsnode.NewController(env.Clock, cluster)
			// Drop the past-due series this spec makes, so no later spec sees them.
			DeferCleanup(func() {
				metricsnode.RepairPastDueTimeSeconds.Reset()
				metricsnode.RepairPastDueDurationSeconds.Reset()
			})
			expectPastDueReason := func(reason string) {
				GinkgoHelper()
				ExpectSingletonReconciled(ctx, metricsController)
				_, found := FindMetricWithLabelValues("karpenter_nodes_repair_past_due_time_seconds", map[string]string{"node_name": node.Name, "blocked_reason": reason})
				Expect(found).To(BeTrue(), "past-due row for %s with blocked_reason %q", node.Name, reason)
			}
			holdBackOnBudget(repairController)
			// Repair already holds the node back, but it isn't past due yet, so its blocked_reason is empty.
			expectPastDueReason("")

			// The policy has no drain bound, so the node is past due 15 minutes after its toleration ends.
			env.Clock.Step(15 * time.Minute)
			ExpectSingletonReconciled(ctx, repairController)
			expectPastDueReason(disruption.RepairBlockedBudget.Name)
		})
	})
})
