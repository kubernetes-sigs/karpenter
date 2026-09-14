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

// Implements the one-shot ("ephemeral", refillStrategy=none) CapacityBuffer behavior with
// monotonic shrink-as-fill: as a matching workload consumes the buffer's capacity, the buffer
// provisions only its unfilled remainder (Status.ConsumedReplicas is a monotonically-increasing
// high-water mark, so consumed capacity is never recreated). When fully consumed the buffer
// latches the terminal Fulfilled condition; when the optional fillDeadline elapses unfilled it
// latches Expired. Either way it stops producing virtual pods.

package provisioning

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/utils/apps"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// fulfillmentUpdate is the outcome of evaluating one ephemeral buffer for one cycle.
type fulfillmentUpdate struct {
	// consumed is the new monotonic high-water mark of consumed chunks.
	consumed int32
	// fillStart is when the fill window opened; zero if not yet known.
	fillStart time.Time
	// condType/reason name the terminal condition to latch, or "" if not terminal this cycle.
	condType, reason string
}

func (u fulfillmentUpdate) terminal() bool { return u.condType != "" }

// needsPersist reports whether the update changes anything worth writing to the buffer's status.
func (u fulfillmentUpdate) needsPersist(cb *autoscalingv1beta1.CapacityBuffer) bool {
	return u.terminal() ||
		u.consumed > lo.FromPtr(cb.Status.ConsumedReplicas) ||
		(cb.Status.FillStartTime == nil && !u.fillStart.IsZero())
}

