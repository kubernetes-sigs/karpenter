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
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	// PodDeletionCostAnnotation is the Kubernetes annotation that influences pod termination priority
	// Lower values indicate higher deletion priority
	PodDeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"
	// KarpenterManagedDeletionCostAnnotation tracks whether Karpenter is managing the deletion cost
	KarpenterManagedDeletionCostAnnotation = "karpenter.sh/managed-deletion-cost"
)

// PodUpdate represents a pod that needs its deletion cost annotation updated
type PodUpdate struct {
	Pod       *corev1.Pod
	NewRank   int
	ShouldAdd bool // true if adding annotation, false if updating
}

// AnnotationManager handles pod deletion cost annotation updates
type AnnotationManager struct {
	kubeClient client.Client
	recorder   events.Recorder

	mu                 sync.Mutex
	lastAssignedValues map[types.UID]string // tracks what Karpenter last set per pod
}

// NewAnnotationManager creates a new AnnotationManager
func NewAnnotationManager(kubeClient client.Client, recorder events.Recorder) *AnnotationManager {
	return &AnnotationManager{
		kubeClient:         kubeClient,
		recorder:           recorder,
		lastAssignedValues: make(map[types.UID]string),
	}
}

// podResult tracks the outcome of processing a single pod
type podResult int

const (
	podResultSuccess podResult = iota
	podResultSkipped
	podResultError
)

// UpdatePodDeletionCosts updates pod deletion cost annotations for all pods on the ranked nodes
func (a *AnnotationManager) UpdatePodDeletionCosts(ctx context.Context, nodeRanks []NodeRank) error {
	defer metrics.Measure(AnnotationDurationSeconds, map[string]string{})()

	var successCount, skippedCount, errorCount int

	// Track which pod UIDs are still active for cleanup
	activePods := make(map[types.UID]bool)

	for _, nodeRank := range nodeRanks {
		pods, err := nodeRank.Node.Pods(ctx, a.kubeClient)
		if err != nil {
			log.FromContext(ctx).WithValues("node", nodeRank.Node.Name()).Error(err, "failed to list pods on node")
			errorCount++
			continue
		}

		// Group D (do-not-disrupt) nodes: clear any existing Karpenter-managed
		// annotations instead of assigning a rank.
		if nodeRank.HasDoNotDisrupt {
			for _, pod := range pods {
				activePods[pod.UID] = true
				if err := a.clearManagedAnnotations(ctx, pod); err != nil {
					errorCount++
				}
			}
			continue
		}

		for _, pod := range pods {
			activePods[pod.UID] = true
			switch a.processSinglePod(ctx, pod, nodeRank.Rank) {
			case podResultSuccess:
				successCount++
			case podResultSkipped:
				skippedCount++
			case podResultError:
				errorCount++
			}
		}
	}

	a.cleanupStalePods(activePods)
	a.recordMetrics(ctx, successCount, skippedCount, errorCount)

	return nil
}

// cleanupStalePods removes tracking entries for pods no longer on any ranked node.
func (a *AnnotationManager) cleanupStalePods(activePods map[types.UID]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for uid := range a.lastAssignedValues {
		if !activePods[uid] {
			delete(a.lastAssignedValues, uid)
		}
	}
}

// recordMetrics emits pod update metrics and logs the summary.
func (a *AnnotationManager) recordMetrics(ctx context.Context, successCount, skippedCount, errorCount int) {
	PodsUpdatedTotal.Add(float64(successCount), map[string]string{resultLabel: "success"})
	PodsUpdatedTotal.Add(float64(skippedCount), map[string]string{resultLabel: "skipped_customer_managed"})
	PodsUpdatedTotal.Add(float64(errorCount), map[string]string{resultLabel: "error"})

	if successCount > 0 || errorCount > 0 {
		log.FromContext(ctx).WithValues(
			"success", successCount,
			"skipped", skippedCount,
			"errors", errorCount,
		).V(1).Info("pod deletion cost annotation update completed")
	}
}

// processSinglePod handles the annotation update logic for a single pod, returning the outcome.
func (a *AnnotationManager) processSinglePod(ctx context.Context, pod *corev1.Pod, rank int) podResult {
	if a.isExternallyModified(pod) {
		if err := a.removeSentinelAnnotation(ctx, pod); err != nil {
			log.FromContext(ctx).WithValues("pod", klog.KObj(pod)).Error(err, "failed to remove sentinel annotation from externally modified pod")
			return podResultError
		}
		a.recorder.Publish(ThirdPartyConflictEvent(pod))
		a.mu.Lock()
		delete(a.lastAssignedValues, pod.UID)
		a.mu.Unlock()
		return podResultSkipped
	}

	if !shouldUpdatePod(pod) {
		return podResultSkipped
	}

	podUpdate := PodUpdate{
		Pod:       pod,
		NewRank:   rank,
		ShouldAdd: !hasDeletionCostAnnotation(pod),
	}
	if err := a.updatePodAnnotation(ctx, podUpdate); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("pod not found, skipping annotation update")
			return podResultSkipped
		}
		if apierrors.IsConflict(err) {
			log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info("conflict updating pod annotation, will retry on next reconcile")
			return podResultError
		}
		log.FromContext(ctx).WithValues("pod", klog.KObj(pod)).Error(err, "failed to update pod deletion cost annotation")
		a.recorder.Publish(UpdateFailedEvent(pod, err))
		return podResultError
	}

	a.mu.Lock()
	a.lastAssignedValues[pod.UID] = fmt.Sprintf("%d", rank)
	a.mu.Unlock()
	return podResultSuccess
}

