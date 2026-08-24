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

// New file: pkg/controllers/provisioning/ephemeral.go (karpenter @ eb62f77).
// Implements the one-shot ("ephemeral", refillStrategy=none) CapacityBuffer behavior with
// monotonic shrink-as-fill: as a matching workload consumes the buffer's capacity, the buffer
// provisions only its unfilled remainder (Status.ConsumedReplicas is a monotonically-increasing
// high-water mark, so consumed capacity is never recreated). When fully consumed — or when the
// optional fillDeadline elapses — the buffer latches the terminal Fulfilled condition and stops
// producing virtual pods.

package provisioning

import (
	"context"
	"fmt"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/utils/apps"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// updateEphemeralFulfillment evaluates every ephemeral (refillStrategy=none) CapacityBuffer and,
// per cycle, advances its Status.ConsumedReplicas high-water mark toward the amount of matching,
// bound capacity observed (monotonic — never decreases, so consumed capacity is not recreated).
// When consumed reaches the desired replica count — or the fillDeadline elapses — the buffer is
// latched terminal (Fulfilled) and its virtual pods evicted.
//
// Called from Provisioner.Reconcile BEFORE Schedule(), under the FeatureGates.CapacityBuffer
// guard, so the same cycle's GetPendingPods sees the updated consumed count (via trimming) and a
// filled buffer contributes zero virtual pods.
func (p *Provisioner) updateEphemeralFulfillment(ctx context.Context) error {
	buffers, err := p.listAllBuffers(ctx)
	if err != nil {
		return err
	}

	// List all pods once. We query pods directly (not GetProvisionablePods) because consumption is
	// measured from BOUND pods (spec.nodeName != ""), which the provisionable filter excludes.
	podList := &corev1.PodList{}
	if err := p.kubeClient.List(ctx, podList); err != nil {
		return fmt.Errorf("listing pods for ephemeral buffer fulfillment, %w", err)
	}

	var errs []error
	for _, cb := range buffers {
		if !isEphemeral(cb) || apimeta.IsStatusConditionTrue(cb.Status.Conditions, autoscalingv1beta1.FulfilledCondition) {
			continue // not one-shot, or already terminal
		}
		consumed, reason, filled := p.evaluateFulfillment(ctx, cb, podList.Items)
		if consumed == lo.FromPtr(cb.Status.ConsumedReplicas) && !filled {
			continue // no shrink progress and not filling this cycle — nothing to persist
		}
		if err := p.persistFulfillment(ctx, cb, consumed, reason, filled); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("updating ephemeral buffer fulfillment: %v", errs)
	}
	return nil
}

// persistFulfillment advances the buffer's monotonic ConsumedReplicas high-water mark and, when
// filled, evicts its virtual pods and sets the terminal Fulfilled condition. The cache eviction
// happens FIRST and unconditionally — it is the load-bearing action that halts provisioning and
// must not be gated on the status write succeeding (the buffer controller patches status
// concurrently, so conflicts are routine). The status patch retries on conflict so it lands this
// cycle.
func (p *Provisioner) persistFulfillment(ctx context.Context, cb *autoscalingv1beta1.CapacityBuffer, consumed int32, reason string, filled bool) error {
	if filled {
		p.virtualPodCache.RemoveEntry(client.ObjectKeyFromObject(cb))
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &autoscalingv1beta1.CapacityBuffer{}
		if err := p.kubeClient.Get(ctx, client.ObjectKeyFromObject(cb), latest); err != nil {
			return err
		}
		if apimeta.IsStatusConditionTrue(latest.Status.Conditions, autoscalingv1beta1.FulfilledCondition) {
			return nil // already terminal
		}
		stored := latest.DeepCopy()
		latest.Status.ConsumedReplicas = lo.ToPtr(max(lo.FromPtr(latest.Status.ConsumedReplicas), consumed)) // monotonic
		if filled {
			latest.SetCondition(autoscalingv1beta1.FulfilledCondition, metav1.ConditionTrue, reason,
				fmt.Sprintf("Ephemeral buffer fulfilled (%s)", reason))
		}
		return p.kubeClient.Status().Patch(ctx, latest, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{}))
	}); err != nil {
		return fmt.Errorf("patching ephemeral buffer %q: %w", cb.Namespace+"/"+cb.Name, err)
	}
	if filled {
		log.FromContext(ctx).WithValues("capacitybuffer", cb.Namespace+"/"+cb.Name, "reason", reason).
			Info("ephemeral capacity buffer fulfilled; halting provisioning")
	}
	return nil
}

