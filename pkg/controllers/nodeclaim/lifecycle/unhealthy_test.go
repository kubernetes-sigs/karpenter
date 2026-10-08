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

package lifecycle_test

import (
	"context"
	"fmt"
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
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	nodeclaimlifecycle "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	karpevents "sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/nodepoolhealth"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Unhealthy Uninitialized Repair", func() {
	var repairCtx context.Context
	var repairCluster *state.Cluster
	var controller *nodeclaimlifecycle.Controller
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node

	newController := func(c client.Client) *nodeclaimlifecycle.Controller {
		return nodeclaimlifecycle.NewController(env.Clock, c, cloudProvider, recorder, repairCluster, nodepoolhealth.NewState(), nil)
	}
	usePolicies := func(policies ...cloudprovider.RepairPolicy) {
		cloudProvider.RepairPolicy = policies
		repairCluster = state.NewCluster(env.Clock, env.Client, cloudProvider,
			state.WithRepairPolicyMatcher(lo.Must(health.NewRepairPolicyMatcher(repairCtx, cloudProvider))))
		controller = newController(env.Client)
	}
	newNodeClaimAndNode := func(nodePoolName string) (*v1.NodeClaim, *corev1.Node) {
		return test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels:     map[string]string{v1.NodePoolLabelKey: nodePoolName},
			Finalizers: []string{v1.TerminationFinalizer},
		}})
	}
	// register applies a NodeClaim whose Node registered but is NotReady, so it never initializes. The API server stamps
	// creation with real time, so the fake clock is kept at or past it.
	register := func(nc *v1.NodeClaim, n *corev1.Node) {
		n.Labels[v1.NodeRegisteredLabelKey] = "true"
		n.Spec.Taints = lo.Reject(n.Spec.Taints, func(t corev1.Taint, _ int) bool { return t.MatchTaint(&v1.UnregisteredNoExecuteTaint) })
		nc.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
		nc.StatusConditions().SetTrue(v1.ConditionTypeRegistered)
		ExpectApplied(ctx, env.Client, nc, n)
		if env.Clock.Now().Before(n.CreationTimestamp.Time) {
			env.Clock.SetTime(n.CreationTimestamp.Time)
		}
		ExpectMakeNodesNotReady(ctx, env.Client, env.Clock, n)
	}
	// initialize applies an initialized NodeClaim and Node, for the circuit breaker's totals.
	initialize := func(nc *v1.NodeClaim, n *corev1.Node) {
		n.Labels[v1.NodeRegisteredLabelKey] = "true"
		n.Labels[v1.NodeInitializedLabelKey] = "true"
		nc.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
		nc.StatusConditions().SetTrue(v1.ConditionTypeRegistered)
		nc.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
		ExpectApplied(ctx, env.Client, nc, n)
		ExpectMakeNodesReady(ctx, env.Client, env.Clock, n)
	}
	reconcile := func(nc *v1.NodeClaim) time.Duration {
		GinkgoHelper()
		return ExpectObjectReconciled(repairCtx, env.Client, controller, nc).RequeueAfter
	}
	expectDeleted := func(nc *v1.NodeClaim) {
		GinkgoHelper()
		Expect(ExpectExists(ctx, env.Client, nc).DeletionTimestamp.IsZero()).To(BeFalse())
	}
	expectNotDeleted := func(nc *v1.NodeClaim) {
		GinkgoHelper()
		Expect(ExpectExists(ctx, env.Client, nc).DeletionTimestamp.IsZero()).To(BeTrue())
	}
	expectDeadline := func(nc *v1.NodeClaim, deadline time.Time) {
		GinkgoHelper()
		Expect(ExpectExists(ctx, env.Client, nc).Annotations).To(HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, deadline.Format(time.RFC3339)))
	}
	expectNoDeadline := func(nc *v1.NodeClaim) {
		GinkgoHelper()
		Expect(ExpectExists(ctx, env.Client, nc).Annotations).ToNot(HaveKey(v1.NodeClaimTerminationTimestampAnnotationKey))
	}
	// markDeleting deletes the NodeClaim with mutate applied to its status first, as another controller would.
	markDeleting := func(nc *v1.NodeClaim, mutate func(*v1.NodeClaim)) {
		stored := ExpectExists(ctx, env.Client, nc)
		mutate(stored)
		ExpectApplied(ctx, env.Client, stored)
		Expect(env.Client.Delete(ctx, stored)).To(Succeed())
	}
	markUnhealthy := func(nc *v1.NodeClaim) {
		nc.StatusConditions(status.WithClock(env.Clock)).SetTrueWithReason(v1.ConditionTypeDisruptionReason,
			string(v1.DisruptionReasonUnhealthy), string(v1.DisruptionReasonUnhealthy))
	}
	blockedNames := func() []string {
		var names []string
		recorder.ForEachEvent(func(evt karpevents.Event) {
			if evt.Reason == karpevents.NodeRepairBlocked {
				if o, ok := evt.InvolvedObject.(client.Object); ok {
					names = append(names, o.GetName())
				}
			}
		})
		return names
	}
	disruptedLabels := func() map[string]string {
		return map[string]string{metrics.ReasonLabel: metrics.UnhealthyReason, metrics.NodePoolLabel: nodePool.Name}
	}

	BeforeEach(func() {
		repairCtx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true)}}))
		env.Clock.SetTime(time.Now().Truncate(time.Second))
		recorder.Reset()
		usePolicies(cloudprovider.RepairPolicy{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode})
		nodePool = test.NodePool()
		ExpectApplied(ctx, env.Client, nodePool)
		nodeClaim, node = newNodeClaimAndNode(nodePool.Name)
	})
	AfterEach(func() {
		metrics.NodeClaimsDisruptedTotal.Reset()
		metrics.PodsDisruptionInitiatedTotal.Reset()
		health.NodeClaimsUnhealthyDisruptedTotal.Reset()
	})

	It("should forcefully replace a registered node that never initialized once the toleration elapses", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		for _, pod := range test.Pods(2, test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{
			{Kind: "ReplicaSet", APIVersion: "appsv1", Name: "rs", UID: "1234567890", Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true)},
		}}}) {
			ExpectApplied(ctx, env.Client, pod)
		}

		reconcile(nodeClaim)

		expectDeleted(nodeClaim)
		stored := ExpectExists(ctx, env.Client, nodeClaim)
		Expect(stored.StatusConditions().Get(v1.ConditionTypeDisruptionReason).IsTrue()).To(BeTrue())
		Expect(stored.StatusConditions().Get(v1.ConditionTypeDisruptionReason).Reason).To(Equal(string(v1.DisruptionReasonUnhealthy)))
		Expect(recorder.Calls(karpevents.DisruptionTerminating)).To(Equal(2)) // one each for the Node and NodeClaim
		labels := lo.Assign(disruptedLabels(), map[string]string{
			metrics.CapacityTypeLabel:        nodeClaim.Labels[v1.CapacityTypeLabelKey],
			metrics.ConsolidationPolicyLabel: "",
			metrics.TerminationModeLabel:     metrics.TerminationModeForceful,
		})
		ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, labels)
		ExpectMetricCounterValue(metrics.PodsDisruptionInitiatedTotal, 2, labels)
		ExpectMetricCounterValue(health.NodeClaimsUnhealthyDisruptedTotal, 1, map[string]string{
			health.RepairCondition.Name:  "ready",
			metrics.NodePoolLabel:        nodePool.Name,
			metrics.CapacityTypeLabel:    nodeClaim.Labels[v1.CapacityTypeLabelKey],
			health.ImageID.Name:          nodeClaim.Status.ImageID,
			metrics.TerminationModeLabel: metrics.TerminationModeForceful,
		})

		// Finalization forces the drain.
		reconcile(nodeClaim)
		expectDeadline(nodeClaim, env.Clock.Now())
		ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, labels)
	})
	It("should label the unhealthy-disrupted metric with the condition that triggered repair", func() {
		usePolicies(cloudprovider.RepairPolicy{ConditionType: "AcceleratorReady", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode})
		register(nodeClaim, node)
		stored := ExpectExists(ctx, env.Client, node)
		stored.Status.Conditions = append(stored.Status.Conditions, corev1.NodeCondition{
			Type: "AcceleratorReady", Status: corev1.ConditionFalse, Reason: "XID48", LastTransitionTime: metav1.NewTime(env.Clock.Now()),
		})
		ExpectApplied(ctx, env.Client, stored)
		env.Clock.Step(31 * time.Minute)

		reconcile(nodeClaim)

		expectDeleted(nodeClaim)
		ExpectMetricCounterValue(health.NodeClaimsUnhealthyDisruptedTotal, 1, map[string]string{
			health.RepairCondition.Name: "accelerator_ready",
			metrics.NodePoolLabel:       nodePool.Name,
		})
	})
	It("should requeue for the toleration and replace the node once it elapses", func() {
		register(nodeClaim, node)
		env.Clock.Step(10 * time.Minute)

		Expect(reconcile(nodeClaim)).To(Equal(20 * time.Minute))
		expectNotDeleted(nodeClaim)

		env.Clock.Step(20 * time.Minute)
		reconcile(nodeClaim)
		expectDeleted(nodeClaim)
	})
	It("should not replace a node that isn't unhealthy", func() {
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: "example.com/startup", Effect: corev1.TaintEffectNoSchedule})
		nodeClaim.Spec.StartupTaints = []corev1.Taint{{Key: "example.com/startup", Effect: corev1.TaintEffectNoSchedule}}
		register(nodeClaim, node)
		ExpectMakeNodesReady(ctx, env.Client, env.Clock, node)
		env.Clock.Step(31 * time.Minute)

		Expect(reconcile(nodeClaim)).To(BeZero())

		expectNotDeleted(nodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeFalse())
	})
	It("should not replace a node that initializes in the same reconcile", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		// Ready, with nothing else outstanding, so initialization marks the NodeClaim before liveness runs.
		ExpectMakeNodesReady(ctx, env.Client, env.Clock, node)
		stored := ExpectExists(ctx, env.Client, node)
		stored.Status.Conditions = append(stored.Status.Conditions, corev1.NodeCondition{
			Type: "AcceleratorReady", Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(env.Clock.Now().Add(-time.Hour)),
		})
		ExpectApplied(ctx, env.Client, stored)
		usePolicies(cloudprovider.RepairPolicy{ConditionType: "AcceleratorReady", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode})

		reconcile(nodeClaim)

		expectNotDeleted(nodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
	})
	It("should not replace a node labeled initialized before its NodeClaim condition catches up", func() {
		node.Labels[v1.NodeInitializedLabelKey] = "true"
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		reconcile(nodeClaim)

		expectNotDeleted(nodeClaim)
	})
	It("should not replace a node that is rebooting", func() {
		nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonIssued, "rebooting")
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		reconcile(nodeClaim)

		expectNotDeleted(nodeClaim)
	})
	It("should only count unhealthy time after a reboot completes", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		stored := ExpectExists(ctx, env.Client, nodeClaim)
		stored.StatusConditions(status.WithClock(env.Clock)).SetFalse(v1.ConditionTypeRebooting, v1.RebootReasonSucceeded, "node rebooted and rejoined the cluster")
		ExpectApplied(ctx, env.Client, stored)

		Expect(reconcile(nodeClaim)).To(Equal(30 * time.Minute))
		expectNotDeleted(nodeClaim)

		env.Clock.Step(31 * time.Minute)
		reconcile(nodeClaim)
		expectDeleted(nodeClaim)
	})
	DescribeTable("should not replace a node annotated do-not-repair",
		func(onNode bool) {
			annotations := map[string]string{v1.DoNotRepairAnnotationKey: "true"}
			if onNode {
				node.Annotations = lo.Assign(node.Annotations, annotations)
			} else {
				nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, annotations)
			}
			register(nodeClaim, node)
			env.Clock.Step(31 * time.Minute)

			reconcile(nodeClaim)

			expectNotDeleted(nodeClaim)
		},
		Entry("on the Node", true),
		Entry("on the NodeClaim", false),
	)
	It("should replace a node annotated do-not-disrupt", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		reconcile(nodeClaim)

		expectDeleted(nodeClaim)
	})
	It("should not replace a node without a NodePool", func() {
		delete(nodeClaim.Labels, v1.NodePoolLabelKey)
		delete(node.Labels, v1.NodePoolLabelKey)
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		reconcile(nodeClaim)

		expectNotDeleted(nodeClaim)
	})
	It("should not replace a node when the NodeRepair feature gate is disabled", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		repairCtx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(false)}}))

		reconcile(nodeClaim)

		expectNotDeleted(nodeClaim)
	})
	It("should not replace a node when cluster state has no repair policy matcher", func() {
		repairCluster = state.NewCluster(env.Clock, env.Client, cloudProvider)
		controller = newController(env.Client)
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		reconcile(nodeClaim)

		expectNotDeleted(nodeClaim)
	})
	It("should replace rather than reboot a registered node that never initialized", func() {
		usePolicies(cloudprovider.RepairPolicy{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
			cloudprovider.RepairPolicy{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^NotReady$", TolerationDuration: 30 * time.Minute, Action: cloudprovider.RebootNode})
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		reconcile(nodeClaim)

		expectDeleted(nodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeRebooting)).To(BeNil())
		Expect(cloudProvider.RebootCalls).To(BeEmpty())
	})
	It("should not replace a NodeClaim that changes between the read and the delete", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		controller = newController(&beforeDeleteClient{Client: env.Client, beforeDelete: func() {
			stored := ExpectExists(ctx, env.Client, nodeClaim)
			stored.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
			ExpectApplied(ctx, env.Client, stored)
		}})

		ExpectObjectReconciled(repairCtx, env.Client, controller, nodeClaim)

		expectNotDeleted(nodeClaim)
		_, found := FindMetricWithLabelValues(ExpectMetricName(metrics.NodeClaimsDisruptedTotal.(*opmetrics.PrometheusCounter)), disruptedLabels())
		Expect(found).To(BeFalse())
	})
	It("should not delete when marking the NodeClaim disrupted fails", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		controller = newController(&failStatusPatchClient{Client: env.Client})

		_ = ExpectObjectReconcileFailed(repairCtx, env.Client, controller, nodeClaim)

		expectNotDeleted(nodeClaim)
	})
	It("should not delete a NodeClaim that changes between the read and the mark", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		// Persist what initialization writes, so the lifecycle status patch has nothing of its own to write back.
		ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		controller = newController(&beforeStatusPatchClient{Client: env.Client, before: func() {
			stored := ExpectExists(ctx, env.Client, nodeClaim)
			stored.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
			ExpectApplied(ctx, env.Client, stored)
		}})

		ExpectObjectReconciled(repairCtx, env.Client, controller, nodeClaim)

		expectNotDeleted(nodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
	})
	It("should not overwrite a change that made the delete fail", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		// Persist what initialization writes, so the only status change this reconcile makes is the mark.
		ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		controller = newController(&beforeDeleteClient{Client: env.Client, beforeDelete: func() {
			stored := ExpectExists(ctx, env.Client, nodeClaim)
			stored.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
			ExpectApplied(ctx, env.Client, stored)
		}})

		ExpectObjectReconciled(repairCtx, env.Client, controller, nodeClaim)

		expectNotDeleted(nodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
	})
	It("should clear its mark when a failed delete left one on a NodeClaim it no longer replaces", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		// Persist what initialization writes, so the lifecycle status patch leaves the mark the failed delete left behind.
		ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		controller = newController(&failDeleteClient{Client: env.Client})
		_ = ExpectObjectReconcileFailed(repairCtx, env.Client, controller, nodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeDisruptionReason).IsTrue()).To(BeTrue())
		controller = newController(env.Client)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
		ExpectApplied(ctx, env.Client, nodeClaim)

		reconcile(nodeClaim)

		expectNotDeleted(nodeClaim)
		Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeDisruptionReason)).To(BeNil())
	})
	Context("Termination Deadline", func() {
		// replace has liveness delete the NodeClaim, without finalizing it.
		replace := func() {
			register(nodeClaim, node)
			env.Clock.Step(31 * time.Minute)
			reconcile(nodeClaim)
			expectDeleted(nodeClaim)
			expectNoDeadline(nodeClaim)
		}
		It("should force the drain of a NodeClaim it marked, without in-process state", func() {
			replace()
			controller = newController(env.Client)

			reconcile(nodeClaim)

			expectDeadline(nodeClaim, env.Clock.Now())
		})
		It("should force the drain even after the node's conditions change", func() {
			replace()
			// The instance is shutting down, so the kubelet stops reporting and Ready goes Unknown.
			stored := ExpectExists(ctx, env.Client, node)
			stored.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown, LastTransitionTime: metav1.NewTime(env.Clock.Now())}}
			ExpectApplied(ctx, env.Client, stored)

			reconcile(nodeClaim)

			expectDeadline(nodeClaim, env.Clock.Now())
		})
		It("should not force the drain of a NodeClaim something else is deleting", func() {
			register(nodeClaim, node)
			env.Clock.Step(31 * time.Minute)
			markDeleting(nodeClaim, func(*v1.NodeClaim) {})

			reconcile(nodeClaim)

			expectNoDeadline(nodeClaim)
		})
		// The repair disruption method marks DisruptionReason Unhealthy with the reason as the message.
		DescribeTable("should not force the drain of a NodeClaim the repair disruption method marked",
			func(mutate func(*v1.NodeClaim)) {
				register(nodeClaim, node)
				markDeleting(nodeClaim, func(nc *v1.NodeClaim) {
					markUnhealthy(nc)
					mutate(nc)
				})

				reconcile(nodeClaim)

				expectNoDeadline(nodeClaim)
			},
			Entry("replacing an initialized node", func(nc *v1.NodeClaim) { nc.StatusConditions().SetTrue(v1.ConditionTypeInitialized) }),
			Entry("replacing after a failed reboot", func(nc *v1.NodeClaim) {
				nc.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonIssued, "rebooting")
			}),
			Entry("after a reboot, before the node initializes again", func(nc *v1.NodeClaim) {
				nc.StatusConditions().SetFalse(v1.ConditionTypeRebooting, v1.RebootReasonSucceeded, "node rebooted and rejoined the cluster")
			}),
		)
		It("should not extend an earlier deadline", func() {
			earlier := env.Clock.Now().Add(-time.Hour)
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: earlier.Format(time.RFC3339)})
			register(nodeClaim, node)
			env.Clock.Step(31 * time.Minute)
			reconcile(nodeClaim)
			expectDeleted(nodeClaim)

			reconcile(nodeClaim)

			expectDeadline(nodeClaim, earlier)
		})
		DescribeTable("should replace a later or unparsable deadline",
			func(value func() string) {
				nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: value()})
				register(nodeClaim, node)
				env.Clock.Step(31 * time.Minute)
				reconcile(nodeClaim)
				expectDeleted(nodeClaim)

				reconcile(nodeClaim)

				expectDeadline(nodeClaim, env.Clock.Now())
			},
			Entry("later", func() string { return env.Clock.Now().Add(24 * time.Hour).Format(time.RFC3339) }),
			Entry("unparsable", func() string { return "not-a-timestamp" }),
		)
		It("should prefer the repair deadline over the NodeClaim's terminationGracePeriod", func() {
			nodeClaim.Spec.TerminationGracePeriod = &metav1.Duration{Duration: time.Hour}
			replace()

			reconcile(nodeClaim)

			expectDeadline(nodeClaim, env.Clock.Now())
		})
	})
	Context("Circuit Breaker", func() {
		// pool fills nodePool with ten nodes, the given number of them registered and unhealthy, including nodeClaim.
		pool := func(unhealthy int) {
			for range 10 - unhealthy {
				initialize(newNodeClaimAndNode(nodePool.Name))
			}
			register(nodeClaim, node)
			for range unhealthy - 1 {
				register(newNodeClaimAndNode(nodePool.Name))
			}
			env.Clock.Step(31 * time.Minute)
		}
		It("should replace when the NodePool is at the unhealthy threshold", func() {
			pool(2)

			reconcile(nodeClaim)

			expectDeleted(nodeClaim)
		})
		It("should block and requeue when the NodePool is above the unhealthy threshold", func() {
			pool(3)

			Expect(reconcile(nodeClaim)).To(Equal(time.Minute))

			expectNotDeleted(nodeClaim)
			Expect(blockedNames()).To(ContainElements(node.Name, nodeClaim.Name, nodePool.Name))
		})
		It("should count unhealthy nodes that are still within toleration", func() {
			for range 7 {
				initialize(newNodeClaimAndNode(nodePool.Name))
			}
			register(nodeClaim, node)
			env.Clock.Step(31 * time.Minute)
			for range 2 {
				nc, n := newNodeClaimAndNode(nodePool.Name)
				initialize(nc, n)
				ExpectMakeNodesNotReady(ctx, env.Client, env.Clock, n)
			}

			reconcile(nodeClaim)

			expectNotDeleted(nodeClaim)
		})
		It("should not block another NodePool", func() {
			pool(3)
			otherPool := test.NodePool()
			ExpectApplied(ctx, env.Client, otherPool)
			otherClaim, otherNode := newNodeClaimAndNode(otherPool.Name)
			register(otherClaim, otherNode)
			for range 9 {
				initialize(newNodeClaimAndNode(otherPool.Name))
			}
			env.Clock.Step(31 * time.Minute)

			reconcile(nodeClaim)
			reconcile(otherClaim)

			expectNotDeleted(nodeClaim)
			expectDeleted(otherClaim)
			Expect(blockedNames()).ToNot(ContainElement(otherNode.Name))
		})
	})
})

type beforeDeleteClient struct {
	client.Client
	beforeDelete func()
}

func (c *beforeDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.beforeDelete()
	return c.Client.Delete(ctx, obj, opts...)
}

type failDeleteClient struct {
	client.Client
}

func (c *failDeleteClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return fmt.Errorf("injected delete failure")
}

// beforeStatusPatchClient runs before ahead of its first status patch.
type beforeStatusPatchClient struct {
	client.Client
	before func()
}

func (c *beforeStatusPatchClient) Status() client.SubResourceWriter {
	return &beforeSubResourceWriter{SubResourceWriter: c.Client.Status(), client: c}
}

type beforeSubResourceWriter struct {
	client.SubResourceWriter
	client *beforeStatusPatchClient
}

func (w *beforeSubResourceWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if before := w.client.before; before != nil {
		w.client.before = nil
		before()
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

type failStatusPatchClient struct {
	client.Client
}

func (c *failStatusPatchClient) Status() client.SubResourceWriter {
	return &failSubResourceWriter{SubResourceWriter: c.Client.Status()}
}

type failSubResourceWriter struct {
	client.SubResourceWriter
}

func (w *failSubResourceWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return fmt.Errorf("injected status patch failure")
}
