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
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// throttlingClient wraps a client.Client and returns 429 TooManyRequests for
// the first N Patch calls, then delegates normally. Used to exercise the
// per-pod retry-with-backoff path in applyRankToPods / clearRanksFromPods.
type throttlingClient struct {
	client.Client
	remaining atomic.Int64
	seen      atomic.Int64
}

func newThrottlingClient(inner client.Client, throttleFirst int) *throttlingClient {
	tc := &throttlingClient{Client: inner}
	tc.remaining.Store(int64(throttleFirst))
	return tc
}

func (c *throttlingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.seen.Add(1)
	// Serve 429 for the first N calls, then fall through.
	if c.remaining.Add(-1) >= 0 {
		return apierrors.NewTooManyRequests("throttled by test client", 1)
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// countingClient wraps a client.Client and counts Patch invocations so tests
// can verify parallel dispatch actually issued the expected number of calls.
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

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			const rank = -10
			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], rank, false)}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).To(HaveKeyWithValue(corev1.PodDeletionCost, strconv.Itoa(rank)))
		})

		It("should overwrite customer-set pod-deletion-cost values", func() {
			// v4 RFC: gate-ON state means the user is OK with Karpenter managing the
			// pod-deletion-cost annotation. There is no overwrite-protection.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			pod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						corev1.PodDeletionCost: "100",
					},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			const rank = -10
			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], rank, false)}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

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
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						corev1.PodDeletionCost: "-5",
					},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			const rank = -20
			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], rank, false)}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(strconv.Itoa(rank)))
		})

		It("should handle pods without any annotations", func() {
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

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			const rank = -3
			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], rank, false)}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

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

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			const rank = -7
			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], rank, false)}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			for _, pod := range pods {
				updatedPod := &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
				Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(strconv.Itoa(rank)))
			}
		})

		It("should handle nodes with no pods", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], -1, false)}
			// Should not error even with no pods
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())
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

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}
			Expect(stateNodes).To(HaveLen(2))

			nodeRanks := []deletioncost.NodeRank{
				nodeRankWithPods(stateNodes[0], -10, false),
				nodeRankWithPods(stateNodes[1], -9, false),
			}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			// Both pods should have their respective node's rank
			for _, sn := range stateNodes {
				var expectedRank string
				for _, nr := range nodeRanks {
					if nr.Node.Node.Name == sn.Node.Name {
						expectedRank = fmt.Sprintf("%d", nr.Rank)
					}
				}
				pods, err := sn.Pods(ctx, env.Client)
				Expect(err).ToNot(HaveOccurred())
				for _, p := range pods {
					updatedPod := &corev1.Pod{}
					Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(p), updatedPod)).To(Succeed())
					Expect(updatedPod.Annotations[corev1.PodDeletionCost]).To(Equal(expectedRank))
				}
			}
		})
	})

	Context("Parallelize and 429 backoff", func() {
		It("should patch many pods on a node via the parallel workqueue", func() {
			// Fan out enough pods to exceed podPatchWorkers (10) so we
			// exercise the workqueue's own scheduling, not just a serial
			// pass. All patches must complete under a single
			// UpdatePodDeletionCosts call.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("32"), corev1.ResourceMemory: resource.MustParse("64Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			const numPods = 25
			pods := make([]*corev1.Pod, numPods)
			for i := range pods {
				pods[i] = rsOwnedPod(test.PodOptions{NodeName: nodes[0].Name})
				ExpectApplied(ctx, env.Client, pods[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			counter := &countingClient{Client: env.Client}
			const rank = -42
			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], rank, false)}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, counter, nodeRanks)).To(Succeed())

			// Every pod got exactly one patch through the counting client
			// (25 pods, 25 patches). The parallel dispatch cannot skip pods.
			Expect(counter.PatchCount()).To(Equal(numPods))
			for _, pod := range pods {
				updatedPod := &corev1.Pod{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
				Expect(updatedPod.Annotations).To(HaveKeyWithValue(corev1.PodDeletionCost, strconv.Itoa(rank)))
			}
		})

		It("should retry 429 responses per-pod and eventually succeed", func() {
			// Inject a throttling client that serves 429 for the first two
			// patch calls, then falls through. The per-pod retry loop
			// retries with podPatchRetryBackoff and should converge before
			// the outer function returns.
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

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			// Throttle the first 2 patches; the third and later succeed.
			throttler := newThrottlingClient(env.Client, 2)

			const rank = -11
			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], rank, false)}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, throttler, nodeRanks)).To(Succeed())

			// The patch retried at least twice (2 throttled attempts + 1
			// success = 3 total). Some slack for jitter/reordering.
			Expect(throttler.seen.Load()).To(BeNumerically(">=", int64(3)))

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).To(HaveKeyWithValue(corev1.PodDeletionCost, strconv.Itoa(rank)))
		})

		It("should give up after exhausting per-pod retries and return an error", func() {
			// Throttle enough calls to blow through the entire retry budget
			// (podPatchRetryBackoff.Steps is 4, so 5 calls exhausts it).
			// Set the throttle count high enough that no attempt succeeds.
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

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			// Throttle any conceivable retry attempt.
			throttler := newThrottlingClient(env.Client, 100)

			const rank = -12
			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], rank, false)}
			// The unretried 429 falls out of retry.OnError once the backoff
			// steps are exhausted, and 429 is not a NotFound/Conflict, so it
			// becomes a pod-level error and surfaces to the caller.
			err := deletioncost.UpdatePodDeletionCosts(ctx, throttler, nodeRanks)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsTooManyRequests(err)).To(BeTrue())

			// The pod was not updated (all retries throttled).
			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).ToNot(HaveKey(corev1.PodDeletionCost))
		})

		It("should skip the API call when the pod's annotation already matches the desired value", func() {
			// applyRankToPods computes value = strconv.Itoa(rank) once per
			// node and passes it into needsUpdate. When the current
			// annotation already equals the pre-computed value, needsUpdate
			// returns false and the patch never fires. Verify by counting
			// patches through a countingClient: a pod whose current value
			// matches the desired rank should not be patched.
			nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
				Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			const rank = -19
			// Pod already has the exact desired value.
			pod := rsOwnedPod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{corev1.PodDeletionCost: strconv.Itoa(rank)}},
				NodeName:   nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			counter := &countingClient{Client: env.Client}
			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], rank, false)}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, counter, nodeRanks)).To(Succeed())

			// needsUpdate short-circuited; no patch was issued.
			Expect(counter.PatchCount()).To(Equal(0))
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
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						corev1.PodDeletionCost: "5",
					},
				},
				NodeName: nodes[0].Name,
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], 10, true)}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).ToNot(HaveKey(corev1.PodDeletionCost))
		})

		It("should skip pods without annotations on do-not-disrupt nodes", func() {
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

			var stateNodes []*state.StateNode
			for n := range cluster.Nodes() {
				stateNodes = append(stateNodes, n)
			}

			nodeRanks := []deletioncost.NodeRank{nodeRankWithPods(stateNodes[0], 10, true)}
			Expect(deletioncost.UpdatePodDeletionCosts(ctx, env.Client, nodeRanks)).To(Succeed())

			updatedPod := &corev1.Pod{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
			Expect(updatedPod.Annotations).ToNot(HaveKey(corev1.PodDeletionCost))
		})
	})
})
