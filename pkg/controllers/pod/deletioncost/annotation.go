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

package deletioncost

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The optimistic lock turns a concurrent writer into a 409, which Reconcile
// records as Skipped and retries next cycle.
func clearAnnotation(ctx context.Context, kubeClient client.Client, pod *corev1.Pod) error {
	// MergeFromWithOptions captures pod by pointer; mutate a copy so the diff is non-empty.
	updated := pod.DeepCopy()
	delete(updated.Annotations, corev1.PodDeletionCost)
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	return kubeClient.Patch(ctx, updated, patch)
}

// patchAnnotation is the symmetric set; see clearAnnotation for the lock.
func patchAnnotation(ctx context.Context, kubeClient client.Client, pod *corev1.Pod, value string) error {
	updated := pod.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[corev1.PodDeletionCost] = value
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	return kubeClient.Patch(ctx, updated, patch)
}