// updateEphemeralFulfillment evaluates every non-terminal ephemeral (refillStrategy=none)
// CapacityBuffer and, per cycle, advances its Status.ConsumedReplicas high-water mark toward the
// amount of matching, bound capacity observed (monotonic — never decreases, so consumed capacity
// is not recreated). When consumed reaches the desired replica count — or the fillDeadline
// elapses — the buffer is latched terminal and its virtual pods evicted.
//
// Called from Provisioner.Reconcile BEFORE Schedule(), under the FeatureGates.CapacityBuffer
// guard, so the same cycle's GetPendingPods sees the shrunken virtual pod set and a filled buffer
// contributes zero virtual pods. Pods are only listed when at least one candidate buffer exists,
// and then per buffer, scoped to the buffer's namespace and match selector.
func (p *Provisioner) updateEphemeralFulfillment(ctx context.Context) error {
	buffers, err := p.listAllBuffers(ctx)
	if err != nil {
		return err
	}
	candidates := lo.Filter(buffers, func(cb *autoscalingv1beta1.CapacityBuffer, _ int) bool {
		return cb.IsEphemeral() && !cb.IsTerminal()
	})
	if len(candidates) == 0 {
		return nil
	}
	var errs []error
	for _, cb := range candidates {
		upd := p.evaluateFulfillment(ctx, cb)
		if !upd.needsPersist(cb) {
			continue
		}
		if err := p.persistFulfillment(ctx, cb, upd); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// persistFulfillment applies the update to the virtual pod cache and then to the buffer's status.
// The cache update happens FIRST and unconditionally — it is the load-bearing action that halts
// or shrinks provisioning for this cycle and must not be gated on the status write succeeding.
//
// The status patch is a single optimistic-lock attempt. The buffer controller patches status
// concurrently, so conflicts are routine; retrying here would re-read the same informer cache and
// keep conflicting, so a conflict is simply left for the next cycle, which re-evaluates from a
// fresher cache and re-applies the (idempotent) cache update.
func (p *Provisioner) persistFulfillment(ctx context.Context, cb *autoscalingv1beta1.CapacityBuffer, upd fulfillmentUpdate) error {
	key := client.ObjectKeyFromObject(cb)
	if upd.terminal() {
		p.virtualPodCache.MarkTerminal(cb)
	} else if upd.consumed > lo.FromPtr(cb.Status.ConsumedReplicas) {
		p.virtualPodCache.Truncate(key, int(max(lo.FromPtr(cb.Status.Replicas)-upd.consumed, 0)))
	}

	stored := cb.DeepCopy()
	cb.Status.ConsumedReplicas = lo.ToPtr(max(lo.FromPtr(cb.Status.ConsumedReplicas), upd.consumed)) // monotonic
	if cb.Status.FillStartTime == nil && !upd.fillStart.IsZero() {
		cb.Status.FillStartTime = lo.ToPtr(metav1.NewTime(upd.fillStart))
	}
	if upd.terminal() {
		cb.SetCondition(upd.condType, metav1.ConditionTrue, upd.reason, fmt.Sprintf("Ephemeral buffer terminal: %s", upd.reason))
	}
	if err := p.kubeClient.Status().Patch(ctx, cb, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			log.FromContext(ctx).WithValues("capacitybuffer", key).V(1).Info("ephemeral buffer status conflict; retrying next cycle")
			return nil
		}
		return fmt.Errorf("patching ephemeral buffer %q: %w", key, err)
	}
	if upd.terminal() {
		log.FromContext(ctx).WithValues("capacitybuffer", key, "condition", upd.condType, "reason", upd.reason).
			Info("ephemeral capacity buffer terminal; halting provisioning")
	}
	return nil
}

// evaluateFulfillment computes the buffer's fulfillmentUpdate for this cycle: the new (monotonic)
// consumed-chunk count, the fill-window start, and, if the buffer should go terminal, the
// condition to latch: Fulfilled/BufferFilled when consumed reaches the desired replica count, or
// Expired/FillDeadlineExceeded when the optional fillDeadline elapses unfilled.
func (p *Provisioner) evaluateFulfillment(ctx context.Context, cb *autoscalingv1beta1.CapacityBuffer) fulfillmentUpdate {
	upd := fulfillmentUpdate{consumed: lo.FromPtr(cb.Status.ConsumedReplicas)}
	// Only consider buffers the controller has marked ready with a positive replica count.
	if !apimeta.IsStatusConditionTrue(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition) {
		return upd
	}
	if cb.Status.Replicas == nil || *cb.Status.Replicas <= 0 {
		return upd
	}
	replicas := *cb.Status.Replicas
	// Fill window: latched once in status; before that, the ReadyForProvisioning transition (which
	// is guaranteed present because the guard above passed). Pods bound before this instant were
	// not provisioned for by this buffer and must not consume it.
	upd.fillStart, _ = fillStartOf(cb)

	logger := log.FromContext(ctx).WithValues("capacitybuffer", client.ObjectKeyFromObject(cb))
	// Consumption path (requires a match selector). Advance the high-water mark by the number of
	// whole chunks currently covered by matching bound capacity.
	if selector, ok, err := matchSelector(cb); err != nil {
		logger.Error(err, "invalid buffer-match-selector annotation; skipping consumption tracking")
	} else if ok {
		if n, err := p.boundChunks(ctx, cb, selector, replicas, upd.fillStart); err != nil {
			logger.Error(err, "tracking ephemeral buffer consumption")
		} else {
			upd.consumed = max(upd.consumed, n) // monotonic
		}
	}

	if upd.consumed >= replicas {
		upd.consumed = replicas
		upd.condType, upd.reason = autoscalingv1beta1.FulfilledCondition, autoscalingv1beta1.FulfilledReasonBufferFilled
		return upd
	}
	// Deadline path (optional; independent of the selector).
	if deadline, ok := fillDeadline(cb); ok && !upd.fillStart.IsZero() && p.clock.Since(upd.fillStart) >= deadline {
		upd.condType, upd.reason = autoscalingv1beta1.ExpiredCondition, autoscalingv1beta1.ExpiredReasonDeadlineExceeded
	}
	return upd
}

// boundChunks returns how many whole chunks of the buffer are currently covered by matching bound
// capacity (capped at replicas). Pods are listed scoped to the buffer's namespace and selector so
// the informer's namespace index bounds the copy instead of materializing every pod in the
// cluster.
func (p *Provisioner) boundChunks(ctx context.Context, cb *autoscalingv1beta1.CapacityBuffer, selector labels.Selector, replicas int32, fillStart time.Time) (int32, error) {
	perChunk, err := p.perChunkRequests(ctx, cb)
	if err != nil {
		return 0, fmt.Errorf("resolving per-chunk requests, %w", err)
	}
	podList := &corev1.PodList{}
	if err := p.kubeClient.List(ctx, podList, client.InNamespace(cb.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return 0, fmt.Errorf("listing matching pods, %w", err)
	}
	bound := boundMatchingCapacity(cb.Namespace, selector, podList.Items, fillStart)
	return consumedChunks(bound, perChunk, replicas), nil
}

// perChunkRequests returns the resource requests of a single buffer chunk (one virtual pod). It
// reads the already-resolved virtual pods from the cache when present and falls back to resolving
// the pod template / scalable ref. The injected v1.ResourcePods count is dropped so consumption
// is measured purely by capacity.
func (p *Provisioner) perChunkRequests(ctx context.Context, cb *autoscalingv1beta1.CapacityBuffer) (corev1.ResourceList, error) {
	var req corev1.ResourceList
	if cached := p.virtualPodCache.Get(ctx, client.ObjectKeyFromObject(cb)); len(cached) > 0 {
		req = resources.RequestsForPods(cached[0])
	} else {
		res, err := apps.ResolveCapacityBuffer(ctx, p.kubeClient, cb)
		if err != nil {
			return nil, err
		}
		req = resources.RequestsForPods(&corev1.Pod{Spec: res.PodTemplateSpec.Spec})
	}
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
// namespace, (b) match the selector, (c) are bound to a node (spec.nodeName != ""), (d) have
// no remaining scheduling gates, and (e) became scheduled at/after fillStart (the buffer's
// fill-window boundary). The gate check matters for Kueue, which holds pods behind
// kueue.x-k8s.io/admission and (with TAS) kueue.x-k8s.io/topology gates. The namespace and
// selector checks are normally already satisfied by the scoped list and act as a safety net.
func boundMatchingCapacity(namespace string, selector labels.Selector, pods []corev1.Pod, fillStart time.Time) corev1.ResourceList {
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
		// Exclude pre-window bindings: a pod whose known bind time precedes the fill window did
		// not consume this buffer's provisioned capacity. When the bind time is unknown (zero) we
		// count the pod rather than under-report real capacity.
		if bt := podBoundSince(pod); !bt.IsZero() && bt.Before(fillStart) {
			continue
		}
		matched = append(matched, pod)
	}
	bound := resources.RequestsForPods(matched...)
	delete(bound, corev1.ResourcePods)
	return bound
}

// podBoundSince returns when the pod became scheduled: the lastTransitionTime of its PodScheduled
// condition, falling back to its creation time, or zero if neither is known.
func podBoundSince(pod *corev1.Pod) time.Time {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodScheduled {
			return pod.Status.Conditions[i].LastTransitionTime.Time
		}
	}
	return pod.CreationTimestamp.Time
}

// --- small helpers -------------------------------------------------------------------------

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

// fillStartOf returns when the buffer's fill window opened: status.fillStartTime once latched,
// otherwise the ReadyForProvisioning=True transition time.
func fillStartOf(cb *autoscalingv1beta1.CapacityBuffer) (time.Time, bool) {
	if cb.Status.FillStartTime != nil && !cb.Status.FillStartTime.IsZero() {
		return cb.Status.FillStartTime.Time, true
	}
	return readyForProvisioningSince(cb)
}

// readyForProvisioningSince returns the lastTransitionTime of the ReadyForProvisioning=True
// condition.
func readyForProvisioningSince(cb *autoscalingv1beta1.CapacityBuffer) (time.Time, bool) {
	c := apimeta.FindStatusCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
	if c == nil || c.Status != metav1.ConditionTrue {
		return time.Time{}, false
	}
	return c.LastTransitionTime.Time, true
}
