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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	resourcehelper "k8s.io/component-helpers/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/state/prediction"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

var podCountOne = resource.MustParse("1")

// ownerResolution stores the result of a resolveTarget call for caching.
type ownerResolution struct {
	uid   types.UID
	found bool
}

//nolint:gocyclo
func resolveTarget(ctx context.Context, c client.Client, pod *corev1.Pod, cachedOwnerResolutions map[types.UID]ownerResolution) (types.UID, bool) {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if cachedOwnerResolutions != nil {
			if entry, ok := cachedOwnerResolutions[ref.UID]; ok {
				return entry.uid, entry.found
			}
		}
		var targetUID types.UID
		var found bool
		switch ref.Kind {
		case "ReplicaSet":
			rs := &metav1.PartialObjectMetadata{}
			rs.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"})
			if err := c.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ref.Name}, rs); err != nil {
				break
			}
			for _, rsRef := range rs.OwnerReferences {
				if rsRef.Controller != nil && *rsRef.Controller && rsRef.Kind == "Deployment" {
					targetUID = rsRef.UID
					found = true
					break
				}
			}
			if !found {
				targetUID = ref.UID
				found = true
			}
		case "StatefulSet":
			targetUID = ref.UID
			found = true
		case "DaemonSet":
			targetUID = ref.UID
			found = true
		case "Job":
			job := &metav1.PartialObjectMetadata{}
			job.SetGroupVersionKind(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"})
			if err := c.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ref.Name}, job); err != nil {
				break
			}
			for _, jobRef := range job.OwnerReferences {
				if jobRef.Controller != nil && *jobRef.Controller && jobRef.Kind == "CronJob" {
					targetUID = jobRef.UID
					found = true
					break
				}
			}
			if !found {
				targetUID = ref.UID
				found = true
			}
		case "ReplicationController":
			targetUID = ref.UID
			found = true
		}
		if cachedOwnerResolutions != nil {
			cachedOwnerResolutions[ref.UID] = ownerResolution{uid: targetUID, found: found}
		}
		return targetUID, found
	}
	return "", false
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
	targetUID, ok := resolveTarget(ctx, c, pod, cachedOwnerResolutions)
	if !ok {
		return resources.RequestsForPods(pod)
	}
	pred, ok := store.Get(targetUID)
	if !ok {
		return resources.RequestsForPods(pod)
	}
	result := computePredictedRequests(pod, pred)
	result[corev1.ResourcePods] = podCountOne
	return result
}

// computePredictedRequests computes effective pod resource requests with VPA predictions applied.
// It applies predictions to container specs and delegates to resourcehelper.PodRequests which
// implements the full Kubernetes scheduling semantics (init container sidecar handling,
// pod-level resources, overhead).
func computePredictedRequests(pod *corev1.Pod, pred *prediction.Prediction) corev1.ResourceList {
	modifiedPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers:     applyPredictions(pod.Spec.Containers, pred),
			InitContainers: applyPredictions(pod.Spec.InitContainers, pred),
			Overhead:       pod.Spec.Overhead,
			Resources:      pod.Spec.Resources,
		},
	}
	return resourcehelper.PodRequests(modifiedPod, resourcehelper.PodResourcesOptions{})
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
