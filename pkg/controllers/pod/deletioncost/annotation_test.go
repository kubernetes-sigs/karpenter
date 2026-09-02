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

package deletioncost_test

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// throttlingClient wraps a client.Client and returns 429 TooManyRequests for
// the first N Patch calls. Used to verify the queue's Reconcile surfaces
// retryable errors to controller-runtime.
type throttlingClient struct {
	client.Client
	remaining atomic.Int64
}

func newThrottlingClient(inner client.Client, throttleFirst int) *throttlingClient {
	tc := &throttlingClient{Client: inner}
	tc.remaining.Store(int64(throttleFirst))
	return tc
}

func (c *throttlingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.remaining.Add(-1) >= 0 {
		return apierrors.NewTooManyRequests("throttled by test client", 1)
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// blockingPatchClient blocks the first Patch invocation in-flight until the
// caller closes proceed, signaling test setup that Patch has entered via the
// entered channel. Subsequent Patch calls pass through unblocked. Used to
// drive queue-contention specs where a caller must observe the reconcile
// loop mid-Patch to exercise concurrent Add/Reconcile interleavings.
type blockingPatchClient struct {
	client.Client
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (c *blockingPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.once.Do(func() { close(c.entered) })
	<-c.proceed
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// countingClient wraps a client.Client and counts Patch invocations.
type countingClient struct {
	client.Client
	mu    sync.Mutex
	count int
}

func (c *countingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *countingClient) PatchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// enqueueAndReconcile is the standard test flow: enqueue a pod on the shared
// suite queue and immediately drive the queue's Reconcile against it. Returns
// after a single Reconcile, mirroring what controller-runtime does per work
// item.
func enqueueAndReconcile(pod *corev1.Pod, rank int, clear bool) {
	GinkgoHelper()
	queue.Add(pod, rank, clear)
	ExpectObjectReconciled(ctx, env.Client, queue, pod)
}

var _ = Describe("Annotation", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
	})

	Context("Pod-deletion-cost write path", func() {
		It("should add the pod-deletion-cost annotation to pods without it", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			const rank = -10
			enqueueAndReconcile(pod, rank, false)

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).To(HaveKeyWithValue(corev1.PodDeletionCost, strconv.Itoa(rank)))
		})

		It("should overwrite customer-set pod-deletion-cost values", func() {
			// v4 RFC: gate-ON = user is OK with Karpenter managing PDC. No
			// overwrite-protection.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{corev1.PodDeletionCost: "100"}},
				NodeName:   nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			const rank = -10
			enqueueAndReconcile(pod, rank, false)

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(strconv.Itoa(rank)))
		})

		It("should update existing pod-deletion-cost values to the new rank", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{corev1.PodDeletionCost: "-5"}},
				NodeName:   nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			const rank = -20
			enqueueAndReconcile(pod, rank, false)

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(strconv.Itoa(rank)))
		})

		It("should update multiple pods on the same node with the same rank", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pods := make([]*corev1.Pod, 3)
			for i := range pods {
				pods[i] = rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
				ExpectApplied(ctx, env.Client, pods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			const rank = -7
			for _, pod := range pods {
				enqueueAndReconcile(pod, rank, false)
			}

			for _, pod := range pods {
				updatedPod := &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
				Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(strconv.Itoa(rank)))
			}
		})

		It("should update pods across multiple ranked nodes", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod0 := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			pod1 := rsOwnedPod(test.PodOptions{NodeName: nodes[1].Name})
			ExpectApplied(ctx, env.Client, pod0, pod1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			enqueueAndReconcile(pod0, -10, false)
			enqueueAndReconcile(pod1, -9, false)

			updated0 := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod0), updated0)).To(Succeed())
			Expect(updated0.Annotations[corev1.PodDeletionCost]).To(Equal("-10"))
			updated1 := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod1), updated1)).To(Succeed())
			Expect(updated1.Annotations[corev1.PodDeletionCost]).To(Equal("-9"))
		})
	})

	Context("Queue semantics", func() {
		It("should skip the API call when the pod's annotation already matches the desired value", func() {
			// The queue's Reconcile checks matchesDesired before patching so
			// the reconcile completes without a Patch call. Count via a
			// counting client that wraps env.Client and drives it as the
			// queue's kubeClient.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			const rank = -19
			pod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{corev1.PodDeletionCost: strconv.Itoa(rank)}},
				NodeName:   nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			counter := &countingClient{Client: env.Client}
			q := deletioncost.NewQueue(counter)
			q.Add(pod, rank, false)
			ExpectObjectReconciled(ctx, env.Client, q, pod)

			Expect(counter.PatchCount()).To(Equal(0))
		})

		It("should surface 429 errors from Reconcile so controller-runtime can retry", func() {
			// Under the queue swap the per-pod retry loop is gone; controller-
			// runtime's rate limiter re-enqueues on error. Verify the queue
			// returns the raw 429 error rather than swallowing it or classifying
			// it as skipped.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			throttler := newThrottlingClient(env.Client, 1)
			q := deletioncost.NewQueue(throttler)
			q.Add(pod, -11, false)
			err := ExpectObjectReconcileFailed(ctx, env.Client, q, pod)
			Expect(apierrors.IsTooManyRequests(err)).To(BeTrue())
			// The item stays enqueued on retryable errors so controller-runtime
			// picks it back up on its next tick.
			Expect(q.Has(pod)).To(BeTrue())

			// Second reconcile succeeds now that the throttler is drained.
			ExpectObjectReconciled(ctx, env.Client, q, pod)
			Expect(q.Has(pod)).To(BeFalse())

			updated := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updated)).To(Succeed())
			Expect(updated.Annotations[corev1.PodDeletionCost]).To(Equal("-11"))
		})

		It("should treat 409 Conflict on the patch as skipped and drop the item from the queue", func() {
			// A racing writer bumps the live pod's ResourceVersion after the
			// queue captured its snapshot. MergeFromWithOptimisticLock sends
			// the snapshot's stale RV as a precondition, and the apiserver
			// responds with 409 Conflict. The queue must treat that as
			// terminal (drop the item, return nil) so controller-runtime does
			// not retry a permanently-lost race. Mirrors the NotFound spec
			// below.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			// Snapshot the pod at its current ResourceVersion, then bump the
			// live pod so a stale-RV patch will 409.
			snapshot := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), snapshot)).To(Succeed())
			live := snapshot.DeepCopy()
			if live.Labels == nil {
				live.Labels = map[string]string{}
			}
			live.Labels["racing-writer"] = "true"
			Expect(env.Client.Update(ctx, live)).To(Succeed())

			queue.Add(snapshot, -1, false)
			result, err := queue.Reconcile(ctx, snapshot)
			Expect(err).ToNot(HaveOccurred(), "409 must not surface as an error; the queue treats it as terminal")
			Expect(result).To(BeZero())
			Expect(queue.Has(snapshot)).To(BeFalse(), "queue must drop the item after Conflict")

			// Live state preserved: the racing writer's label update stuck,
			// and the queue's stale-RV patch never landed the annotation.
			updated := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updated)).To(Succeed())
			Expect(updated.Annotations).ToNot(HaveKey(corev1.PodDeletionCost),
				"racing writer won; the queue's stale-RV patch must not have applied the annotation")
			Expect(updated.Labels).To(HaveKeyWithValue("racing-writer", "true"),
				"racing writer's label update must be preserved")
		})

		It("should treat NotFound on the patch as skipped and drop the item from the queue", func() {
			// Apply a pod, enqueue it, then delete it before Reconcile runs.
			// The Patch call now 404s and the queue must treat that as
			// terminal (drop the item, return nil) so controller-runtime does
			// not retry a ghost pod indefinitely.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			// Snapshot the applied pod so it carries a ResourceVersion (required
			// by the optimistic-lock merge patch). Then delete it and call
			// Reconcile with the snapshot — the Patch API call will 404.
			live := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), live)).To(Succeed())
			queue.Add(live, -1, false)
			Expect(env.Client.Delete(ctx, live)).To(Succeed())
			_, err := queue.Reconcile(ctx, live)
			Expect(err).ToNot(HaveOccurred())
			Expect(queue.Has(live)).To(BeFalse())
		})

		It("should treat a UID mismatch as a race and drop the reconcile silently", func() {
			// The queue is keyed by (namespace, name, UID). A pod that arrives
			// through the fetch adapter with a different UID than what was
			// enqueued is a different pod entirely and must exit without
			// evicting or annotating.
			pod := rsOwnedPod(test.PodOptions{})
			queue.Add(pod, -1, false)
			Expect(queue.Has(pod)).To(BeTrue())

			// Fabricate a same-name replacement with a fresh UID: the map
			// lookup misses and Reconcile returns nil.
			replacement := pod.DeepCopy()
			replacement.UID = "different-uid"
			result, err := queue.Reconcile(ctx, replacement)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(BeZero())
			// Original entry still enqueued for its own eventual reconcile.
			Expect(queue.Has(pod)).To(BeTrue())
		})

		It("should collapse repeated Adds for the same pod into a single reconcile with the latest state", func() {
			// Overwrite-on-Add: enqueue with rank -1, then -5, then Reconcile
			// once. The persisted annotation reflects the latest add.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			queue.Add(pod, -1, false)
			queue.Add(pod, -5, false)
			ExpectObjectReconciled(ctx, env.Client, queue, pod)

			updated := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updated)).To(Succeed())
			Expect(updated.Annotations[corev1.PodDeletionCost]).To(Equal("-5"))
		})

		// PENDING: The current Queue implementation cannot preserve a mid-flight
		// Add(pod, newRank) when the Add races an in-progress Reconcile. Add's
		// "no source push when already enqueued" combined with complete()'s
		// unconditional delete drops the newer desired state — the queue is
		// empty after the racing Reconcile returns, and controller-runtime is
		// never told to re-enqueue the pod. The 60s Controller.Reconcile cycle
		// re-Adds and eventually converges, so real-world impact is bounded,
		// but the queue itself does not guarantee lossless mid-flight updates.
		//
		// This spec asserts the intended lossless behavior. Un-Pending it once
		// the queue is repaired (e.g. always push to source, or version-check
		// during complete). See gc-a82ehs report for the trace + design options.
		PIt("should preserve a mid-flight Add's desired state so the next reconcile lands the newer value", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			blocker := &blockingPatchClient{
				Client:  env.Client,
				entered: make(chan struct{}),
				proceed: make(chan struct{}),
			}
			q := deletioncost.NewQueue(blocker)
			q.Add(pod, -1, false)

			done := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				_, err := q.Reconcile(ctx, pod)
				done <- err
			}()

			// Wait for the first Patch to be in flight, then update the
			// desired state before it returns.
			Eventually(blocker.entered).WithTimeout(5 * time.Second).Should(BeClosed())
			q.Add(pod, -5, false)
			close(blocker.proceed)
			Expect(<-done).ToNot(HaveOccurred())

			// The queue must still have work outstanding for the pod; the
			// mid-flight Add published a newer desired state that has not
			// yet been persisted to the apiserver. Drain the queue against
			// the pod to land the -5.
			Expect(q.Has(pod)).To(BeTrue(),
				"mid-flight Add(pod,-5) must leave the pod enqueued so the next Reconcile picks up the newer desired state")
			ExpectObjectReconciled(ctx, env.Client, q, pod)

			updated := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updated)).To(Succeed())
			Expect(updated.Annotations[corev1.PodDeletionCost]).To(Equal("-5"),
				"final annotation should reflect the latest Add, not the value written by the racing patch")
		})

		It("should leave every pod enqueued when a shared throttle window rejects each pod's first patch", func() {
			// Two pods, both draining through a throttler primed to reject
			// the next two Patch calls. Each pod's Reconcile surfaces a 429,
			// so both entries stay in the map for controller-runtime's next
			// tick. Guards against a regression where a shared-error path
			// might accidentally clear other pods' entries.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			podA := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			podB := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, podA, podB)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			throttler := newThrottlingClient(env.Client, 2)
			q := deletioncost.NewQueue(throttler)
			q.Add(podA, -3, false)
			q.Add(podB, -3, false)

			// Drive both reconciles in parallel through the shared throttle.
			var wg sync.WaitGroup
			wg.Add(2)
			errs := make(chan error, 2)
			for _, p := range []*corev1.Pod{podA, podB} {
				go func(pod *corev1.Pod) {
					defer GinkgoRecover()
					defer wg.Done()
					err := ExpectObjectReconcileFailed(ctx, env.Client, q, pod)
					errs <- err
				}(p)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				Expect(apierrors.IsTooManyRequests(err)).To(BeTrue(),
					"each parallel reconcile should surface the throttler's 429 rather than a mismatched error")
			}
			Expect(q.Has(podA)).To(BeTrue(), "podA should stay enqueued after the throttled failure")
			Expect(q.Has(podB)).To(BeTrue(), "podB should stay enqueued after the throttled failure")
		})
	})

	Context("Group D (do-not-disrupt) annotation clearing", func() {
		It("should clear the pod-deletion-cost annotation on do-not-disrupt nodes", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{corev1.PodDeletionCost: "5"}},
				NodeName:   nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			enqueueAndReconcile(pod, 0, true)

			updated := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updated)).To(Succeed())
			Expect(updated.Annotations).ToNot(HaveKey(corev1.PodDeletionCost))
		})

		It("should skip pods without annotations on do-not-disrupt nodes", func() {
			// Already-cleared pods take the matchesDesired short-circuit and
			// issue no Patch call.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			counter := &countingClient{Client: env.Client}
			q := deletioncost.NewQueue(counter)
			q.Add(pod, 0, true)
			ExpectObjectReconciled(ctx, env.Client, q, pod)

			Expect(counter.PatchCount()).To(Equal(0))
			updated := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updated)).To(Succeed())
			Expect(updated.Annotations).ToNot(HaveKey(corev1.PodDeletionCost))
		})
	})
})