// isExternallyModified checks if the pod's deletion cost annotation was changed
// by a third party since Karpenter last set it.
func (a *AnnotationManager) isExternallyModified(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}
	// Only check pods we previously managed
	_, hasManaged := pod.Annotations[KarpenterManagedDeletionCostAnnotation]
	if !hasManaged {
		return false
	}
	currentVal, hasCost := pod.Annotations[PodDeletionCostAnnotation]
	if !hasCost {
		return false
	}
	a.mu.Lock()
	lastVal, tracked := a.lastAssignedValues[pod.UID]
	a.mu.Unlock()
	if !tracked {
		return false
	}
	return currentVal != lastVal
}

// clearManagedAnnotations removes both the deletion cost and sentinel annotations from a
// Karpenter-managed pod. Used for Group D (do-not-disrupt) nodes that should not carry
// any Karpenter-set deletion cost.
func (a *AnnotationManager) clearManagedAnnotations(ctx context.Context, pod *corev1.Pod) error {
	if pod.Annotations == nil {
		return nil
	}
	if _, ok := pod.Annotations[KarpenterManagedDeletionCostAnnotation]; !ok {
		return nil
	}
	updated := pod.DeepCopy()
	delete(updated.Annotations, KarpenterManagedDeletionCostAnnotation)
	delete(updated.Annotations, PodDeletionCostAnnotation)
	if err := a.kubeClient.Update(ctx, updated); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		log.FromContext(ctx).WithValues("pod", klog.KObj(pod)).Error(err, "failed to clear managed annotations on do-not-disrupt node")
		return err
	}
	a.mu.Lock()
	delete(a.lastAssignedValues, pod.UID)
	a.mu.Unlock()
	return nil
}

// removeSentinelAnnotation removes the Karpenter management sentinel from a pod
func (a *AnnotationManager) removeSentinelAnnotation(ctx context.Context, pod *corev1.Pod) error {
	updated := pod.DeepCopy()
	delete(updated.Annotations, KarpenterManagedDeletionCostAnnotation)
	return a.kubeClient.Update(ctx, updated)
}

// CleanupNodeAnnotations removes Karpenter-managed deletion cost annotations from all pods on a node
func (a *AnnotationManager) CleanupNodeAnnotations(ctx context.Context, nodeName string) error {
	var podList corev1.PodList
	if err := a.kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return err
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Annotations == nil {
			continue
		}
		if _, ok := pod.Annotations[KarpenterManagedDeletionCostAnnotation]; !ok {
			continue
		}
		updated := pod.DeepCopy()
		delete(updated.Annotations, KarpenterManagedDeletionCostAnnotation)
		delete(updated.Annotations, PodDeletionCostAnnotation)
		if err := a.kubeClient.Update(ctx, updated); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			log.FromContext(ctx).WithValues("pod", klog.KObj(pod)).Error(err, "failed to clean up pod deletion cost annotation")
		}
		a.mu.Lock()
		delete(a.lastAssignedValues, pod.UID)
		a.mu.Unlock()
	}
	return nil
}

// hasDeletionCostAnnotation checks if a pod has the deletion cost annotation
func hasDeletionCostAnnotation(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}
	_, exists := pod.Annotations[PodDeletionCostAnnotation]
	return exists
}

// shouldUpdatePod determines if a pod should have its deletion cost updated
func shouldUpdatePod(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		return true
	}
	hasDeletionCost := false
	hasManagedAnnotation := false
	if _, exists := pod.Annotations[PodDeletionCostAnnotation]; exists {
		hasDeletionCost = true
	}
	if _, exists := pod.Annotations[KarpenterManagedDeletionCostAnnotation]; exists {
		hasManagedAnnotation = true
	}
	// Customer-managed: has cost but no sentinel
	if hasDeletionCost && !hasManagedAnnotation {
		return false
	}
	return true
}

// updatePodAnnotation updates a single pod's deletion cost annotation
func (a *AnnotationManager) updatePodAnnotation(ctx context.Context, podUpdate PodUpdate) error {
	pod := podUpdate.Pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[PodDeletionCostAnnotation] = fmt.Sprintf("%d", podUpdate.NewRank)
	pod.Annotations[KarpenterManagedDeletionCostAnnotation] = "true"
	return a.kubeClient.Update(ctx, pod)
}
