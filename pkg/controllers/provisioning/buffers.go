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

	autoscalingv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1alpha1"
		scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
)

// appendVirtualPods lists all CapacityBuffers that are ReadyForProvisioning,
// resolves their PodTemplate, and appends in-memory virtual pods to the
// pending-pod list. Virtual pods never round-trip through etcd; they exist
// only for the duration of one scheduling simulation.
//
// Called from GetPendingPods AFTER the Validate()/filter step so we skip PVC
// validation and don't pollute cluster state with synthetic decisions.
// listBuffersReadyForProvisioning returns all CapacityBuffers whose
// ReadyForProvisioning condition is True and whose desired replica count is > 0.
func (p *Provisioner) listBuffersReadyForProvisioning(ctx context.Context) ([]*autoscalingv1alpha1.CapacityBuffer, error) {
	list := &autoscalingv1alpha1.CapacityBufferList{}
	if err := p.kubeClient.List(ctx, list); err != nil {
		return nil, err
	}
	out := make([]*autoscalingv1alpha1.CapacityBuffer, 0, len(list.Items))
	for i := range list.Items {
		cb := &list.Items[i]
		if !apimeta.IsStatusConditionTrue(cb.Status.Conditions, autoscalingv1alpha1.ReadyForProvisioningCondition) {
			continue
		}
		if cb.Status.Replicas == nil || *cb.Status.Replicas <= 0 {
			continue
		}
		if cb.Status.PodTemplateRef == nil {
			continue
		}
		out = append(out, cb)
	}
	return out, nil
}

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

// resolveVirtualPodSpec fetches the PodTemplate referenced by the buffer's
// status and returns its pod spec.
func (p *Provisioner) resolveVirtualPodSpec(ctx context.Context, cb *autoscalingv1alpha1.CapacityBuffer) (corev1.PodSpec, error) {
	pt := &corev1.PodTemplate{}
	if err := p.kubeClient.Get(ctx, types.NamespacedName{
		Name:      cb.Status.PodTemplateRef.Name,
		Namespace: cb.Namespace,
	}, pt); err != nil {
		return corev1.PodSpec{}, fmt.Errorf("getting PodTemplate %q: %w", cb.Status.PodTemplateRef.Name, err)
	}
	return pt.Template.Spec, nil
}

// buildVirtualPods materializes N identical placeholder pods for a buffer using
// the given pod spec. Deterministic names and UIDs let downstream components
// associate results back to the owning buffer without additional bookkeeping.
func buildVirtualPods(cb *autoscalingv1alpha1.CapacityBuffer, spec corev1.PodSpec) []*corev1.Pod {
	if cb.Status.Replicas == nil || *cb.Status.Replicas <= 0 {
		return nil
	}
	count := int(*cb.Status.Replicas)
	out := make([]*corev1.Pod, 0, count)
	// Strip anything that would make the scheduler call the API server.
	strippedSpec := sanitizeVirtualPodSpec(spec)
	strippedSpec.Priority = lo.ToPtr(autoscalingv1alpha1.VirtualPodPriority)

	for i := 1; i <= count; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("capacity-buffer-%s-%d", cb.Name, i),
				Namespace: cb.Namespace,
				UID:       types.UID(fmt.Sprintf("%s-%d", cb.UID, i)),
				Annotations: map[string]string{
					autoscalingv1alpha1.FakePodAnnotationKey: autoscalingv1alpha1.FakePodAnnotationValue,
				},
				Labels: map[string]string{
					autoscalingv1alpha1.BufferNameLabel: cb.Name,
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
	// Drop any PVC-backed volumes and their mounts to avoid PVC topology checks.
	keepVolumes := spec.Volumes[:0]
	droppedVolumeNames := map[string]struct{}{}
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			droppedVolumeNames[v.Name] = struct{}{}
			continue
		}
		keepVolumes = append(keepVolumes, v)
	}
	spec.Volumes = keepVolumes
	if len(droppedVolumeNames) > 0 {
		for i := range spec.Containers {
			spec.Containers[i].VolumeMounts = filterMounts(spec.Containers[i].VolumeMounts, droppedVolumeNames)
		}
		for i := range spec.InitContainers {
			spec.InitContainers[i].VolumeMounts = filterMounts(spec.InitContainers[i].VolumeMounts, droppedVolumeNames)
		}
	}
	return spec
}

func filterMounts(mounts []corev1.VolumeMount, drop map[string]struct{}) []corev1.VolumeMount {
	out := mounts[:0]
	for _, m := range mounts {
		if _, dropped := drop[m.Name]; dropped {
			continue
		}
		out = append(out, m)
	}
	return out
}

// IsVirtualPod returns true if the pod is a CapacityBuffer virtual pod.
func IsVirtualPod(pod *corev1.Pod) bool {
	if pod == nil || pod.Annotations == nil {
		return false
	}
	return pod.Annotations[autoscalingv1alpha1.FakePodAnnotationKey] == autoscalingv1alpha1.FakePodAnnotationValue
}

