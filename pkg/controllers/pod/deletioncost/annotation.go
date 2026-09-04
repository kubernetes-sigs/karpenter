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

// clearAnnotation removes pod-deletion-cost via merge-patch with optimistic
// lock. MergeFromWithOptimisticLock is used so a concurrent writer of the
// same annotation (customer kubectl, third-party HPAs, admission webhooks)
// surfaces as a 409 Conflict — the queue's Reconcile then treats it as
// Skipped and lets the next cycle converge. See
// pkg/controllers/nodeclaim/lifecycle/controller.go:295-309 for the same
// annotation-race precedent.
func clearAnnotation(ctx context.Context, kubeClient client.Client, pod *corev1.Pod) error {
	updated := pod.DeepCopy()
	delete(updated.Annotations, corev1.PodDeletionCost)
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	return kubeClient.Patch(ctx, updated, patch)
}

// patchAnnotation sets pod-deletion-cost=value via merge-patch with
// optimistic lock. Symmetric with clearAnnotation; see that function's
// comment for the Conflict-detection rationale.
func patchAnnotation(ctx context.Context, kubeClient client.Client, pod *corev1.Pod, value string) error {
	updated := pod.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[corev1.PodDeletionCost] = value
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	return kubeClient.Patch(ctx, updated, patch)
}
