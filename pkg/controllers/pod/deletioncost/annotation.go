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
	"errors"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

// UpdatePodDeletionCosts updates pod deletion cost annotations for all pods on
// the ranked nodes. When the feature gate is on the controller writes
// pod-deletion-cost directly: there is no overwrite-protection for
// customer-set values. Customers steering Karpenter consolidation are expected
// to use karpenter.sh/disruption-cost instead.
//
// Per-pod errors are aggregated via errors.Join so the caller sees the full
// set of failures from a single reconcile.
//
// NotFound and Conflict on the patch are both classified as Skipped: NotFound
// means the pod is already gone, Conflict means another writer raced us and
// we'll retry on the next reconcile. Neither is an operator-visible failure.
func UpdatePodDeletionCosts(ctx context.Context, kubeClient client.Client, nodeRanks []NodeRank) error {
	defer metrics.Measure(annotationDurationSeconds, noLabels)()

	var updatedCount, skippedCount, errorCount int
	var aggErr error

	for _, nodeRank := range nodeRanks {
		pods, err := nodeRank.Node.Pods(ctx, kubeClient)
		if err != nil {
			aggErr = errors.Join(aggErr, err)
			errorCount++
			continue
		}

		// Group D nodes (do-not-disrupt): clear any existing
		// pod-deletion-cost annotations the controller previously set,
		// instead of assigning a rank.
		if nodeRank.HasDoNotDisrupt {
			for _, pod := range pods {
				cleared, err := clearDeletionCost(ctx, kubeClient, pod)
				switch {
				case err != nil:
					aggErr = errors.Join(aggErr, err)
					errorCount++
				case cleared:
					updatedCount++
				default:
					skippedCount++
				}
			}
			continue
		}

		for _, pod := range pods {
			if !needsUpdate(pod, nodeRank.Rank) {
				skippedCount++
				continue
			}
			if err := patchAnnotation(ctx, kubeClient, pod, strconv.Itoa(nodeRank.Rank)); err != nil {
				if apierrors.IsNotFound(err) {
					log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("pod not found, skipping annotation update")
					skippedCount++
					continue
				}
				if apierrors.IsConflict(err) {
					log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("conflict updating pod annotation, will retry on next reconcile")
					skippedCount++
					continue
				}
				aggErr = errors.Join(aggErr, err)
				errorCount++
				continue
			}
			updatedCount++
		}
	}

	podsUpdatedTotal.Add(float64(updatedCount), map[string]string{resultLabel: "updated"})
	podsUpdatedTotal.Add(float64(skippedCount), map[string]string{resultLabel: "skipped_unchanged"})
	podsUpdatedTotal.Add(float64(errorCount), map[string]string{resultLabel: "error"})

	if updatedCount > 0 || errorCount > 0 {
		log.FromContext(ctx).WithValues(
			"updated", updatedCount,
			"skipped", skippedCount,
			"errors", errorCount,
		).V(1).Info("pod deletion cost annotation update completed")
	}
	return aggErr
}

// clearDeletionCost removes the pod-deletion-cost annotation from a pod when
// the node's group is do-not-disrupt. Returns (cleared, err) so callers can
// distinguish patches issued from no-ops. NotFound and Conflict are silently
// treated as no-ops (logged at V(1)) since the pod is gone or another writer
// raced us; we'll converge on the next reconcile.
func clearDeletionCost(ctx context.Context, kubeClient client.Client, pod *corev1.Pod) (bool, error) {
	if pod.Annotations == nil {
		return false, nil
	}
	if _, ok := pod.Annotations[corev1.PodDeletionCost]; !ok {
		return false, nil
	}
	updated := pod.DeepCopy()
	delete(updated.Annotations, corev1.PodDeletionCost)
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	if err := kubeClient.Patch(ctx, updated, patch); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("pod not found, skipping annotation clear")
			return false, nil
		}
		if apierrors.IsConflict(err) {
			log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("conflict clearing pod annotation, will retry on next reconcile")
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// needsUpdate reports whether the pod's pod-deletion-cost annotation already
// matches the desired rank. Avoids unnecessary API writes when the value is
// already correct.
func needsUpdate(pod *corev1.Pod, rank int) bool {
	if pod.Annotations == nil {
		return true
	}
	current, ok := pod.Annotations[corev1.PodDeletionCost]
	if !ok {
		return true
	}
	return current != strconv.Itoa(rank)
}

// patchAnnotation sets the pod-deletion-cost annotation on a pod via a merge
// patch with optimistic-lock so concurrent writers cannot silently overwrite
// each other. The caller's pod object is not mutated; we mutate a deep-copy.
func patchAnnotation(ctx context.Context, kubeClient client.Client, pod *corev1.Pod, value string) error {
	updated := pod.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[corev1.PodDeletionCost] = value
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	return kubeClient.Patch(ctx, updated, patch)
}
