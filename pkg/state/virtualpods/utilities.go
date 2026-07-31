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

package virtualpods

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

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/utils/apps"
)

// listBuffersReadyForProvisioning returns all CapacityBuffers whose
// ReadyForProvisioning condition is True and whose desired replica count is > 0.
func listBuffersReadyForProvisioning(ctx context.Context, kubeClient client.Client) ([]*autoscalingv1beta1.CapacityBuffer, error) {
	list := &autoscalingv1beta1.CapacityBufferList{}
	if err := kubeClient.List(ctx, list); err != nil {
		return nil, err
	}
	out := make([]*autoscalingv1beta1.CapacityBuffer, 0, len(list.Items))
	for i := range list.Items {
		cb := &list.Items[i]
		if isBufferReadyForProvisioning(cb) {
			out = append(out, cb)
		}
	}
	return out, nil
}

func isBufferReadyForProvisioning(cb *autoscalingv1beta1.CapacityBuffer) bool {
	if !apimeta.IsStatusConditionTrue(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition) {
		return false
	}
	if cb.Status.Replicas == nil || *cb.Status.Replicas <= 0 {
		return false
	}
	if cb.Spec.PodTemplateRef == nil && cb.Spec.ScalableRef == nil {
		return false
	}
	return true
}

// resolveVirtualPodSpec fetches the pod spec for a buffer using the shared
// workload resolution utilities. Reads from spec (not status) to avoid stale
// references when users switch between podTemplateRef and scalableRef.
func resolveVirtualPodSpec(ctx context.Context, kubeClient client.Client, cb *autoscalingv1beta1.CapacityBuffer) (corev1.PodSpec, error) {
	result, err := apps.ResolveCapacityBuffer(ctx, kubeClient, cb)
	if err != nil {
		return corev1.PodSpec{}, err
	}
	return result.PodSpec, nil
}

// BuildVirtualPods materializes N identical placeholder pods for a buffer using
// the given pod spec. Deterministic names and UIDs let downstream components
// associate results back to the owning buffer without additional bookkeeping.
func BuildVirtualPods(cb *autoscalingv1beta1.CapacityBuffer, spec corev1.PodSpec) []*corev1.Pod {
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
