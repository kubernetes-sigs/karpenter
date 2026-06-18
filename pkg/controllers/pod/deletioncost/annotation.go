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
		var u, s, e int
		var perr error
		if nodeRank.HasDoNotDisrupt {
			u, s, e, perr = clearRanksFromPods(ctx, kubeClient, pods)
		} else {
			u, s, e, perr = applyRankToPods(ctx, kubeClient, pods, nodeRank.Rank)
		}
		updatedCount += u
		skippedCount += s
		errorCount += e
		if perr != nil {
			aggErr = errors.Join(aggErr, perr)
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

// applyRankToPods writes pod-deletion-cost=rank to each pod via patchAnnotation,
// classifying NotFound and Conflict as skipped (logged at V(1)) and aggregating
// other errors via errors.Join.
func applyRankToPods(ctx context.Context, kubeClient client.Client, pods []*corev1.Pod, rank int) (updated, skipped, errCount int, err error) {
	value := strconv.Itoa(rank)
	for _, pod := range pods {
		if !needsUpdate(pod, rank) {
			skipped++
			continue
		}
		if perr := patchAnnotation(ctx, kubeClient, pod, value); perr != nil {
			if apierrors.IsNotFound(perr) {
				log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("pod not found, skipping annotation update")
				skipped++
				continue
			}
			if apierrors.IsConflict(perr) {
				log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("conflict updating pod annotation, will retry on next reconcile")
				skipped++
				continue
			}
			err = errors.Join(err, perr)
			errCount++
			continue
		}
		updated++
	}
	return updated, skipped, errCount, err
}

// clearRanksFromPods removes pod-deletion-cost from each pod via clearDeletionCost,
// counting cleared patches as updated and aggregating non-NotFound/non-Conflict
// errors via errors.Join.
func clearRanksFromPods(ctx context.Context, kubeClient client.Client, pods []*corev1.Pod) (updated, skipped, errCount int, err error) {
	for _, pod := range pods {
		cleared, perr := clearDeletionCost(ctx, kubeClient, pod)
		switch {
		case perr != nil:
			err = errors.Join(err, perr)
			errCount++
		case cleared:
			updated++
		default:
			skipped++
		}
	}
	return updated, skipped, errCount, err
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
