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

package uninitialized

import (
	"context"
	"fmt"
	"sync"
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
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// defaultOwnerRefs makes a pod reschedulable, so it counts toward the disrupted-pods metric.
var defaultOwnerRefs = []metav1.OwnerReference{{Kind: "ReplicaSet", APIVersion: "appsv1", Name: "rs", UID: "1234567890", Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true)}}

var _ = Describe("Uninitialized Node Repair", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node

	// syncState feeds the objects' current API state into cluster state, as the informers would.
	syncState := func(nc *v1.NodeClaim, n *corev1.Node) {
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nc))
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(n))
	}
	// newNodeClaimAndNode returns a NodePool-owned NodeClaim/Node pair whose finalizers keep a deleted NodeClaim
	// observable. Cluster state only tracks an uninitialized managed Node once it has an instance type label.
	newNodeClaimAndNode := func() (*v1.NodeClaim, *corev1.Node) {
		return test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[0].Name,
			},
			Finalizers: []string{v1.TerminationFinalizer},
		}})
	}
	// apply writes a NodeClaim/Node pair that has launched, keeping the fake clock at or past the Node's creation: the
	// API server stamps creation with real time, and a condition can't predate it.
	apply := func(nc *v1.NodeClaim, n *corev1.Node) {
		n.Spec.Taints = lo.Reject(n.Spec.Taints, func(t corev1.Taint, _ int) bool { return t.MatchTaint(&v1.UnregisteredNoExecuteTaint) })
		nc.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
		ExpectApplied(ctx, env.Client, nc, n)
		if env.Clock.Now().Before(n.CreationTimestamp.Time) {
			env.Clock.SetTime(n.CreationTimestamp.Time)
		}
	}
	// register applies a NodeClaim/Node pair that registered but never initialized because the Node went NotReady at
	// the current clock time, then syncs cluster state.
	register := func(nc *v1.NodeClaim, n *corev1.Node) {
		n.Labels[v1.NodeRegisteredLabelKey] = "true"
		nc.StatusConditions().SetTrue(v1.ConditionTypeRegistered)
		apply(nc, n)
		ExpectMakeNodesNotReady(ctx, env.Client, env.Clock, n)
		syncState(nc, n)
	}
	// initialize applies an initialized, healthy NodeClaim/Node pair and syncs cluster state.
	initialize := func(nc *v1.NodeClaim, n *corev1.Node) {
		n.Labels[v1.NodeRegisteredLabelKey] = "true"
		n.Labels[v1.NodeInitializedLabelKey] = "true"
		nc.StatusConditions().SetTrue(v1.ConditionTypeRegistered)
		nc.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
		apply(nc, n)
		ExpectMakeNodesReady(ctx, env.Client, env.Clock, n)
		syncState(nc, n)
	}
	// updateNodeClaim applies a change to the stored NodeClaim and syncs cluster state.
	updateNodeClaim := func(mutate func(nc *v1.NodeClaim)) {
		stored := ExpectExists(ctx, env.Client, nodeClaim)
		mutate(stored)
		ExpectApplied(ctx, env.Client, stored)
		syncState(stored, node)
	}
	// markDeleting deletes the NodeClaim out from under the controller; its finalizer keeps it observable.
	markDeleting := func(nc *v1.NodeClaim) {
		Expect(env.Client.Delete(ctx, nc)).To(Succeed())
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nc))
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
	disruptedLabels := func() map[string]string {
		return map[string]string{metrics.ReasonLabel: metrics.UnhealthyReason, metrics.NodePoolLabel: nodePool.Name}
	}

	BeforeEach(func() {
		env.Clock.SetTime(time.Now().Truncate(time.Second))
		cloudProvider.Reset()
		cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
		recorder.Reset()
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		})
		nodePool = test.NodePool()
		ExpectApplied(ctx, env.Client, nodePool)
		nodeClaim, node = newNodeClaimAndNode()
	})
	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
		cluster.Reset()
		metrics.NodeClaimsDisruptedTotal.Reset()
		health.NodeClaimsUnhealthyDisruptedTotal.Reset()
	})

	It("should forcefully delete a registered node that never initialized once the toleration elapses", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		// Two reschedulable pods ride the node, so the disrupted-pod metric has something to count.
		pods := test.Pods(2, test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
		for _, pod := range pods {
			ExpectApplied(ctx, env.Client, pod)
		}

		result := ExpectSingletonReconciled(ctx, controller)

		expectDeleted(nodeClaim)
		expectDeadline(nodeClaim, env.Clock.Now())
		Expect(result.RequeueAfter).To(Equal(15 * time.Second))
		fullLabels := lo.Assign(disruptedLabels(), map[string]string{
			metrics.CapacityTypeLabel:        nodeClaim.Labels[v1.CapacityTypeLabelKey],
			metrics.ConsolidationPolicyLabel: "",
			metrics.TerminationModeLabel:     metrics.TerminationModeForceful,
		})
		ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, fullLabels)
		ExpectMetricCounterValue(metrics.PodsDisruptionInitiatedTotal, 2, fullLabels)
		ExpectMetricCounterValue(health.NodeClaimsUnhealthyDisruptedTotal, 1, map[string]string{
			health.RepairCondition.Name:  "ready",
			metrics.NodePoolLabel:        nodePool.Name,
			metrics.CapacityTypeLabel:    nodeClaim.Labels[v1.CapacityTypeLabelKey],
			health.ImageID.Name:          nodeClaim.Status.ImageID,
			metrics.TerminationModeLabel: metrics.TerminationModeForceful,
		})
	})
	It("should label the unhealthy-disrupted metric with the condition that triggered repair", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: "AcceleratorReady", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		})
		node.Labels[v1.NodeRegisteredLabelKey] = "true"
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeRegistered)
		apply(nodeClaim, node)
		stored := ExpectExists(ctx, env.Client, node)
		stored.Status.Conditions = []corev1.NodeCondition{{
			Type: "AcceleratorReady", Status: corev1.ConditionFalse, Reason: "XID48", LastTransitionTime: metav1.NewTime(env.Clock.Now()),
		}}
		ExpectApplied(ctx, env.Client, stored)
		syncState(nodeClaim, stored)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectDeleted(nodeClaim)
		ExpectMetricCounterValue(health.NodeClaimsUnhealthyDisruptedTotal, 1, map[string]string{
			health.RepairCondition.Name: "accelerator_ready",
			metrics.NodePoolLabel:       nodePool.Name,
		})
	})
	It("should skip a Node whose NodeClaim cluster state hasn't observed yet", func() {
		// The index admits a Node with no NodeClaim; eligibility reads NodeClaim fields, so it must be skipped.
		node.Labels[v1.NodeRegisteredLabelKey] = "true"
		apply(nodeClaim, node)
		ExpectMakeNodesNotReady(ctx, env.Client, env.Clock, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		env.Clock.Step(31 * time.Minute)
		Expect(controller.unhealthyNodes(env.Clock.Now())).To(HaveLen(1))
		Expect(controller.unhealthyNodes(env.Clock.Now())[0].Managed()).To(BeFalse())

		Expect(func() { ExpectSingletonReconciled(ctx, controller) }).ToNot(Panic())

		expectNotDeleted(nodeClaim)
	})
	It("should keep repairing other nodes when one node's delete fails", func() {
		register(nodeClaim, node)
		otherNodeClaim, otherNode := newNodeClaimAndNode()
		register(otherNodeClaim, otherNode)
		for range 8 {
			initialize(newNodeClaimAndNode())
		}
		env.Clock.Step(31 * time.Minute)
		failing := &failDeleteClient{Client: env.Client, name: nodeClaim.Name}
		erroring := NewController(env.Clock, failing, cluster, recorder)

		// The pass reports no error, so a failing node can't push the loop into exponential backoff.
		result := ExpectSingletonReconciled(ctx, erroring)

		Expect(result.RequeueAfter).To(Equal(15 * time.Second))
		expectNotDeleted(nodeClaim)
		expectDeleted(otherNodeClaim)
	})
	It("should repair an eligible node alongside an ineligible unhealthy node", func() {
		// The ineligible node is unhealthy and indexed, but hasn't registered.
		ineligibleClaim, ineligibleNode := newNodeClaimAndNode()
		apply(ineligibleClaim, ineligibleNode)
		ExpectMakeNodesNotReady(ctx, env.Client, env.Clock, ineligibleNode)
		syncState(ineligibleClaim, ineligibleNode)
		register(nodeClaim, node)
		for range 8 {
			initialize(newNodeClaimAndNode())
		}
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectDeleted(nodeClaim)
		expectNotDeleted(ineligibleClaim)
	})
	It("should delete every eligible node in one pass", func() {
		register(nodeClaim, node)
		otherNodeClaim, otherNode := newNodeClaimAndNode()
		register(otherNodeClaim, otherNode)
		// Keep the pool under the circuit breaker's threshold.
		for range 8 {
			initialize(newNodeClaimAndNode())
		}
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectDeleted(nodeClaim)
		expectDeleted(otherNodeClaim)
	})
	It("should not delete a registered node that never initialized before the toleration elapses", func() {
		register(nodeClaim, node)
		env.Clock.Step(29 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectNotDeleted(nodeClaim)
	})
	It("should not delete a healthy registered node that hasn't initialized", func() {
		register(nodeClaim, node)
		ExpectMakeNodesReady(ctx, env.Client, env.Clock, node)
		syncState(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		// A healthy node leaves the index entirely, so this asserts the controller never sees it.
		Expect(controller.unhealthyNodes(env.Clock.Now())).To(BeEmpty())

		ExpectSingletonReconciled(ctx, controller)

		expectNotDeleted(nodeClaim)
	})
	It("should not delete a node whose NodeClaim hasn't registered", func() {
		apply(nodeClaim, node)
		ExpectMakeNodesNotReady(ctx, env.Client, env.Clock, node)
		syncState(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectNotDeleted(nodeClaim)
	})
	It("should delete once the NodeClaim is marked Registered after the Node was labeled registered", func() {
		// Registration labels the Node first; the NodeClaim's Registered condition lands in a later status patch.
		node.Labels[v1.NodeRegisteredLabelKey] = "true"
		apply(nodeClaim, node)
		ExpectMakeNodesNotReady(ctx, env.Client, env.Clock, node)
		syncState(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)
		expectNotDeleted(nodeClaim)

		updateNodeClaim(func(nc *v1.NodeClaim) { nc.StatusConditions().SetTrue(v1.ConditionTypeRegistered) })
		ExpectSingletonReconciled(ctx, controller)
		expectDeleted(nodeClaim)
	})
	It("should not delete a node whose NodeClaim is initialized", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectNotDeleted(nodeClaim)
	})
	It("should not delete a node labeled initialized before its NodeClaim condition catches up", func() {
		node.Labels[v1.NodeInitializedLabelKey] = "true"
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectNotDeleted(nodeClaim)
	})
	It("should not delete a node that is rebooting", func() {
		nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonIssued, "rebooting")
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectNotDeleted(nodeClaim)
	})
	It("should only count unhealthy time after a reboot completes", func() {
		// The condition predates the reboot, e.g. an agent hasn't re-reported it since the node came back.
		nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonIssued, "rebooting")
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		updateNodeClaim(func(nc *v1.NodeClaim) {
			nc.StatusConditions(status.WithClock(env.Clock)).SetFalse(v1.ConditionTypeRebooting, v1.RebootReasonSucceeded, "node rebooted and rejoined the cluster")
		})

		env.Clock.Step(29 * time.Minute)
		ExpectSingletonReconciled(ctx, controller)
		expectNotDeleted(nodeClaim)

		env.Clock.Step(2 * time.Minute)
		ExpectSingletonReconciled(ctx, controller)
		expectDeleted(nodeClaim)
	})
	It("should count unhealthy time that starts after a reboot completes from the condition", func() {
		nodeClaim.StatusConditions(status.WithClock(env.Clock)).SetFalse(v1.ConditionTypeRebooting, v1.RebootReasonSucceeded, "node rebooted and rejoined the cluster")
		env.Clock.Step(time.Minute)
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectDeleted(nodeClaim)
	})
	DescribeTable("should not delete a node annotated do-not-repair",
		func(onNode bool) {
			annotations := map[string]string{v1.DoNotRepairAnnotationKey: "true"}
			if onNode {
				node.Annotations = lo.Assign(node.Annotations, annotations)
			} else {
				nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, annotations)
			}
			register(nodeClaim, node)
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, controller)

			expectNotDeleted(nodeClaim)
		},
		Entry("on the Node", true),
		Entry("on the NodeClaim", false),
	)
	It("should delete a node annotated do-not-disrupt", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectDeleted(nodeClaim)
	})
	It("should not delete a node without a NodePool", func() {
		delete(nodeClaim.Labels, v1.NodePoolLabelKey)
		delete(node.Labels, v1.NodePoolLabelKey)
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectNotDeleted(nodeClaim)
	})
	It("should not delete a node when the NodeRepair feature gate is disabled", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		gateOffCtx := options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(false)}}))

		result := ExpectSingletonReconciled(gateOffCtx, controller)

		expectNotDeleted(nodeClaim)
		Expect(result.RequeueAfter).To(Equal(pollInterval))
	})
	It("should replace rather than reboot a registered node that never initialized", func() {
		useRepairPolicies([]cloudprovider.RepairPolicy{
			{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
			{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^NotReady$", TolerationDuration: 30 * time.Minute, Action: cloudprovider.RebootNode},
		})
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		// Guard the premise: the matching policy selects a reboot.
		Expect(controller.unhealthyNodes(env.Clock.Now())).To(HaveLen(1))
		Expect(controller.unhealthyNodes(env.Clock.Now())[0].GetRepairResult(env.Clock.Now()).Action).To(Equal(cloudprovider.RebootNode))

		ExpectSingletonReconciled(ctx, controller)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting)).To(BeNil())
		Expect(cloudProvider.RebootCalls).To(BeEmpty())
	})
	It("should not extend an earlier termination deadline", func() {
		earlier := env.Clock.Now().Add(-time.Hour)
		nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: earlier.Format(time.RFC3339)})
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, controller)

		expectDeadline(nodeClaim, earlier)
	})
	It("should not delete a node labeled initialized after cluster state observed it", func() {
		// Initialization labels the Node before it marks the NodeClaim Initialized, and that label write does not bump the
		// NodeClaim's ResourceVersion, so the delete precondition alone cannot catch this.
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		stored := ExpectExists(ctx, env.Client, node)
		stored.Labels[v1.NodeInitializedLabelKey] = "true"
		ExpectApplied(ctx, env.Client, stored)

		ExpectSingletonReconciled(ctx, controller)

		expectNotDeleted(nodeClaim)
		expectNoDeadline(nodeClaim)
	})
	It("should not delete a node whose conditions changed after cluster state observed it", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		ExpectMakeNodesReady(ctx, env.Client, env.Clock, node)

		ExpectSingletonReconciled(ctx, controller)

		expectNotDeleted(nodeClaim)
	})
	It("should not delete a NodeClaim that initializes between the re-read and the delete", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		racing := NewController(env.Clock, &beforeDeleteClient{Client: env.Client, beforeDelete: func() {
			stored := ExpectExists(ctx, env.Client, nodeClaim)
			stored.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
			ExpectApplied(ctx, env.Client, stored)
		}}, cluster, recorder)

		ExpectSingletonReconciled(ctx, racing)

		expectNotDeleted(nodeClaim)
		expectNoDeadline(nodeClaim)
	})
	It("should not race cluster state updates", func() {
		// Run with -race. A pass reads cluster state's nodes only through copies taken under its read lock, so concurrent
		// Node updates can't race it, even if cluster state later starts mutating StateNodes in place.
		register(nodeClaim, node)
		for range 8 {
			initialize(newNodeClaimAndNode())
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			for i := range 50 {
				stored := ExpectExists(ctx, env.Client, node)
				stored.Status.Conditions[0].Reason = fmt.Sprintf("NotReady%d", i)
				Expect(cluster.UpdateNode(ctx, stored)).To(Succeed())
			}
		}()
		for range 20 {
			ExpectSingletonReconciled(ctx, controller)
		}
		wg.Wait()
	})
	It("should not delete a NodeClaim that initialized after cluster state observed it", func() {
		register(nodeClaim, node)
		env.Clock.Step(31 * time.Minute)
		// Initialize the NodeClaim without syncing cluster state, so the controller acts on a stale ResourceVersion.
		stored := ExpectExists(ctx, env.Client, nodeClaim)
		stored.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
		ExpectApplied(ctx, env.Client, stored)

		ExpectSingletonReconciled(ctx, controller)

		expectNotDeleted(nodeClaim)
		expectNoDeadline(nodeClaim)
		_, found := FindMetricWithLabelValues(ExpectMetricName(metrics.NodeClaimsDisruptedTotal.(*opmetrics.PrometheusCounter)), disruptedLabels())
		Expect(found).To(BeFalse())
	})
	Context("Deleting NodeClaims", func() {
		It("should not stamp a deadline on an eligible NodeClaim that something else is deleting", func() {
			// Tightening the deadline here would turn an operator's or the disruption queue's graceful drain into a
			// forceful one.
			register(nodeClaim, node)
			env.Clock.Step(31 * time.Minute)
			markDeleting(nodeClaim)

			ExpectSingletonReconciled(ctx, controller)

			expectNoDeadline(nodeClaim)
			// Only this controller's own delete counts as a disruption.
			_, found := FindMetricWithLabelValues(ExpectMetricName(metrics.NodeClaimsDisruptedTotal.(*opmetrics.PrometheusCounter)), disruptedLabels())
			Expect(found).To(BeFalse())
		})
		It("should retry the deadline when stamping fails after its own delete", func() {
			register(nodeClaim, node)
			env.Clock.Step(31 * time.Minute)
			failing := &failPatchClient{Client: env.Client, failures: 1}
			retrying := NewController(env.Clock, failing, cluster, recorder)

			// The pass reports no error, so one bad node can't push the loop into exponential backoff.
			result := ExpectSingletonReconciled(ctx, retrying)
			Expect(result.RequeueAfter).To(Equal(pollInterval))
			expectDeleted(nodeClaim)
			expectNoDeadline(nodeClaim)
			Expect(failing.calls).To(Equal(1))

			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectSingletonReconciled(ctx, retrying)
			expectDeadline(nodeClaim, env.Clock.Now())
			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, disruptedLabels())
		})
		It("should not stamp a deleting NodeClaim that isn't eligible", func() {
			register(nodeClaim, node)
			env.Clock.Step(10 * time.Minute)
			markDeleting(nodeClaim)

			ExpectSingletonReconciled(ctx, controller)

			expectNoDeadline(nodeClaim)
		})
		It("should not stamp a deleting NodeClaim annotated do-not-repair", func() {
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
			register(nodeClaim, node)
			env.Clock.Step(31 * time.Minute)
			markDeleting(nodeClaim)

			ExpectSingletonReconciled(ctx, controller)

			expectNoDeadline(nodeClaim)
		})
	})
	Context("Circuit Breaker", func() {
		// pool builds a ten-node NodePool with the given number of registered-but-unhealthy nodes, all past toleration.
		pool := func(unhealthy int) {
			for range 10 - unhealthy {
				initialize(newNodeClaimAndNode())
			}
			register(nodeClaim, node)
			for range unhealthy - 1 {
				register(newNodeClaimAndNode())
			}
			env.Clock.Step(31 * time.Minute)
		}
		It("should delete when the NodePool is at the unhealthy threshold", func() {
			pool(2)

			ExpectSingletonReconciled(ctx, controller)

			expectDeleted(nodeClaim)
		})
		It("should block when the NodePool is above the unhealthy threshold", func() {
			pool(3)

			result := ExpectSingletonReconciled(ctx, controller)

			expectNotDeleted(nodeClaim)
			Expect(result.RequeueAfter).To(Equal(pollInterval))
			Expect(recorder.Calls(events.NodeRepairBlocked)).To(BeNumerically(">", 0))
		})
		It("should count initialized unhealthy nodes toward the threshold", func() {
			for range 7 {
				initialize(newNodeClaimAndNode())
			}
			for range 2 {
				nc, n := newNodeClaimAndNode()
				initialize(nc, n)
				ExpectMakeNodesNotReady(ctx, env.Client, env.Clock, n)
				syncState(nc, n)
			}
			register(nodeClaim, node)
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, controller)

			expectNotDeleted(nodeClaim)
		})
	})
})

// beforeDeleteClient runs beforeDelete ahead of each Delete so tests can race a write against the controller's delete.
type beforeDeleteClient struct {
	client.Client
	beforeDelete func()
}

func (c *beforeDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.beforeDelete()
	return c.Client.Delete(ctx, obj, opts...)
}

// failDeleteClient fails Delete for one named NodeClaim so tests can check the pass continues past it.
type failDeleteClient struct {
	client.Client
	name string
}

func (c *failDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if obj.GetName() == c.name {
		return fmt.Errorf("injected delete failure")
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// failPatchClient fails its first `failures` Patch calls so tests can fail the termination deadline stamp after a
// successful delete, then observe the retry.
type failPatchClient struct {
	client.Client
	failures int
	calls    int
}

func (c *failPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.calls++
	if c.calls <= c.failures {
		return fmt.Errorf("injected patch failure")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}
