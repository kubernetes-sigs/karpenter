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

// Orchestration tests for the ephemeral latch: exercise updateEphemeralFulfillment end to end
// (list pods -> match/sum -> resolve intended -> Status().Patch) against a fake client and a
// fake clock. Internal package so it can construct a minimal *Provisioner and call the method
// directly, without booting the full scheduling suite.

package provisioning

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/state/virtualpods"
)

var _ = Describe("updateEphemeralFulfillment", func() {
	var (
		ctx   context.Context
		fc    *clocktesting.FakeClock
		start time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		fc = clocktesting.NewFakeClock(start)
	})

	// newProv builds a minimal Provisioner with only the deps updateEphemeralFulfillment needs,
	// including a real virtual-pod cache so we can assert immediate eviction on latch.
	newProv := func(objs ...client.Object) (*Provisioner, client.Client, *virtualpods.Cache) {
		c := fakecr.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(objs...).
			WithStatusSubresource(&autoscalingv1beta1.CapacityBuffer{}).
			Build()
		cache := virtualpods.NewVirtualPodCache(c)
		return &Provisioner{kubeClient: c, clock: fc, virtualPodCache: cache}, c, cache
	}

	// ephemeralBufferReady builds a ready ephemeral buffer (ReadyForProvisioning=True, replicas set)
	// referencing a "<name>-template" PodTemplate, with the given annotations.
	ephemeralBufferReady := func(name string, replicas int32, anns map[string]string) *autoscalingv1beta1.CapacityBuffer {
		refill := autoscalingv1beta1.RefillStrategyNone
		cb := &autoscalingv1beta1.CapacityBuffer{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: anns},
			Spec: autoscalingv1beta1.CapacityBufferSpec{
				RefillStrategy: &refill,
				PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: name + "-template"},
				Replicas:       lo.ToPtr(replicas),
			},
			Status: autoscalingv1beta1.CapacityBufferStatus{
				Replicas:       lo.ToPtr(replicas),
				PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: name + "-template"},
				Conditions: []metav1.Condition{{
					Type:               autoscalingv1beta1.ReadyForProvisioningCondition,
					Status:             metav1.ConditionTrue,
					Reason:             "Resolved",
					LastTransitionTime: metav1.NewTime(start),
				}},
			},
		}
		return cb
	}

	// template1cpu is the PodTemplate a "<name>-template" ref resolves to: one 1-CPU container.
	//nolint:unparam // test helper kept general; name is constant across current callers
	template1cpu := func(name string) *corev1.PodTemplate {
		return &corev1.PodTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-template", Namespace: "default"},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "pause:v1",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
						},
					}},
				},
			},
		}
	}

	// matchingPod builds a pod labeled app=gang, requesting cpu, optionally bound to a node. A bound
	// pod carries a PodScheduled condition transitioned just after the buffer's readiness (start), so
	// it counts past the readiness boundary; use matchingPodBoundAt to place the binding earlier.
	matchingPod := func(name, cpu, nodeName string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"app": "gang"}},
			Spec: corev1.PodSpec{
				NodeName: nodeName,
				Containers: []corev1.Container{{
					Name:      "c",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
				}},
			},
		}
		if nodeName != "" {
			pod.Status.Conditions = []corev1.PodCondition{{
				Type:               corev1.PodScheduled,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(start.Add(time.Minute)),
			}}
		}
		return pod
	}

	// matchingPodBoundAt is matchingPod with the PodScheduled transition set to a specific time,
	// used to exercise the readiness boundary (pods bound before the buffer was ready).
	//nolint:unparam // test helper kept general; cpu is constant across current callers
	matchingPodBoundAt := func(name, cpu, nodeName string, boundAt time.Time) *corev1.Pod {
		pod := matchingPod(name, cpu, nodeName)
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(boundAt),
		}}
		return pod
	}

	condOf := func(c client.Client, name, condType string) *metav1.Condition {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		Expect(c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, cb)).To(Succeed())
		return apimeta.FindStatusCondition(cb.Status.Conditions, condType)
	}
	fulfilled := func(c client.Client, name string) *metav1.Condition {
		return condOf(c, name, autoscalingv1beta1.FulfilledCondition)
	}
	expired := func(c client.Client, name string) *metav1.Condition {
		return condOf(c, name, autoscalingv1beta1.ExpiredCondition)
	}
	// consumedReplicas reads the buffer's shrink high-water mark.
	consumedReplicas := func(c client.Client, name string) int32 {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		Expect(c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, cb)).To(Succeed())
		return lo.FromPtr(cb.Status.ConsumedReplicas)
	}

	const sel = "app=gang"

	It("latches Fulfilled=BufferFilled when bound matching capacity covers intended", func() {
		cb := ephemeralBufferReady("gang", 4, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		// intended = 4 * 1cpu = 4 cpu. Provide 4 bound matching pods (4 cpu).
		p, c, _ := newProv(cb, template1cpu("gang"),
			matchingPod("p1", "1", "node-1"),
			matchingPod("p2", "1", "node-1"),
			matchingPod("p3", "1", "node-2"),
			matchingPod("p4", "1", "node-2"),
		)
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())

		cond := fulfilled(c, "gang")
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(autoscalingv1beta1.FulfilledReasonBufferFilled))
	})

	It("does NOT latch while matching capacity is short of intended", func() {
		cb := ephemeralBufferReady("gang", 4, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		// Only 3 of 4 cpu bound.
		p, c, _ := newProv(cb, template1cpu("gang"),
			matchingPod("p1", "1", "node-1"),
			matchingPod("p2", "1", "node-1"),
			matchingPod("p3", "1", "node-2"),
		)
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(fulfilled(c, "gang")).To(BeNil())
	})

	It("does NOT count unbound matching pods toward fill", func() {
		cb := ephemeralBufferReady("gang", 2, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		// 2 cpu intended, but both pods are unbound (no nodeName).
		p, c, _ := newProv(cb, template1cpu("gang"),
			matchingPod("p1", "1", ""),
			matchingPod("p2", "1", ""),
		)
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(fulfilled(c, "gang")).To(BeNil())
	})

	It("latches from a single large pod covering many small chunks (capacity, not count)", func() {
		cb := ephemeralBufferReady("gang", 4, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		// intended = 4 cpu; one bound pod requesting 4 cpu should suffice.
		p, c, _ := newProv(cb, template1cpu("gang"), matchingPod("big", "4", "node-1"))
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())

		cond := fulfilled(c, "gang")
		Expect(cond).ToNot(BeNil())
		Expect(cond.Reason).To(Equal(autoscalingv1beta1.FulfilledReasonBufferFilled))
	})

	It("does not latch a non-one-shot (refillStrategy=recreate) buffer even when covered", func() {
		recreate := autoscalingv1beta1.RefillStrategyRecreate
		cb := ephemeralBufferReady("gang", 1, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		cb.Spec.RefillStrategy = &recreate
		p, c, _ := newProv(cb, template1cpu("gang"), matchingPod("p1", "1", "node-1"))
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(fulfilled(c, "gang")).To(BeNil())
	})

	It("does not latch an ephemeral buffer with no match selector (capacity path needs one)", func() {
		cb := ephemeralBufferReady("gang", 1, nil) // no selector annotation
		p, c, _ := newProv(cb, template1cpu("gang"), matchingPod("p1", "1", "node-1"))
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(fulfilled(c, "gang")).To(BeNil())
	})

	It("does NOT count pods bound before the buffer became ready (readiness boundary)", func() {
		cb := ephemeralBufferReady("gang", 2, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		// Both pods match and are bound, but they bound an hour BEFORE the buffer went ready — this
		// buffer did not provision for them, so they must not consume it.
		p, c, _ := newProv(cb, template1cpu("gang"),
			matchingPodBoundAt("old1", "1", "node-1", start.Add(-time.Hour)),
			matchingPodBoundAt("old2", "1", "node-1", start.Add(-time.Hour)),
		)
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(fulfilled(c, "gang")).To(BeNil())
		Expect(consumedReplicas(c, "gang")).To(Equal(int32(0)))
	})

	It("latches Expired=FillDeadlineExceeded (NOT Fulfilled) when the deadline elapses unfilled", func() {
		cb := ephemeralBufferReady("gang", 4, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		cb.Spec.FillDeadlineSeconds = lo.ToPtr(int32(30 * 60)) // 30m
		p, c, _ := newProv(cb, template1cpu("gang"))           // no matching pods at all
		// Before the deadline: not terminal.
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(fulfilled(c, "gang")).To(BeNil())
		Expect(expired(c, "gang")).To(BeNil())
		// Advance past the deadline.
		fc.SetTime(start.Add(31 * time.Minute))
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		// Deadline give-up is reported as Expired, NOT Fulfilled — consumers must distinguish
		// success from expiry (a Fulfilled=True here would misreport an unfilled buffer as filled).
		Expect(fulfilled(c, "gang")).To(BeNil())
		cond := expired(c, "gang")
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(autoscalingv1beta1.ExpiredReasonDeadlineExceeded))
	})

	It("is sticky: once Fulfilled it is not re-evaluated even if matching pods disappear", func() {
		cb := ephemeralBufferReady("gang", 1, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		p, c, _ := newProv(cb, template1cpu("gang"), matchingPod("p1", "1", "node-1"))
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(fulfilled(c, "gang")).ToNot(BeNil())

		// Delete the matching pod (gang finished) and reconcile again: stays Fulfilled.
		Expect(c.Delete(ctx, matchingPod("p1", "1", "node-1"))).To(Succeed())
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		cond := fulfilled(c, "gang")
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("ignores matching pods in a different namespace", func() {
		cb := ephemeralBufferReady("gang", 1, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		other := matchingPod("p1", "1", "node-1")
		other.Namespace = "other-ns"
		p, c, _ := newProv(cb, template1cpu("gang"), other)
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(fulfilled(c, "gang")).To(BeNil())
	})

	It("evicts the buffer's virtual pods from the cache immediately on latch (closes the re-fill race)", func() {
		cb := ephemeralBufferReady("gang", 2, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		p, _, cache := newProv(cb, template1cpu("gang"),
			matchingPod("p1", "1", "node-1"),
			matchingPod("p2", "1", "node-1"),
		)
		// Warm the cache so it holds this buffer's 2 virtual pods, as it would before the latch.
		Expect(cache.GetAll(ctx)).To(HaveLen(2))

		// Latch. updateEphemeralFulfillment must synchronously evict the entry so a subsequent
		// GetAll returns nothing — no stale virtual pods for an already-Fulfilled buffer.
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(cache.GetAll(ctx)).To(BeEmpty())
	})

	// Regression for the bug the KWOK live run surfaced: the buffer controller patches buffer
	// status concurrently, so the latch's own status Patch routinely hits a 409 Conflict. The
	// cache eviction (which is what actually halts provisioning) must NOT be gated on that write
	// succeeding — otherwise a conflict leaves stale virtual pods and triggers a spurious scale-up.
	It("still evicts virtual pods even when the status patch conflicts (KWOK-surfaced race)", func() {
		cb := ephemeralBufferReady("gang", 2, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		base := fakecr.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(cb, template1cpu("gang"),
				matchingPod("p1", "1", "node-1"),
				matchingPod("p2", "1", "node-1"),
			).
			WithStatusSubresource(&autoscalingv1beta1.CapacityBuffer{}).
			Build()
		// Wrap the client so EVERY status patch on the buffer returns Conflict, emulating the
		// buffer controller winning the optimistic-lock race on every attempt.
		conflicting := interceptor.NewClient(base, interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				return apierrors.NewConflict(
					schema.GroupResource{Group: "autoscaling.x-k8s.io", Resource: "capacitybuffers"},
					"gang", errors.New("the object has been modified"))
			},
		})
		cache := virtualpods.NewVirtualPodCache(conflicting)
		p := &Provisioner{kubeClient: conflicting, clock: fc, virtualPodCache: cache}

		Expect(cache.GetAll(ctx)).To(HaveLen(2))

		// The patch will conflict on every retry, so the call returns an error...
		err := p.updateEphemeralFulfillment(ctx)
		Expect(err).To(HaveOccurred())
		// ...but the cache MUST still be empty — provisioning is halted regardless of the write.
		Expect(cache.GetAll(ctx)).To(BeEmpty())
	})

	It("shrink-as-fill: partial fill advances consumedReplicas and trims virtual pods, without latching", func() {
		cb := ephemeralBufferReady("gang", 4, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		p, c, cache := newProv(cb, template1cpu("gang"),
			matchingPod("p1", "1", "node-1"),
			matchingPod("p2", "1", "node-1"),
		)
		Expect(cache.GetAll(ctx)).To(HaveLen(4)) // full size before any fill

		// 2 of 4 chunks consumed: not fulfilled, consumed=2.
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(fulfilled(c, "gang")).To(BeNil())
		Expect(consumedReplicas(c, "gang")).To(Equal(int32(2)))

		// Shrink: injection should now emit only the 2 unfilled chunks.
		trimmed := p.trimConsumedVirtualPods(ctx, cache.GetAll(ctx))
		Expect(trimmed).To(HaveLen(2))
	})

	It("shrink is monotonic: consumed does not decrease if matching pods later disappear", func() {
		cb := ephemeralBufferReady("gang", 4, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		p1 := matchingPod("p1", "1", "node-1")
		p2 := matchingPod("p2", "1", "node-1")
		p, c, _ := newProv(cb, template1cpu("gang"), p1, p2)

		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(consumedReplicas(c, "gang")).To(Equal(int32(2)))

		// Delete both matching pods, then reconcile: consumed must stay at 2 (not recreate).
		Expect(c.Delete(ctx, p1)).To(Succeed())
		Expect(c.Delete(ctx, p2)).To(Succeed())
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(consumedReplicas(c, "gang")).To(Equal(int32(2)))
		Expect(fulfilled(c, "gang")).To(BeNil()) // still not full (2 of 4), just doesn't shrink back
	})

	It("fully filling after a partial fill latches Fulfilled=BufferFilled", func() {
		cb := ephemeralBufferReady("gang", 2, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: sel})
		p, c, _ := newProv(cb, template1cpu("gang"), matchingPod("p1", "1", "node-1"))
		// 1 of 2: partial.
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		Expect(consumedReplicas(c, "gang")).To(Equal(int32(1)))
		Expect(fulfilled(c, "gang")).To(BeNil())
		// Add the 2nd matching pod, reconcile: now full -> Fulfilled.
		Expect(c.Create(ctx, matchingPod("p2", "1", "node-1"))).To(Succeed())
		Expect(p.updateEphemeralFulfillment(ctx)).To(Succeed())
		cond := fulfilled(c, "gang")
		Expect(cond).ToNot(BeNil())
		Expect(cond.Reason).To(Equal(autoscalingv1beta1.FulfilledReasonBufferFilled))
	})
})
