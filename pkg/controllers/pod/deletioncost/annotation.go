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
	"strconv"

	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

// podPatchStats aggregates per-pod patch outcomes across a single reconcile.
// Carried by value through helpers so the outer reconcile can fold per-iteration
// stats with += and emit metrics once at the end.
type podPatchStats struct {
	updated int
	skipped int
	errors  int
}

func (s *podPatchStats) add(other podPatchStats) {
	s.updated += other.updated
	s.skipped += other.skipped
	s.errors += other.errors
}

// UpdatePodDeletionCosts updates pod deletion cost annotations for all pods on
// the ranked nodes. When the feature gate is on the controller writes
// pod-deletion-cost directly: there is no overwrite-protection for
// customer-set values. Customers steering Karpenter consolidation are expected
// to use karpenter.sh/disruption-cost instead.
//
// Per-pod errors are aggregated via multierr.Append so the caller sees the full
// set of failures from a single reconcile.
//
// NotFound and Conflict on the patch are both classified as Skipped: NotFound
// means the pod is already gone, Conflict means another writer raced us and
// we'll retry on the next reconcile. Neither is an operator-visible failure.
func UpdatePodDeletionCosts(ctx context.Context, kubeClient client.Client, nodeRanks []NodeRank) error {
	defer metrics.Measure(annotationDurationSeconds, noLabels)()

	var totals podPatchStats
	var aggErr error

	for _, nodeRank := range nodeRanks {
		pods, err := nodeRank.Node.Pods(ctx, kubeClient)
		if err != nil {
			aggErr = multierr.Append(aggErr, err)
			totals.errors++
			continue
		}
		var stats podPatchStats
		var perr error
		if nodeRank.HasDoNotDisrupt {
			stats, perr = clearRanksFromPods(ctx, kubeClient, pods)
		} else {
			stats, perr = applyRankToPods(ctx, kubeClient, pods, nodeRank.Rank)
		}
		totals.add(stats)
		if perr != nil {
			aggErr = multierr.Append(aggErr, perr)
		}
	}

	podsUpdatedTotal.Add(float64(totals.updated), map[string]string{resultLabel: "updated"})
	podsUpdatedTotal.Add(float64(totals.skipped), map[string]string{resultLabel: "skipped_unchanged"})
	podsUpdatedTotal.Add(float64(totals.errors), map[string]string{resultLabel: "error"})

	if totals.updated > 0 || totals.errors > 0 {
		log.FromContext(ctx).WithValues(
			"updated", totals.updated,
			"skipped", totals.skipped,
			"errors", totals.errors,
		).V(1).Info("pod deletion cost annotation update completed")
	}
	return aggErr
}

// applyRankToPods writes pod-deletion-cost=rank to each pod via patchAnnotation,
// classifying NotFound and Conflict as skipped (logged at V(1)) and aggregating
// other errors via multierr.Append.
func applyRankToPods(ctx context.Context, kubeClient client.Client, pods []*corev1.Pod, rank int) (podPatchStats, error) {
	value := strconv.Itoa(rank)
	var stats podPatchStats
	var err error
	for _, pod := range pods {
		if !needsUpdate(pod, value) {
			stats.skipped++
			continue
		}
		if perr := patchAnnotation(ctx, kubeClient, pod, value); perr != nil {
			if apierrors.IsNotFound(perr) {
				log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("pod not found, skipping annotation update")
				stats.skipped++
				continue
			}
			if apierrors.IsConflict(perr) {
				log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("conflict updating pod annotation, will retry on next reconcile")
				stats.skipped++
				continue
			}
			err = multierr.Append(err, perr)
			stats.errors++
			continue
		}
		stats.updated++
	}
	return stats, err
}

// clearRanksFromPods removes pod-deletion-cost from each pod via clearDeletionCost,
// counting cleared patches as updated and aggregating non-NotFound/non-Conflict
// errors via multierr.Append.
func clearRanksFromPods(ctx context.Context, kubeClient client.Client, pods []*corev1.Pod) (podPatchStats, error) {
	var stats podPatchStats
	var err error
	for _, pod := range pods {
		cleared, perr := clearDeletionCost(ctx, kubeClient, pod)
		switch {
		case perr != nil:
			err = multierr.Append(err, perr)
			stats.errors++
		case cleared:
			stats.updated++
		default:
			stats.skipped++
		}
	}
	return stats, err
}

// clearDeletionCost removes the pod-deletion-cost annotation from a pod when
// the node's group is do-not-disrupt. Returns (cleared, err) so callers can
// distinguish patches issued from no-ops. NotFound and Conflict are silently
// treated as no-ops (logged at V(1)) since the pod is gone or another writer
// raced us; we'll converge on the next reconcile.
func clearDeletionCost(ctx context.Context, kubeClient client.Client, pod *corev1.Pod) (bool, error) {
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
// matches the desired value. The caller passes the pre-stringified rank so the
// strconv.Itoa cost is paid once per node instead of once per pod. Relies on
// Go's nil-safe map read: indexing a nil map returns the zero value plus
// ok=false, so no separate nil-check is needed.
func needsUpdate(pod *corev1.Pod, value string) bool {
	current, ok := pod.Annotations[corev1.PodDeletionCost]
	if !ok {
		return true
	}
	return current != value
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
