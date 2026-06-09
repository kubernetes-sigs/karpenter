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

	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// AnnotationManager handles pod deletion cost annotation updates.
// Exported so tests can drive it directly.
type AnnotationManager struct {
	kubeClient client.Client
	recorder   events.Recorder
}

func NewAnnotationManager(kubeClient client.Client, recorder events.Recorder) *AnnotationManager {
	return &AnnotationManager{kubeClient: kubeClient, recorder: recorder}
}

// podResult tracks the outcome of processing a single pod.
type podResult int

const (
	podResultSuccess podResult = iota
	// podResultSkipped covers no-op writes (the desired value already matches),
	// missing pods (NotFound), and conflict retries (which are benign and will
	// retry on the next reconcile).
	podResultSkipped
	podResultError
)

// UpdatePodDeletionCosts updates pod deletion cost annotations for all pods on
// the ranked nodes. When the feature gate is on the controller writes
// pod-deletion-cost directly: there is no overwrite-protection for
// customer-set values. Customers steering Karpenter consolidation are expected
// to use karpenter.sh/disruption-cost instead.
//
// Per-pod errors are aggregated via errors.Join so the caller sees the full
// set of failures from a single reconcile.
func (a *AnnotationManager) UpdatePodDeletionCosts(ctx context.Context, nodeRanks []NodeRank) error {
	defer metrics.Measure(annotationDurationSeconds, noLabels)()

	var successCount, skippedCount, errorCount int
	var aggErr error

	for _, nodeRank := range nodeRanks {
		pods, err := nodeRank.Node.Pods(ctx, a.kubeClient)
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
				if err := a.clearDeletionCost(ctx, pod); err != nil {
					aggErr = errors.Join(aggErr, err)
					errorCount++
				}
			}
			continue
		}

		for _, pod := range pods {
			res, err := a.processSinglePod(ctx, pod, nodeRank.Rank)
			switch res {
			case podResultSuccess:
				successCount++
			case podResultSkipped:
				skippedCount++
			case podResultError:
				errorCount++
				aggErr = errors.Join(aggErr, err)
			}
		}
	}

	a.recordMetrics(ctx, successCount, skippedCount, errorCount)
	return aggErr
}

// recordMetrics emits pod update metrics and logs the summary.
func (a *AnnotationManager) recordMetrics(ctx context.Context, successCount, skippedCount, errorCount int) {
	podsUpdatedTotal.Add(float64(successCount), map[string]string{resultLabel: "success"})
	podsUpdatedTotal.Add(float64(skippedCount), map[string]string{resultLabel: "skipped_unchanged"})
	podsUpdatedTotal.Add(float64(errorCount), map[string]string{resultLabel: "error"})

	if successCount > 0 || errorCount > 0 {
		log.FromContext(ctx).WithValues(
			"success", successCount,
			"skipped", skippedCount,
			"errors", errorCount,
		).V(1).Info("pod deletion cost annotation update completed")
	}
}

// processSinglePod handles the annotation update for a single pod. Returns
// (result, err) so the caller can aggregate errors via errors.Join.
//
// NotFound and Conflict are both classified as Skipped: NotFound means the
// pod is already gone, Conflict means another writer raced us and we'll
// retry on the next reconcile. Neither is an operator-visible failure.
func (a *AnnotationManager) processSinglePod(ctx context.Context, pod *corev1.Pod, rank int) (podResult, error) {
	if !needsUpdate(pod, rank) {
		return podResultSkipped, nil
	}
	if err := a.patchAnnotation(ctx, pod, strconv.Itoa(rank)); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("pod not found, skipping annotation update")
			return podResultSkipped, nil
		}
		if apierrors.IsConflict(err) {
			log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("conflict updating pod annotation, will retry on next reconcile")
			return podResultSkipped, nil
		}
		a.recorder.Publish(UpdateFailedEvent(pod, err))
		return podResultError, err
	}
	return podResultSuccess, nil
}

// clearDeletionCost removes the pod-deletion-cost annotation from a pod when
// the node's group is do-not-disrupt. The annotation is only cleared if it is
// currently set, to avoid an unnecessary API write. Errors are returned so
// the caller can aggregate them; logging happens once at the caller. The
// caller's pod object is not mutated even when the patch is built; we mutate
// a deep-copy.
func (a *AnnotationManager) clearDeletionCost(ctx context.Context, pod *corev1.Pod) error {
	if pod.Annotations == nil {
		return nil
	}
	if _, ok := pod.Annotations[corev1.PodDeletionCost]; !ok {
		return nil
	}
	updated := pod.DeepCopy()
	delete(updated.Annotations, corev1.PodDeletionCost)
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	if err := a.kubeClient.Patch(ctx, updated, patch); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// Pod gone or conflicted; we'll retry next cycle.
			return nil
		}
		return err
	}
	return nil
}

// CleanupNodeAnnotations removes pod-deletion-cost annotations from all pods
// on a node. Used when a node drops out of the ranked set so its pods are no
// longer being managed. Errors from individual pod patches are aggregated so
// callers see the full failure set.
func (a *AnnotationManager) CleanupNodeAnnotations(ctx context.Context, nodeName string) error {
	var podList corev1.PodList
	if err := a.kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return err
	}
	var aggErr error
	for i := range podList.Items {
		pod := &podList.Items[i]
		if err := a.clearDeletionCost(ctx, pod); err != nil {
			aggErr = errors.Join(aggErr, err)
		}
	}
	return aggErr
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
func (a *AnnotationManager) patchAnnotation(ctx context.Context, pod *corev1.Pod, value string) error {
	updated := pod.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[corev1.PodDeletionCost] = value
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	return a.kubeClient.Patch(ctx, updated, patch)
}
