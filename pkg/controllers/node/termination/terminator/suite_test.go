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

package terminator_test

import (
	"context"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var env *test.Environment
var recorder *test.EventRecorder
var queue *terminator.Queue
var pdb *policyv1.PodDisruptionBudget
var pod *corev1.Pod
var node *corev1.Node
var terminatorInstance *terminator.Terminator

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Eviction")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(
		test.WithCRDs(apis.CRDs...),
		test.WithCRDs(v1alpha1.CRDs...),
		test.WithFieldIndexers(test.NodeClaimProviderIDFieldIndexer(ctx)),
	)
	ctx = options.ToContext(ctx, test.Options())
	recorder = test.NewEventRecorder()
	queue = terminator.NewQueue(env.Client, recorder)
	terminatorInstance = terminator.NewTerminator(env.Clock, env.Client, queue, recorder)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	recorder.Reset() // Reset the events that we captured during the run
	// Shut down the queue and restart it to ensure no races
	*queue = lo.FromPtr(terminator.NewQueue(env.Client, recorder))
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var testLabels = map[string]string{"test": "label"}

var _ = Describe("Eviction/Queue", func() {
	BeforeEach(func() {
		pdb = test.PodDisruptionBudget(test.PDBOptions{
			Labels:         testLabels,
			MaxUnavailable: &intstr.IntOrString{IntVal: 0},
		})
		pod = test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: testLabels,
			},
		})
		node = test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Name: pod.Spec.NodeName, Namespace: pod.Namespace}, ProviderID: "my-provider"})

		terminator.PodsEvictionRequestsTotal.Reset()
		terminator.PodsDrainedTotal.Reset()
	})

	Context("Eviction API", func() {
		It("should succeed with no event when the pod is not found", func() {
			ExpectObjectReconciled(ctx, env.Client, queue, pod)
			Expect(recorder.Events()).To(HaveLen(0))
		})
		It("should succeed with no event when the pod UID conflicts", func() {
			ExpectApplied(ctx, env.Client, pod, node)
			queue.Add(pod)
			pod.UID = uuid.NewUUID()
			Expect(queue.Has(pod)).To(BeFalse())
		})
		It("should succeed with an evicted event when there are no PDBs", func() {
			ExpectApplied(ctx, env.Client, pod, node)
			queue.Add(pod)
			ExpectObjectReconciled(ctx, env.Client, queue, pod)
			ExpectMetricCounterValue(terminator.PodsEvictionRequestsTotal, 1, map[string]string{terminator.CodeLabel: "200"})
			Expect(recorder.Calls(events.Evicted)).To(Equal(1))
		})
		It("should succeed with no event when there are PDBs that allow an eviction", func() {
			pdb = test.PodDisruptionBudget(test.PDBOptions{
				Labels:         testLabels,
				MaxUnavailable: &intstr.IntOrString{IntVal: 1},
			})
			ExpectApplied(ctx, env.Client, pod, node)
			queue.Add(pod)
			ExpectObjectReconciled(ctx, env.Client, queue, pod)
			Expect(recorder.Calls(events.Evicted)).To(Equal(1))
		})
		It("should return a NodeDrainError event when a PDB is blocking", func() {
			ExpectApplied(ctx, env.Client, pdb, pod, node)
			ExpectManualBinding(ctx, env.Client, pod, node)
			queue.Add(pod)
			result := ExpectObjectReconciled(ctx, env.Client, queue, pod)
			//nolint:staticcheck
			Expect(result.Requeue).To(BeTrue())
			ExpectMetricCounterValue(terminator.PodsEvictionRequestsTotal, 1, map[string]string{terminator.CodeLabel: "429"})
			e := recorder.Events()
			Expect(e).To(HaveLen(1))
			Expect(e[0].Reason).To(Equal(events.FailedDraining))
			Expect(e[0].Message).To(ContainSubstring("evicting pod violates a PDB"))
		})
		It("should return a NodeDrainError event when two PDBs refer to the same pod", func() {
			pdb2 := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         testLabels,
				MaxUnavailable: &intstr.IntOrString{IntVal: 0},
			})
			ExpectApplied(ctx, env.Client, pdb, pdb2, pod, node)
			ExpectManualBinding(ctx, env.Client, pod, node)
			queue.Add(pod)
			result := ExpectObjectReconciled(ctx, env.Client, queue, pod)
			//nolint:staticcheck
			Expect(result.Requeue).To(BeTrue())
			ExpectMetricCounterValue(terminator.PodsEvictionRequestsTotal, 1, map[string]string{terminator.CodeLabel: "500"})
			e := recorder.Events()
			Expect(e).To(HaveLen(1))
			Expect(e[0].Reason).To(Equal(events.FailedDraining))
			Expect(e[0].Message).To(ContainSubstring("eviction does not support multiple PDBs"))
		})
		It("should ensure that calling Evict() is valid while making Add() calls", func() {
			// Ensure that we add enough pods to the queue while we are pulling items off of the queue (enough to trigger a DATA RACE)
			pods := test.Pods(1000)
			cancelContext, cancelFunc := context.WithCancel(ctx)
			defer cancelFunc()

			for _, pod := range pods {
				go func() {
					for {
						if cancelContext.Err() != nil {
							return
						}
						queue.Add(pod)
					}
				}()
			}

			for _, pod = range pods {
				ExpectObjectReconciled(ctx, env.Client, queue, pod)
			}

		})
		It("should increment PodsDrainedTotal metric when a pod is evicted", func() {
			ExpectApplied(ctx, env.Client, pod, node)
			queue.Add(pod)
			ExpectObjectReconciled(ctx, env.Client, queue, pod)
			ExpectMetricCounterValue(terminator.PodsDrainedTotal, 1, map[string]string{terminator.ReasonLabel: ""})
			ExpectMetricCounterValue(terminator.PodsEvictionRequestsTotal, 1, map[string]string{terminator.CodeLabel: "200"})
			Expect(recorder.Calls(events.Evicted)).To(Equal(1))
		})
		It("should increment PodsDrainedTotal metric with specific reason when a pod is evicted", func() {
			nodeClaim := test.NodeClaim(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-nodeclaim",
				},
				Status: v1.NodeClaimStatus{
					ProviderID: node.Spec.ProviderID,
					NodeName:   node.Name,
				},
			})
			nodeClaim.StatusConditions().Set(status.Condition{
				Type:    v1.ConditionTypeDisruptionReason,
				Status:  metav1.ConditionTrue,
				Reason:  "SpotInterruption",
				Message: "Node is being interrupted",
			})

			ExpectApplied(ctx, env.Client, nodeClaim, node, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)
			queue.Add(pod)
			ExpectObjectReconciled(ctx, env.Client, queue, pod)

			ExpectMetricCounterValue(terminator.PodsDrainedTotal, 1, map[string]string{terminator.ReasonLabel: "SpotInterruption"})
			ExpectMetricCounterValue(terminator.PodsEvictionRequestsTotal, 1, map[string]string{terminator.CodeLabel: "200"})
			Expect(recorder.Calls(events.Evicted)).To(Equal(1))
		})
	})

	Context("Pod Deletion API", func() {
		It("should not delete a pod with no nodeTerminationTime", func() {
			ExpectApplied(ctx, env.Client, pod, node)

			Expect(terminatorInstance.DeleteExpiringPods(ctx, []*corev1.Pod{pod}, nil)).To(Succeed())
			ExpectExists(ctx, env.Client, pod)
			Expect(recorder.Calls(events.Disrupted)).To(Equal(0))
		})
		It("should not delete a pod with terminationGracePeriodSeconds still remaining before nodeTerminationTime", func() {
			pod.Spec.TerminationGracePeriodSeconds = lo.ToPtr[int64](60)
			ExpectApplied(ctx, env.Client, pod, node)

			nodeTerminationTime := time.Now().Add(time.Minute * 5)
			Expect(terminatorInstance.DeleteExpiringPods(ctx, []*corev1.Pod{pod}, &nodeTerminationTime)).To(Succeed())
			ExpectExists(ctx, env.Client, pod)
			Expect(recorder.Calls(events.Disrupted)).To(Equal(0))
		})
		It("should delete a pod with less than terminationGracePeriodSeconds remaining before nodeTerminationTime", func() {
			pod.Spec.TerminationGracePeriodSeconds = lo.ToPtr[int64](120)
			ExpectApplied(ctx, env.Client, pod)

			nodeTerminationTime := time.Now().Add(time.Minute * 1)
			Expect(terminatorInstance.DeleteExpiringPods(ctx, []*corev1.Pod{pod}, &nodeTerminationTime)).To(Succeed())
			ExpectNotFound(ctx, env.Client, pod)
			Expect(recorder.Calls(events.Disrupted)).To(Equal(1))
		})
		It("should clamp gracePeriodSeconds to >= 1 when nodeTerminationTime is in the past", func() {
			// Use a finalizer so envtest does not immediately garbage-collect the pod after the
			// delete call. Without a finalizer, the pod disappears before we can read back the
			// DeletionTimestamp that the API server stamped on it, making it impossible to
			// verify that the clamp produced a graceful delete (not a force-delete).
			pod.Spec.TerminationGracePeriodSeconds = lo.ToPtr[int64](120)
			pod.Finalizers = []string{"karpenter.sh/test-finalizer"}
			ExpectApplied(ctx, env.Client, pod)
			DeferCleanup(func() {
				// Remove the finalizer so AfterEach's ExpectCleanedUp can delete the pod.
				ExpectFinalizersRemoved(ctx, env.Client, pod)
			})

			// Set the termination time 1 hour in the past: remaining = -3600s.
			// Without the clamp the grace period would be <=0, sending gracePeriodSeconds=0
			// to the API (force-delete). The clamp must produce >= 1.
			pastTerminationTime := env.Clock.Now().Add(-1 * time.Hour)
			Expect(terminatorInstance.DeleteExpiringPods(ctx, []*corev1.Pod{pod}, &pastTerminationTime)).To(Succeed())

			// Verify the delete was graceful (not a force-delete): the Disrupted event must have
			// been published and contain "1 seconds of grace-period" in the message, proving the
			// clamp produced gracePeriodSeconds=1 (not 0).
			// Note: DeletionGracePeriodSeconds on the re-fetched pod may be 0 because the API
			// server recomputes it as floor(DeletionTimestamp-now()); with a 1s grace period it
			// decays to 0 during the round-trip. The event message is captured at delete time.
			Expect(recorder.Calls(events.Disrupted)).To(Equal(1))
			evts := recorder.Events()
			Expect(evts).To(HaveLen(1))
			Expect(evts[0].Message).To(ContainSubstring("granted 1 seconds of grace-period"))

			// Also verify the pod is terminating (DeletionTimestamp set), not force-deleted
			// (force-delete would bypass the graceful termination and remove the object immediately
			// even with a finalizer in older Kubernetes; a set DeletionTimestamp confirms graceful).
			pod = ExpectExists(ctx, env.Client, pod)
			Expect(pod.DeletionTimestamp).ToNot(BeNil())
		})
	})
})
