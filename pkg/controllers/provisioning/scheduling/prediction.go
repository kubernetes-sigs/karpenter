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

package scheduling

import (
	"context"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/state/prediction"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

var podCountOne = resource.MustParse("1")

// ownerResolution stores the result of a resolveTarget call for caching.
type ownerResolution struct {
	target prediction.TargetKey
	found  bool
}

// resolveTarget resolves a pod to the workload its prediction is stored under, which is always one of
// prediction.SupportedTargets. Keep the two in sync.
//
//nolint:gocyclo
func resolveTarget(ctx context.Context, c client.Client, pod *corev1.Pod, cachedOwnerResolutions map[types.UID]ownerResolution) (prediction.TargetKey, bool) {
	ref := metav1.GetControllerOfNoCopy(pod)
	if ref == nil {
		return prediction.TargetKey{}, false
	}
	if cachedOwnerResolutions != nil {
		if entry, ok := cachedOwnerResolutions[ref.UID]; ok {
			return entry.target, entry.found
		}
	}
	var target *metav1.OwnerReference
	switch ref.Kind {
	case "ReplicaSet":
		rs := &metav1.PartialObjectMetadata{}
		rs.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"})
		if err := c.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ref.Name}, rs); err != nil {
			if !apierrors.IsNotFound(err) {
				log.FromContext(ctx).WithValues("Pod", klog.KObj(pod), "ReplicaSet", klog.KRef(pod.Namespace, ref.Name)).V(1).Info("failed resolving pod owner, skipping prediction", "error", err)
			}
			break
		}
		target = ref
		if owner := metav1.GetControllerOfNoCopy(rs); owner != nil && owner.Kind == "Deployment" {
			target = owner
		}
	case "StatefulSet", "DaemonSet", "ReplicationController":
		target = ref
	case "Job":
		job := &metav1.PartialObjectMetadata{}
		job.SetGroupVersionKind(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"})
		if err := c.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ref.Name}, job); err != nil {
			if !apierrors.IsNotFound(err) {
				log.FromContext(ctx).WithValues("Pod", klog.KObj(pod), "Job", klog.KRef(pod.Namespace, ref.Name)).V(1).Info("failed resolving pod owner, skipping prediction", "error", err)
			}
			break
		}
		target = ref
		if owner := metav1.GetControllerOfNoCopy(job); owner != nil && owner.Kind == "CronJob" {
			target = owner
		}
	}
	resolution := ownerResolution{found: target != nil}
	if target != nil {
		resolution.target = prediction.TargetKey{
			GroupKind:      schema.FromAPIVersionAndKind(target.APIVersion, target.Kind).GroupKind(),
			NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: target.Name},
		}
	}
	if cachedOwnerResolutions != nil {
		cachedOwnerResolutions[ref.UID] = resolution
	}
	return resolution.target, resolution.found
}

// PredictedRequests returns the pod's resource requests with VPA predictions applied.
// For each container with a prediction, it replaces current requests with the predicted value.
// Containers without a prediction keep their current requests.
// If the store is nil or no prediction exists for the pod's owner, returns resources.RequestsForPods.
// The cachedOwnerResolutions, if non-nil, avoids redundant owner resolution for pods sharing the same controller.
func PredictedRequests(ctx context.Context, c client.Client, store *prediction.Store, pod *corev1.Pod, cachedOwnerResolutions map[types.UID]ownerResolution) corev1.ResourceList {
	if store == nil || store.Len() == 0 {
		return resources.RequestsForPods(pod)
	}
	target, ok := resolveTarget(ctx, c, pod, cachedOwnerResolutions)
	if !ok {
		return resources.RequestsForPods(pod)
	}
	pred, ok := store.Get(target)
	if !ok {
		return resources.RequestsForPods(pod)
	}
	result := computePredictedRequests(pod, pred)
	result[corev1.ResourcePods] = podCountOne
	return result
}

func computePredictedRequests(pod *corev1.Pod, pred *prediction.Prediction) corev1.ResourceList {
	return resources.RequestsForSpec(&corev1.PodSpec{
		Containers:     applyPredictions(pod.Spec.Containers, pred),
		InitContainers: applyPredictions(pod.Spec.InitContainers, pred),
		Overhead:       pod.Spec.Overhead,
		Resources:      pod.Spec.Resources,
	})
}

// applyPredictions returns a copy of the containers slice with predicted resource requests
// substituted where available. Containers without predictions are returned unchanged.
func applyPredictions(containers []corev1.Container, pred *prediction.Prediction) []corev1.Container {
	result := make([]corev1.Container, len(containers))
	for i, c := range containers {
		result[i] = c
		predicted, ok := pred.Containers[c.Name]
		if !ok {
			continue
		}
		// Start with current requests, override/add predicted values
		merged := make(corev1.ResourceList, len(c.Resources.Requests)+len(predicted))
		maps.Copy(merged, c.Resources.Requests)
		maps.Copy(merged, predicted)
		result[i].Resources = corev1.ResourceRequirements{Requests: merged, Limits: c.Resources.Limits}
	}
	return result
}