// evaluateFulfillment returns the buffer's new (monotonic) consumed-chunk count for this cycle,
// and whether it should latch terminal. It latches when consumed reaches the desired replica
// count, or when the optional fillDeadline has elapsed.
func (p *Provisioner) evaluateFulfillment(ctx context.Context, cb *autoscalingv1beta1.CapacityBuffer, pods []corev1.Pod) (consumed int32, reason string, filled bool) {
	prev := lo.FromPtr(cb.Status.ConsumedReplicas)
	// Only consider buffers the controller has marked ready with a positive replica count.
	if !apimeta.IsStatusConditionTrue(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition) {
		return prev, "", false
	}
	if cb.Status.Replicas == nil || *cb.Status.Replicas <= 0 {
		return prev, "", false
	}
	replicas := *cb.Status.Replicas
	consumed = prev

	// Consumption path (requires a match selector). Advance the high-water mark by the number of
	// whole chunks currently covered by matching bound capacity.
	if selector, ok, err := matchSelector(cb); err != nil {
		log.FromContext(ctx).WithValues("capacitybuffer", cb.Namespace+"/"+cb.Name).
			Error(err, "invalid buffer-match-selector annotation; skipping consumption tracking")
	} else if ok {
		perChunk, err := p.perChunkRequests(ctx, cb)
		if err != nil {
			log.FromContext(ctx).WithValues("capacitybuffer", cb.Namespace+"/"+cb.Name).
				Error(err, "resolving per-chunk requests")
		} else {
			bound := boundMatchingCapacity(cb.Namespace, selector, pods)
			consumed = max(consumed, consumedChunks(bound, perChunk, replicas)) // monotonic
		}
	}

	if consumed >= replicas {
		return replicas, autoscalingv1beta1.FulfilledReasonBufferFilled, true
	}
	// Deadline path (optional; independent of the selector).
	if deadline, ok := fillDeadline(cb); ok {
		if since, found := readyForProvisioningSince(cb); found && p.clock.Since(since) >= deadline {
			return consumed, autoscalingv1beta1.FulfilledReasonDeadlineExceeded, true
		}
	}
	return consumed, "", false
}

// trimConsumedVirtualPods implements shrink-as-fill: for each ephemeral buffer, it drops
// Status.ConsumedReplicas of that buffer's virtual pods, so a partially-filled one-shot buffer
// contributes only its unfilled remainder to the scheduling simulation. Because ConsumedReplicas
// is monotonic, the emitted count only ever decreases (consumed capacity is not recreated).
func (p *Provisioner) trimConsumedVirtualPods(ctx context.Context, vpods []*corev1.Pod) []*corev1.Pod {
	buffers, err := p.listAllBuffers(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "listing buffers to trim consumed virtual pods")
		return vpods
	}
	toDrop := map[string]int32{}
	for _, cb := range buffers {
		if isEphemeral(cb) {
			if c := lo.FromPtr(cb.Status.ConsumedReplicas); c > 0 {
				toDrop[cb.Namespace+"/"+cb.Name] = c
			}
		}
	}
	if len(toDrop) == 0 {
		return vpods
	}
	out := make([]*corev1.Pod, 0, len(vpods))
	for _, pod := range vpods {
		key := bufferKeyOf(pod)
		if n, ok := toDrop[key]; ok && n > 0 {
			toDrop[key] = n - 1
			continue // this chunk has been consumed — do not provision for it
		}
		out = append(out, pod)
	}
	return out
}

