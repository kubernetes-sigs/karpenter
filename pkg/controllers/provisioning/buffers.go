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
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/apps"
)

// Called from GetPendingPods AFTER the Validate()/filter step so we skip PVC
// validation and don't pollute cluster state with synthetic decisions.
// listBuffersReadyForProvisioning returns all CapacityBuffers whose
// ReadyForProvisioning condition is True and whose desired replica count is > 0.
func (p *Provisioner) listBuffersReadyForProvisioning(ctx context.Context) ([]*autoscalingv1beta1.CapacityBuffer, error) {
	list := &autoscalingv1beta1.CapacityBufferList{}
	if err := p.kubeClient.List(ctx, list); err != nil {
		return nil, err
	}
	out := make([]*autoscalingv1beta1.CapacityBuffer, 0, len(list.Items))
	for i := range list.Items {
		cb := &list.Items[i]
		if !apimeta.IsStatusConditionTrue(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition) {
			continue
		}
		if cb.Status.Replicas == nil || *cb.Status.Replicas <= 0 {
			continue
		}
		if cb.Spec.PodTemplateRef == nil && cb.Spec.ScalableRef == nil {
			continue
		}
		out = append(out, cb)
	}
	return out, nil
}

// appendVirtualPods lists all CapacityBuffers that are ReadyForProvisioning,
// resolves their PodTemplate, and appends in-memory virtual pods to the
// pending-pod list. Virtual pods never round-trip through etcd; they exist
// only for the duration of one scheduling simulation.
//
// TODO: Consider having the buffer controller precompute virtual pods into an
// in-memory store (similar to pkg/controllers/state/Cluster) so this hot path
// becomes a cache read instead of List + Get per scheduling pass.
// Issue - https://github.com/kubernetes-sigs/karpenter/issues/3090
func (p *Provisioner) appendVirtualPods(ctx context.Context, pods []*corev1.Pod) []*corev1.Pod {
	buffers, err := p.listBuffersReadyForProvisioning(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "listing CapacityBuffers; proceeding without virtual pods")
		return pods
	}
	for _, cb := range buffers {
		spec, err := p.resolveVirtualPodSpec(ctx, cb)
		if err != nil {
			log.FromContext(ctx).WithValues("capacitybuffer", client.ObjectKeyFromObject(cb)).V(1).Info("skipping buffer", "reason", err.Error())
			continue
		}
		pods = append(pods, buildVirtualPods(cb, spec)...)
	}
	return pods
}

// resolveVirtualPodSpec fetches the pod spec for a buffer using the shared
// workload resolution utilities. Reads from spec (not status) to avoid stale
// references when users switch between podTemplateRef and scalableRef.
func (p *Provisioner) resolveVirtualPodSpec(ctx context.Context, cb *autoscalingv1beta1.CapacityBuffer) (corev1.PodSpec, error) {
	switch {
	case cb.Spec.PodTemplateRef != nil:
		result, err := apps.ResolvePodTemplateRef(ctx, p.kubeClient, cb.Spec.PodTemplateRef.Name, cb.Namespace)
		if err != nil {
			return corev1.PodSpec{}, err
		}
		return result.PodSpec, nil
	case cb.Spec.ScalableRef != nil:
		result, err := apps.ResolveScalableRef(ctx, p.kubeClient, cb.Spec.ScalableRef, cb.Namespace)
		if err != nil {
			return corev1.PodSpec{}, err
		}
		return result.PodSpec, nil
	default:
		return corev1.PodSpec{}, fmt.Errorf("buffer %q has neither podTemplateRef nor scalableRef in spec", cb.Name)
	}
}

// buildVirtualPods materializes N identical placeholder pods for a buffer using
// the given pod spec. Deterministic names and UIDs let downstream components
// associate results back to the owning buffer without additional bookkeeping.
func buildVirtualPods(cb *autoscalingv1beta1.CapacityBuffer, spec corev1.PodSpec) []*corev1.Pod {
	if cb.Status.Replicas == nil || *cb.Status.Replicas <= 0 {
		return nil
	}
	count := int(*cb.Status.Replicas)
	out := make([]*corev1.Pod, 0, count)
	// Strip anything that would make the scheduler call the API server.
	strippedSpec := sanitizeVirtualPodSpec(spec)
	strippedSpec.Priority = lo.ToPtr(autoscalingv1beta1.VirtualPodPriority)

	for i := 1; i <= count; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("capacity-buffer-%s-%d", cb.Name, i),
				Namespace: cb.Namespace,
				UID:       types.UID(fmt.Sprintf("%s-%d", cb.UID, i)),
				Annotations: map[string]string{
					autoscalingv1beta1.FakePodAnnotationKey: autoscalingv1beta1.FakePodAnnotationValue,
				},
				Labels: map[string]string{
					autoscalingv1beta1.BufferNameLabel:      cb.Name,
					autoscalingv1beta1.BufferNamespaceLabel: cb.Namespace,
				},
				CreationTimestamp: metav1.NewTime(time.Now()),
			},
			Spec: strippedSpec,
			Status: corev1.PodStatus{
				// Virtual pods must appear unschedulable so any code path that
				// re-checks IsProvisionable accepts them.
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodScheduled,
					Status: corev1.ConditionFalse,
					Reason: corev1.PodReasonUnschedulable,
				}},
			},
		}
		out = append(out, pod)
	}
	return out
}

// sanitizeVirtualPodSpec removes fields that would make a synthetic pod
// problematic for the scheduler. We can't resolve PVC topology without a
// real PVC, and we don't want to inherit a nodeName from the template.
func sanitizeVirtualPodSpec(spec corev1.PodSpec) corev1.PodSpec {
	spec = *spec.DeepCopy()
	spec.NodeName = ""
	// Drop PVC-backed and ephemeral volumes and their mounts. Ephemeral volumes
	// derive a PVC name from the pod name; for virtual pods that PVC will never
	// exist, causing topology resolution errors.
	keepVolumes := spec.Volumes[:0]
	droppedVolumeNames := map[string]struct{}{}
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil || v.Ephemeral != nil {
			droppedVolumeNames[v.Name] = struct{}{}
			continue
		}
		keepVolumes = append(keepVolumes, v)
	}
	spec.Volumes = keepVolumes
	if len(droppedVolumeNames) > 0 {
		for i := range spec.Containers {
			spec.Containers[i].VolumeMounts = lo.Filter(spec.Containers[i].VolumeMounts, func(m corev1.VolumeMount, _ int) bool {
				_, dropped := droppedVolumeNames[m.Name]
				return !dropped
			})
		}
		for i := range spec.InitContainers {
			spec.InitContainers[i].VolumeMounts = lo.Filter(spec.InitContainers[i].VolumeMounts, func(m corev1.VolumeMount, _ int) bool {
				_, dropped := droppedVolumeNames[m.Name]
				return !dropped
			})
		}
	}
	return spec
}

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