// bufferNameOf returns the buffer name a virtual pod belongs to, or "" if it
// isn't a virtual pod.
func bufferNameOf(pod *corev1.Pod) string {
	if !IsVirtualPod(pod) {
		return ""
	}
	return pod.Labels[autoscalingv1alpha1.BufferNameLabel]
}

// bufferProvisioningStatus summarises, per buffer, which virtual pods scheduled
// to existing capacity vs. required new NodeClaims vs. failed outright.
type bufferProvisioningStatus struct {
	namespace          string
	existing           int
	requiresNewClaim   int
	failed             int
	desiredReplicas    int
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
	byName := map[string]*autoscalingv1alpha1.CapacityBuffer{}
	for _, cb := range buffers {
		byName[cb.Name] = cb
	}

	summary := classifyBufferPods(results, byName)

	var errs []error
	for name, cb := range byName {
		stored := cb.DeepCopy()
		newCondition := computeProvisioningCondition(cb, summary[name])
		if newCondition == nil {
			continue
		}
		apimeta.SetStatusCondition(&cb.Status.Conditions, *newCondition)
		if err := p.kubeClient.Status().Patch(ctx, cb, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			errs = append(errs, fmt.Errorf("patching buffer %q: %w", name, err))
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
func (p *Provisioner) listAllBuffers(ctx context.Context) ([]*autoscalingv1alpha1.CapacityBuffer, error) {
	list := &autoscalingv1alpha1.CapacityBufferList{}
	if err := p.kubeClient.List(ctx, list); err != nil {
		return nil, err
	}
	out := make([]*autoscalingv1alpha1.CapacityBuffer, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out, nil
}

// computeProvisioningCondition returns the Provisioning condition the
// provisioner should write for the given buffer, or nil if the controller
// hasn't yet marked it ReadyForProvisioning (in which case we leave the status
// alone rather than racing the controller).
func computeProvisioningCondition(cb *autoscalingv1alpha1.CapacityBuffer, s *bufferProvisioningStatus) *metav1.Condition {
	now := metav1.Now()
	if !apimeta.IsStatusConditionTrue(cb.Status.Conditions, autoscalingv1alpha1.ReadyForProvisioningCondition) {
		return &metav1.Condition{
			Type:               autoscalingv1alpha1.ProvisioningCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "NotReadyForProvisioning",
			Message:            "Buffer is not ReadyForProvisioning",
			ObservedGeneration: cb.Generation,
			LastTransitionTime: now,
		}
	}
	if cb.Status.Replicas == nil || *cb.Status.Replicas == 0 {
		return &metav1.Condition{
			Type:               autoscalingv1alpha1.ProvisioningCondition,
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
			Type:               autoscalingv1alpha1.ProvisioningCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "RequiresNewCapacity",
			Message:            fmt.Sprintf("%d/%d virtual pods required new capacity, %d failed", s.requiresNewClaim, s.desiredReplicas, s.failed),
			ObservedGeneration: cb.Generation,
			LastTransitionTime: now,
		}
	}
	if s.existing == s.desiredReplicas && s.desiredReplicas > 0 {
		return &metav1.Condition{
			Type:               autoscalingv1alpha1.ProvisioningCondition,
			Status:             metav1.ConditionTrue,
			Reason:             "FitsExistingCapacity",
			Message:            fmt.Sprintf("All %d virtual pods fit on existing capacity", s.desiredReplicas),
			ObservedGeneration: cb.Generation,
			LastTransitionTime: now,
		}
	}
	return nil
}

// classifyBufferPods walks Schedule()'s Results and buckets virtual pods by
// owning buffer. Real (non-virtual) pods are ignored.
func classifyBufferPods(results scheduler.Results, buffers map[string]*autoscalingv1alpha1.CapacityBuffer) map[string]*bufferProvisioningStatus {
	out := map[string]*bufferProvisioningStatus{}
	ensure := func(name, namespace string) *bufferProvisioningStatus {
		s, ok := out[name]
		if !ok {
			s = &bufferProvisioningStatus{namespace: namespace}
			if cb, found := buffers[name]; found && cb.Status.Replicas != nil {
				s.desiredReplicas = int(*cb.Status.Replicas)
			}
			out[name] = s
		}
		return s
	}

	for _, existing := range results.ExistingNodes {
		for _, pod := range existing.Pods {
			name := bufferNameOf(pod)
			if name == "" {
				continue
			}
			ensure(name, pod.Namespace).existing++
		}
	}
	for _, nc := range results.NewNodeClaims {
		for _, pod := range nc.Pods {
			name := bufferNameOf(pod)
			if name == "" {
				continue
			}
			ensure(name, pod.Namespace).requiresNewClaim++
		}
	}
	for pod := range results.PodErrors {
		name := bufferNameOf(pod)
		if name == "" {
			continue
		}
		ensure(name, pod.Namespace).failed++
	}
	return out
}
