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

package provisioning

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
)

// filterVirtualPodErrors returns a copy of the map with virtual buffer pods removed.
func filterVirtualPodErrors(m map[*corev1.Pod]error) map[*corev1.Pod]error {
	out := make(map[*corev1.Pod]error, len(m))
	for pod, err := range m {
		if !IsVirtualPod(pod) {
			out[pod] = err
		}
	}
	return out
}

// filterVirtualPodMapping returns a copy of the map with virtual buffer pods removed from each slice.
func filterVirtualPodMapping(m map[string][]*corev1.Pod) map[string][]*corev1.Pod {
	out := make(map[string][]*corev1.Pod, len(m))
	for key, pods := range m {
		var real []*corev1.Pod
		for _, pod := range pods {
			if !IsVirtualPod(pod) {
				real = append(real, pod)
			}
		}
		if len(real) > 0 {
			out[key] = real
		}
	}
	return out
}

// IsVirtualPod returns true if the pod is a CapacityBuffer virtual pod.
func IsVirtualPod(pod *corev1.Pod) bool {
	if pod == nil || pod.Annotations == nil {
		return false
	}
	return pod.Annotations[autoscalingv1beta1.FakePodAnnotationKey] == autoscalingv1beta1.FakePodAnnotationValue
}

// bufferKeyOf returns "namespace/name" for the buffer a virtual pod belongs to,
// or "" if it isn't a virtual pod.
func bufferKeyOf(pod *corev1.Pod) string {
	if !IsVirtualPod(pod) {
		return ""
	}
	ns := pod.Labels[autoscalingv1beta1.BufferNamespaceLabel]
	name := pod.Labels[autoscalingv1beta1.BufferNameLabel]
	if ns == "" || name == "" {
		return ""
	}
	return ns + "/" + name
}

// bufferProvisioningStatus summarizes, per buffer, which virtual pods scheduled
// to existing capacity vs. required new NodeClaims vs. failed outright.
type bufferProvisioningStatus struct {
	existing         int
	requiresNewClaim int
	failed           int
	desiredReplicas  int
}