// perChunkRequests returns the resource requests of a single buffer chunk (one virtual pod),
// derived from the resolved pod template / scalable ref. The injected v1.ResourcePods count is
// dropped so consumption is measured purely by capacity.
func (p *Provisioner) perChunkRequests(ctx context.Context, cb *autoscalingv1beta1.CapacityBuffer) (corev1.ResourceList, error) {
	res, err := apps.ResolveCapacityBuffer(ctx, p.kubeClient, cb)
	if err != nil {
		return nil, err
	}
	req := resources.RequestsForPods(&corev1.Pod{Spec: res.PodTemplateSpec.Spec})
	delete(req, corev1.ResourcePods)
	return req, nil
}

// consumedChunks returns how many whole buffer chunks the bound capacity represents: the minimum,
// over each requested resource dimension, of floor(bound[r] / perChunk[r]), capped at replicas.
// Comparing per-dimension keeps a workload whose shape differs from the template from over- or
// under-counting on a single resource.
func consumedChunks(bound, perChunk corev1.ResourceList, replicas int32) int32 {
	minChunks := int64(-1)
	for name, per := range perChunk {
		if per.IsZero() {
			continue
		}
		have := bound[name] // zero-value Quantity if the dimension is absent
		c := have.MilliValue() / per.MilliValue()
		if minChunks < 0 || c < minChunks {
			minChunks = c
		}
	}
	if minChunks < 0 {
		return 0 // no positive per-chunk dimension to measure against
	}
	if minChunks > int64(replicas) {
		minChunks = int64(replicas)
	}
	return int32(minChunks)
}

// boundMatchingCapacity sums the resource requests of pods that (a) are in the buffer's
// namespace, (b) match the selector, (c) are bound to a node (spec.nodeName != ""), and (d) have
// no remaining scheduling gates. The gate check matters for Kueue, which holds pods behind
// kueue.x-k8s.io/admission and (with TAS) kueue.x-k8s.io/topology gates.
func boundMatchingCapacity(namespace string, selector labels.Selector, pods []corev1.Pod) corev1.ResourceList {
	matched := make([]*corev1.Pod, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		if pod.Namespace != namespace {
			continue
		}
		if pod.Spec.NodeName == "" {
			continue
		}
		if len(pod.Spec.SchedulingGates) > 0 {
			continue
		}
		if !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		matched = append(matched, pod)
	}
	bound := resources.RequestsForPods(matched...)
	delete(bound, corev1.ResourcePods)
	return bound
}

// --- small helpers -------------------------------------------------------------------------

// isEphemeral reports whether the buffer is one-shot, i.e. its refill strategy is "none".
// This is orthogonal to provisioningStrategy (which describes the kind of capacity).
func isEphemeral(cb *autoscalingv1beta1.CapacityBuffer) bool {
	return cb.Spec.RefillStrategy != nil &&
		*cb.Spec.RefillStrategy == autoscalingv1beta1.RefillStrategyNone
}

// matchSelector parses the buffer-match-selector annotation. Returns (selector, true, nil) when
// present and valid, (nil, false, nil) when absent, (nil, false, err) when present but invalid.
func matchSelector(cb *autoscalingv1beta1.CapacityBuffer) (labels.Selector, bool, error) {
	raw, ok := cb.Annotations[autoscalingv1beta1.BufferMatchSelectorAnnotation]
	if !ok || raw == "" {
		return nil, false, nil
	}
	// kubectl-style selector string, e.g. "app=trainer,role in (worker)".
	sel, err := labels.Parse(raw)
	if err != nil {
		return nil, false, err
	}
	return sel, true, nil
}

// fillDeadline returns the spec.fillDeadlineSeconds as a Duration, if set to a positive value.
func fillDeadline(cb *autoscalingv1beta1.CapacityBuffer) (time.Duration, bool) {
	if cb.Spec.FillDeadlineSeconds == nil || *cb.Spec.FillDeadlineSeconds <= 0 {
		return 0, false
	}
	return time.Duration(*cb.Spec.FillDeadlineSeconds) * time.Second, true
}

// readyForProvisioningSince returns the lastTransitionTime of the ReadyForProvisioning=True
// condition, used as the start of the fill-deadline clock.
func readyForProvisioningSince(cb *autoscalingv1beta1.CapacityBuffer) (time.Time, bool) {
	c := apimeta.FindStatusCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
	if c == nil || c.Status != metav1.ConditionTrue {
		return time.Time{}, false
	}
	return c.LastTransitionTime.Time, true
}
