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

package reboot_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/reboot"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var rebootController *reboot.Controller
var env *test.Environment
var cloudProvider *fake.CloudProvider
var recorder *test.EventRecorder
var queue *terminator.Queue

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Reboot")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(
		test.WithCRDs(apis.CRDs...),
		test.WithCRDs(v1alpha1.CRDs...),
		test.WithFieldIndexers(test.NodeClaimProviderIDFieldIndexer(ctx), test.NodeProviderIDFieldIndexer(ctx), test.VolumeAttachmentFieldIndexer(ctx)),
	)
	cloudProvider = fake.NewCloudProvider()
	recorder = test.NewEventRecorder()
	queue = terminator.NewQueue(env.Clock, env.Client, recorder)
	rebootController = reboot.NewController(env.Clock, env.Client, cloudProvider, terminator.NewTerminator(env.Clock, env.Client, queue, recorder), recorder)
})

// nodeClaimDeleteErrorClient fails every NodeClaim delete with err.
type nodeClaimDeleteErrorClient struct {
	client.Client
	err error
}

func (c *nodeClaimDeleteErrorClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*v1.NodeClaim); ok {
		return c.err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Reboot Lifecycle", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node

	BeforeEach(func() {
		env.Clock.SetTime(time.Now())
		cloudProvider.Reset()
		recorder.Reset()
		reboot.RebootsTotal.Reset()
		reboot.RebootDurationSeconds.Reset()
		reboot.RebootRecoveryDurationSeconds.Reset()

		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name},
		}})
		node.Labels[v1.NodePoolLabelKey] = nodePool.Name
		node.Labels[v1.NodeInitializedLabelKey] = "true"
		node.Status.NodeInfo.BootID = "boot-1"
		// Default to forceful reboot; individual specs override as needed.
		nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.RebootTerminationGracePeriodAnnotationKey: "0s"})
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
		nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonRequested, "reboot requested")
	})

	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
	})

	hasRebootTaint := func(n *corev1.Node) bool {
		return lo.ContainsBy(n.Spec.Taints, func(t corev1.Taint) bool { return t.MatchTaint(&v1.RebootingNoScheduleTaint) })
	}

	expectInvalidRequest := func(nc *v1.NodeClaim) {
		nc = ExpectExists(ctx, env.Client, nc)
		cond := nc.StatusConditions().Get(v1.ConditionTypeRebooting)
		Expect(cond.IsFalse()).To(BeTrue())
		Expect(cond.Reason).To(Equal(v1.RebootReasonFailed))
		Expect(nc.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(cloudProvider.RebootCalls).To(BeEmpty())
		ExpectMetricCounterValue(reboot.RebootsTotal, 1, map[string]string{"result": "invalid_request"})
	}

	// A failed post-drain reboot should escalate to NodeClaim replacement.
	expectReplaced := func(nc *v1.NodeClaim, result string) {
		Expect(recorder.Calls(events.RebootFailed)).To(Equal(1))
		ExpectMetricCounterValue(reboot.RebootsTotal, 1, map[string]string{"result": result})
		updated := &v1.NodeClaim{}
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(nc), updated); err == nil {
			Expect(updated.DeletionTimestamp.IsZero()).To(BeFalse())
			Expect(updated.StatusConditions().Get(v1.ConditionTypeRebooting).IsTrue()).To(BeTrue())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	}

	// Advance past the minimum drain window.
	stepPastDrainFloor := func() { env.Clock.Step(6 * time.Second) }

	Context("RebootRequested", func() {
		It("holds a drained reboot for minDrainTime before issuing, even when there is nothing to drain", func() {
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.RebootTerminationGracePeriodAnnotationKey: "1m"})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			result := ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			Expect(cloudProvider.RebootCalls).To(BeEmpty())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(result.RequeueAfter).To(BeNumerically("<=", 5*time.Second))
			node = ExpectExists(ctx, env.Client, node)
			Expect(hasRebootTaint(node)).To(BeTrue())

			stepPastDrainFloor()
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			Expect(cloudProvider.RebootCalls).To(HaveLen(1))
		})

		It("issues the reboot and transitions to RebootIssued", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			stepPastDrainFloor()
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			Expect(cloudProvider.RebootCalls).To(HaveLen(1))

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting)
			Expect(cond.IsTrue()).To(BeTrue())
			Expect(cond.Reason).To(Equal(v1.RebootReasonIssued))
			Expect(cond.Message).To(Equal("reboot requested")) // driving-fault message carried forward from RebootRequested
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.RebootPreBootIDAnnotationKey, "boot-1"))
			Expect(cloudProvider.RebootOperationIDs[0]).ToNot(BeEmpty())
			// A committed reboot invalidates Initialized until the node re-initializes.
			initialized := nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized)
			Expect(initialized.Status).To(Equal(metav1.ConditionUnknown))
			Expect(initialized.Reason).To(Equal(v1.RebootReasonRebooting))

			node = ExpectExists(ctx, env.Client, node)
			Expect(hasRebootTaint(node)).To(BeTrue())
			Expect(node.Labels).ToNot(HaveKey(v1.NodeInitializedLabelKey))
		})

		It("issues a committed reboot even when node repair is disabled", func() {
			repairDisabled := options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(false)}}))
			ExpectApplied(repairDisabled, env.Client, nodePool, nodeClaim, node)
			stepPastDrainFloor()
			ExpectObjectReconciled(repairDisabled, env.Client, rebootController, nodeClaim)

			Expect(cloudProvider.RebootCalls).To(HaveLen(1))
			Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonIssued))
		})

		It("re-issues on a subsequent reboot of the same NodeClaim (no stale-state false success)", func() {
			// Episode 1: issue and succeed.
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			stepPastDrainFloor()
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			Expect(cloudProvider.RebootCalls).To(HaveLen(1))
			firstID := cloudProvider.RebootOperationIDs[0]

			node = ExpectExists(ctx, env.Client, node)
			node.Status.NodeInfo.BootID = "boot-2"
			node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			ExpectApplied(ctx, env.Client, node)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonSucceeded))
			// Episode-scoped state must not leak into the next reboot.
			Expect(nodeClaim.Annotations).ToNot(HaveKey(v1.RebootPreBootIDAnnotationKey))

			// Episode 2: request another reboot on the same NodeClaim.
			node = ExpectExists(ctx, env.Client, node)
			node.Status.NodeInfo.BootID = "boot-2"
			ExpectApplied(ctx, env.Client, node)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
			nodeClaim.StatusConditions(status.WithClock(env.Clock)).SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonRequested, "reboot requested again")
			ExpectApplied(ctx, env.Client, nodeClaim)
			stepPastDrainFloor()
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			// Must issue again rather than treating stale state as a completed reboot.
			Expect(cloudProvider.RebootCalls).To(HaveLen(2))
			Expect(cloudProvider.RebootOperationIDs[1]).ToNot(Equal(firstID)) // distinct per episode
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonIssued))
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.RebootPreBootIDAnnotationKey, "boot-2"))
		})

		It("fails with provider_error when the provider does not implement reboot", func() {
			cloudProvider.NextRebootErr = cloudprovider.NewNodeRebootNotImplementedError()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			stepPastDrainFloor()
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			node = ExpectExists(ctx, env.Client, node)
			Expect(hasRebootTaint(node)).To(BeFalse())
			expectReplaced(nodeClaim, "provider_error")
		})

		DescribeTable("fails with provider_error on the first attempt when the provider rejects the reboot for good",
			func(err error) {
				cloudProvider.NextRebootErr = err
				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
				stepPastDrainFloor()
				ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

				Expect(cloudProvider.RebootCalls).To(BeEmpty())
				expectReplaced(nodeClaim, "provider_error")
			},
			Entry("a failed reboot", cloudprovider.NewNodeRebootFailedError(fmt.Errorf("unauthorized"))),
			Entry("an instance that no longer exists", cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance not found"))),
		)

		It("retries on a transient provider error, staying in RebootRequested", func() {
			cloudProvider.NextRebootErr = fmt.Errorf("throttled")
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			stepPastDrainFloor()
			_ = ExpectObjectReconcileFailed(ctx, env.Client, rebootController, nodeClaim)

			Expect(cloudProvider.RebootCalls).To(BeEmpty())
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting)
			Expect(cond.IsTrue()).To(BeTrue())
			Expect(cond.Reason).To(Equal(v1.RebootReasonRequested))
		})

		It("skips issuing when the boot already changed after recording issuing state (restart safety)", func() {
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{
				v1.RebootPreBootIDAnnotationKey: "boot-1",
			})
			node.Status.NodeInfo.BootID = "boot-2"
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			Expect(cloudProvider.RebootCalls).To(BeEmpty())
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonIssued))
		})

		It("waits to issue while pods still need draining", func() {
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.RebootTerminationGracePeriodAnnotationKey: "10m"})
			pod := test.Pod(test.PodOptions{NodeName: node.Name})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
			result := ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			Expect(cloudProvider.RebootCalls).To(BeEmpty())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonRequested))
		})

		It("leaves pods that can't be evicted on the node when a bounded drain expires", func() {
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.RebootTerminationGracePeriodAnnotationKey: "1m"})
			labels := map[string]string{test.RandomName(): test.RandomName()}
			pdbBlocked := test.Pod(test.PodOptions{
				NodeName:                      node.Name,
				ObjectMeta:                    metav1.ObjectMeta{Labels: labels},
				Phase:                         corev1.PodRunning,
				TerminationGracePeriodSeconds: lo.ToPtr[int64](30),
			})
			doNotDisrupt := test.Pod(test.PodOptions{
				NodeName:                      node.Name,
				ObjectMeta:                    metav1.ObjectMeta{Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"}},
				Phase:                         corev1.PodRunning,
				TerminationGracePeriodSeconds: lo.ToPtr[int64](30),
			})
			pdb := test.PodDisruptionBudget(test.PDBOptions{Labels: labels, MinAvailable: lo.ToPtr(intstr.FromInt32(1))})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pdbBlocked, doNotDisrupt, pdb)
			pods := []*corev1.Pod{pdbBlocked, doNotDisrupt}

			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			for _, pod := range pods {
				ExpectObjectReconciled(ctx, env.Client, queue, pod)
			}
			env.Clock.Step(45 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			for _, pod := range pods {
				ExpectObjectReconciled(ctx, env.Client, queue, pod)
				Expect(ExpectExists(ctx, env.Client, pod).DeletionTimestamp.IsZero()).To(BeTrue())
			}
			Expect(cloudProvider.RebootCalls).To(BeEmpty())

			env.Clock.Step(30 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			Expect(cloudProvider.RebootCalls).To(HaveLen(1))
			for _, pod := range pods {
				ExpectObjectReconciled(ctx, env.Client, queue, pod)
				Expect(queue.Has(pod)).To(BeFalse())
				Expect(ExpectExists(ctx, env.Client, pod).DeletionTimestamp.IsZero()).To(BeTrue())
			}
		})

		It("issues a forceful (0s) reboot immediately, leaving pods on the node", func() {
			pod := test.Pod(test.PodOptions{NodeName: node.Name, Phase: corev1.PodRunning})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			Expect(cloudProvider.RebootCalls).To(HaveLen(1))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonIssued))
			Expect(queue.Has(pod)).To(BeFalse())
			Expect(ExpectExists(ctx, env.Client, pod).DeletionTimestamp.IsZero()).To(BeTrue())
		})

		It("fails with provider_error when issuance does not succeed within the issuance timeout", func() {
			// First reconcile starts the issuance window; a later reconcile enforces the timeout.
			cloudProvider.NextRebootErr = fmt.Errorf("throttled")
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			stepPastDrainFloor()
			_ = ExpectObjectReconcileFailed(ctx, env.Client, rebootController, nodeClaim)
			Expect(cloudProvider.RebootCalls).To(BeEmpty())

			env.Clock.Step(6 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			Expect(cloudProvider.RebootCalls).To(BeEmpty())
			expectReplaced(nodeClaim, "provider_error")
		})

		It("re-seeds the issuance timer on restart and still enforces the timeout", func() {
			// Simulate restart with persisted issuing state but no in-memory timer.
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{
				v1.RebootPreBootIDAnnotationKey: "boot-1",
			})
			node.Status.NodeInfo.BootID = "boot-1"
			cloudProvider.NextRebootErr = fmt.Errorf("throttled")
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			// First reconcile re-seeds the issuance timer.
			_ = ExpectObjectReconcileFailed(ctx, env.Client, rebootController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonRequested))

			// The re-seeded timer still enforces the timeout.
			env.Clock.Step(6 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			expectReplaced(nodeClaim, "provider_error")
		})

		It("drains without a deadline when the reboot termination grace period is absent", func() {
			// Absent grace period means an unbounded graceful drain.
			delete(nodeClaim.Annotations, v1.RebootTerminationGracePeriodAnnotationKey)
			pod := test.Pod(test.PodOptions{NodeName: node.Name})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
			result := ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			Expect(result.RequeueAfter).To(Equal(15 * time.Second))

			env.Clock.Step(24 * time.Hour)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			Expect(cloudProvider.RebootCalls).To(BeEmpty())
			ExpectExists(ctx, env.Client, pod)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonRequested))
			Expect(recorder.Calls(events.RebootFailed)).To(Equal(0))

			// Once the node is empty, the reboot issues.
			ExpectDeleted(ctx, env.Client, pod)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			Expect(cloudProvider.RebootCalls).To(HaveLen(1))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonIssued))
		})

		It("fails when the node is gone before the drain and the issuance timeout elapses", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim) // node intentionally not applied
			result := ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(ExpectExists(ctx, env.Client, nodeClaim).StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonRequested))

			env.Clock.Step(6 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			expectReplaced(nodeClaim, "provider_error")
		})

		It("fails with invalid_request when the reboot termination grace period is malformed", func() {
			nodeClaim.Annotations[v1.RebootTerminationGracePeriodAnnotationKey] = "5min"
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			expectInvalidRequest(nodeClaim)
		})

		It("fails with invalid_request when the reboot termination grace period is negative", func() {
			nodeClaim.Annotations[v1.RebootTerminationGracePeriodAnnotationKey] = "-5m"
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			expectInvalidRequest(nodeClaim)
		})
	})

	Context("RebootIssued", func() {
		BeforeEach(func() {
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{
				v1.RebootPreBootIDAnnotationKey: "boot-1",
			})
			nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonIssued, "reboot issued")
			// issuedAt is derived from this transition.
			nodeClaim.StatusConditions().SetUnknownWithReason(v1.ConditionTypeInitialized, v1.RebootReasonRebooting, "node is rebooting")
			node.Spec.Taints = append(node.Spec.Taints, v1.RebootingNoScheduleTaint)
		})

		It("clears the initialized label in the Issued phase (self-heals a crash after the transition)", func() {
			// Simulate a crash after status was updated but before the label was cleared.
			node.Labels[v1.NodeInitializedLabelKey] = "true"
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			node = ExpectExists(ctx, env.Client, node)
			Expect(node.Labels).ToNot(HaveKey(v1.NodeInitializedLabelKey))
		})

		It("stays issued while the node has not rebooted", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			result := ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonIssued))
			node = ExpectExists(ctx, env.Client, node)
			Expect(hasRebootTaint(node)).To(BeTrue())
		})

		It("removes the fence and emits Observed when the boot changes but the node is not yet Ready", func() {
			node.Status.NodeInfo.BootID = "boot-2"
			node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			node = ExpectExists(ctx, env.Client, node)
			Expect(hasRebootTaint(node)).To(BeFalse())
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).IsTrue()).To(BeTrue())
			Expect(recorder.Calls(events.RebootObserved)).To(Equal(1))
		})

		It("records a success once when a stale cached NodeClaim is reconciled again", func() {
			node.Status.NodeInfo.BootID = "boot-2"
			node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			stale := ExpectExists(ctx, env.Client, nodeClaim).DeepCopy()
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			_, err := rebootController.Reconcile(ctx, stale)
			Expect(err).To(HaveOccurred())
			ExpectMetricCounterValue(reboot.RebootsTotal, 1, map[string]string{"result": "succeeded"})
			ExpectMetricHistogramSampleCountValue("karpenter_nodes_reboot_recovery_duration_seconds", 1, map[string]string{})
		})

		It("succeeds when the boot changed and the node is Ready", func() {
			node.Status.NodeInfo.BootID = "boot-2"
			node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting)
			Expect(cond.IsFalse()).To(BeTrue())
			Expect(cond.Reason).To(Equal(v1.RebootReasonSucceeded))
			node = ExpectExists(ctx, env.Client, node)
			Expect(hasRebootTaint(node)).To(BeFalse())

			ExpectMetricCounterValue(reboot.RebootsTotal, 1, map[string]string{"result": "succeeded"})
			ExpectMetricHistogramSampleCountValue("karpenter_nodes_reboot_duration_seconds", 1, map[string]string{"result": "succeeded"})
			ExpectMetricHistogramSampleCountValue("karpenter_nodes_reboot_recovery_duration_seconds", 1, map[string]string{})
		})

		It("fails with recovery_timeout when the observation window elapses without recovery", func() {
			env.Clock.Step(21 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			node = ExpectExists(ctx, env.Client, node)
			Expect(hasRebootTaint(node)).To(BeFalse())
			expectReplaced(nodeClaim, "recovery_timeout")
		})

		It("retries the replacement delete when it fails, rather than stranding the node", func() {
			env.Clock.Step(21 * time.Minute)
			nodeClaim.Finalizers = append(nodeClaim.Finalizers, "test.karpenter.sh/hold")
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			failingClient := &nodeClaimDeleteErrorClient{Client: env.Client, err: fmt.Errorf("injected delete failure")}
			failing := reboot.NewController(env.Clock, failingClient, cloudProvider, terminator.NewTerminator(env.Clock, env.Client, queue, recorder), recorder)
			_ = ExpectObjectReconcileFailed(ctx, env.Client, failing, nodeClaim)

			// Failed delete leaves the reboot active so replacement can be retried.
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp.IsZero()).To(BeTrue())
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).IsTrue()).To(BeTrue())
			Expect(recorder.Calls(events.RebootFailed)).To(Equal(0))

			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			expectReplaced(nodeClaim, "recovery_timeout")

			// Once deleting, termination owns the NodeClaim.
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)
			Expect(recorder.Calls(events.RebootFailed)).To(Equal(1))
		})

		It("stamps the reboot drain bound as the termination deadline when escalating to replacement", func() {
			nodeClaim.Annotations[v1.RebootTerminationGracePeriodAnnotationKey] = "5m0s"
			nodeClaim.Finalizers = append(nodeClaim.Finalizers, "test.karpenter.sh/hold")
			env.Clock.Step(21 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			expectReplaced(nodeClaim, "recovery_timeout")
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Add(5*time.Minute).Format(time.RFC3339)))
			ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
		})

		It("does not extend an earlier termination deadline when escalating to replacement", func() {
			env.Clock.Step(21 * time.Minute)
			earlier := env.Clock.Now().Add(time.Minute).Format(time.RFC3339)
			nodeClaim.Annotations[v1.RebootTerminationGracePeriodAnnotationKey] = "5m0s"
			nodeClaim.Annotations[v1.NodeClaimTerminationTimestampAnnotationKey] = earlier
			nodeClaim.Finalizers = append(nodeClaim.Finalizers, "test.karpenter.sh/hold")
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			expectReplaced(nodeClaim, "recovery_timeout")
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, earlier))
			ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
		})

		It("stamps no termination deadline when escalating an unbounded reboot", func() {
			delete(nodeClaim.Annotations, v1.RebootTerminationGracePeriodAnnotationKey)
			nodeClaim.Finalizers = append(nodeClaim.Finalizers, "test.karpenter.sh/hold")
			env.Clock.Step(21 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			expectReplaced(nodeClaim, "recovery_timeout")
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).ToNot(HaveKey(v1.NodeClaimTerminationTimestampAnnotationKey))
			ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
		})

		It("fails when the node is gone and the deadline has elapsed", func() {
			env.Clock.Step(21 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			expectReplaced(nodeClaim, "recovery_timeout")
		})

		It("keeps polling when the node is gone but the deadline has not elapsed", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			result := ExpectObjectReconciled(ctx, env.Client, rebootController, nodeClaim)

			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonIssued))
		})
	})
})