// updateBufferProvisioningStatus patches the Provisioning condition on every
// CapacityBuffer, based on whether its virtual pods ended up on existing
// capacity (Provisioning=True) or required new NodeClaims (Provisioning=False).
func (p *Provisioner) updateBufferProvisioningStatus(ctx context.Context, results scheduler.Results) error {
	buffers, err := p.listAllBuffers(ctx)
	if err != nil {
		return err
	}
	if len(buffers) == 0 {
		return nil
	}
	byKey := map[string]*autoscalingv1beta1.CapacityBuffer{}
	for _, cb := range buffers {
		byKey[cb.Namespace+"/"+cb.Name] = cb
	}

	summary := classifyBufferPods(results, byKey)

	var errs []error
	for key, cb := range byKey {
		stored := cb.DeepCopy()
		newCondition := computeProvisioningCondition(cb, summary[key])
		if newCondition == nil {
			continue
		}
		apimeta.SetStatusCondition(&cb.Status.Conditions, *newCondition)
		if err := p.kubeClient.Status().Patch(ctx, cb, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			errs = append(errs, fmt.Errorf("patching buffer %q: %w", key, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("updating buffer statuses: %v", errs)
	}
	return nil
}

// listAllBuffers returns every CapacityBuffer; unlike listBuffersReadyForProvisioning
// we also want to observe buffers that are NotReady so we can set their
// Provisioning condition to False with the appropriate reason.
func (p *Provisioner) listAllBuffers(ctx context.Context) ([]*autoscalingv1beta1.CapacityBuffer, error) {
	list := &autoscalingv1beta1.CapacityBufferList{}
	if err := p.kubeClient.List(ctx, list); err != nil {
		return nil, err
	}
	out := make([]*autoscalingv1beta1.CapacityBuffer, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out, nil
}

// computeProvisioningCondition returns the Provisioning condition the
// provisioner should write for the given buffer, or nil if the controller
// hasn't yet marked it ReadyForProvisioning (in which case we leave the status
// alone rather than racing the controller).
func computeProvisioningCondition(cb *autoscalingv1beta1.CapacityBuffer, s *bufferProvisioningStatus) *metav1.Condition {
	now := metav1.Now()
	if !apimeta.IsStatusConditionTrue(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition) {
		return &metav1.Condition{
			Type:               autoscalingv1beta1.ProvisioningCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "NotReadyForProvisioning",
			Message:            "Buffer is not ReadyForProvisioning",
			ObservedGeneration: cb.Generation,
			LastTransitionTime: now,
		}
	}
	if cb.Status.Replicas == nil || *cb.Status.Replicas == 0 {
		return &metav1.Condition{
			Type:               autoscalingv1beta1.ProvisioningCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "BufferEmpty",
			Message:            "Buffer has zero desired replicas",
			ObservedGeneration: cb.Generation,
			LastTransitionTime: now,
		}
	}
	if s == nil {
		// Buffer ready but no virtual pods observed in results — nothing ran in
		// this scheduling cycle (e.g. results empty). Leave condition unchanged.
		return nil
	}
	if s.requiresNewClaim > 0 || s.failed > 0 {
		return &metav1.Condition{
			Type:               autoscalingv1beta1.ProvisioningCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "RequiresNewCapacity",
			Message:            fmt.Sprintf("%d/%d virtual pods required new capacity, %d failed", s.requiresNewClaim, s.desiredReplicas, s.failed),
			ObservedGeneration: cb.Generation,
			LastTransitionTime: now,
		}
	}
	if s.existing == s.desiredReplicas && s.desiredReplicas > 0 {
		return &metav1.Condition{
			Type:               autoscalingv1beta1.ProvisioningCondition,
			Status:             metav1.ConditionTrue,
			Reason:             "FitsExistingCapacity",
			Message:            fmt.Sprintf("All %d virtual pods fit on existing capacity", s.desiredReplicas),
			ObservedGeneration: cb.Generation,
			LastTransitionTime: now,
		}
	}
	return nil
}

// bufferPodCountsFromResults builds a providerID→count mapping of how many
// virtual buffer pods were placed on each existing node during this scheduling
// pass. Used to update cluster state (Cluster.bufferPodCounts) so the emptiness
// disruption path knows which nodes host buffer capacity.
//
// Only ExistingNodes are counted — pods on NewNodeClaims don't have a providerID
// yet, and those nodes are naturally protected from consolidation by the
// Consolidatable condition timer (which hasn't elapsed on a brand-new node).
//
// Consolidation does NOT consult this mapping. Instead, it naturally accounts
// for buffer pods because SimulateScheduling calls GetPendingPods (which injects
// virtual pods). The simulation must fit all pending pods (including virtual ones)
// onto the remaining/replacement nodes, so a replacement that's too small to
// host the buffer will be rejected.
func bufferPodCountsFromResults(results scheduler.Results) map[string]int {
	counts := map[string]int{}
	for _, existing := range results.ExistingNodes {
		for _, pod := range existing.Pods {
			if !IsVirtualPod(pod) {
				continue
			}
			counts[existing.ProviderID()]++
		}
	}
	return counts
}

// classifyBufferPods walks Schedule()'s Results and buckets virtual pods by
// owning buffer (keyed by "namespace/name"). Real (non-virtual) pods are ignored.
func classifyBufferPods(results scheduler.Results, buffers map[string]*autoscalingv1beta1.CapacityBuffer) map[string]*bufferProvisioningStatus {
	out := map[string]*bufferProvisioningStatus{}

	for _, existing := range results.ExistingNodes {
		countVirtualPods(existing.Pods, buffers, out, func(s *bufferProvisioningStatus) { s.existing++ })
	}
	for _, nc := range results.NewNodeClaims {
		countVirtualPods(nc.Pods, buffers, out, func(s *bufferProvisioningStatus) { s.requiresNewClaim++ })
	}
	for pod := range results.PodErrors {
		key := bufferKeyOf(pod)
		if key == "" {
			continue
		}
		ensureStatus(key, buffers, out).failed++
	}
	return out
}

func countVirtualPods(pods []*corev1.Pod, buffers map[string]*autoscalingv1beta1.CapacityBuffer, out map[string]*bufferProvisioningStatus, inc func(*bufferProvisioningStatus)) {
	for _, pod := range pods {
		key := bufferKeyOf(pod)
		if key == "" {
			continue
		}
		inc(ensureStatus(key, buffers, out))
	}
}

func ensureStatus(key string, buffers map[string]*autoscalingv1beta1.CapacityBuffer, out map[string]*bufferProvisioningStatus) *bufferProvisioningStatus {
	s, ok := out[key]
	if !ok {
		s = &bufferProvisioningStatus{}
		if cb, found := buffers[key]; found && cb.Status.Replicas != nil {
			s.desiredReplicas = int(*cb.Status.Replicas)
		}
		out[key] = s
	}
	return s
}
