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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
}

// NewAnnotationManager creates a new AnnotationManager
func NewAnnotationManager(kubeClient client.Client, recorder events.Recorder) *AnnotationManager {
	return &AnnotationManager{
		kubeClient: kubeClient,
		recorder:   recorder,
	}
}

// UpdatePodDeletionCosts updates pod deletion cost annotations for all pods on the ranked nodes
func (a *AnnotationManager) UpdatePodDeletionCosts(ctx context.Context, nodeRanks []NodeRank) error {
	// Measure annotation update duration
	defer metrics.Measure(AnnotationDurationSeconds, map[string]string{})()

	var successCount, skippedCount, errorCount int

	// Process each ranked node
	for _, nodeRank := range nodeRanks {
		// Get all pods on this node
		pods, err := nodeRank.Node.Pods(ctx, a.kubeClient)
		if err != nil {
			log.FromContext(ctx).WithValues("node", nodeRank.Node.Name()).Error(err, "failed to list pods on node")
			errorCount++
			continue
		}

		// Build list of pods to update
		podsToUpdate := []PodUpdate{}
		for _, pod := range pods {
			// Check if pod needs updating
			if shouldUpdatePod(pod) {
				podsToUpdate = append(podsToUpdate, PodUpdate{
					Pod:       pod,
					NewRank:   nodeRank.Rank,
					ShouldAdd: !hasDeletionCostAnnotation(pod),
				})
			} else {
				skippedCount++
			}
		}

		// Batch update pods with new deletion cost
		for _, podUpdate := range podsToUpdate {
			if err := a.updatePodAnnotation(ctx, podUpdate); err != nil {
				// Handle pod not found errors gracefully (pod was deleted)
				if apierrors.IsNotFound(err) {
					log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(podUpdate.Pod)).Info("pod not found, skipping annotation update")
					continue
				}

				// Handle conflict errors (will retry on next reconcile)
				if apierrors.IsConflict(err) {
					log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(podUpdate.Pod)).Info("conflict updating pod annotation, will retry on next reconcile")
					errorCount++
					continue
				}

				// Log warnings for other failures and continue processing other pods
				log.FromContext(ctx).WithValues("pod", klog.KObj(podUpdate.Pod)).Error(err, "failed to update pod deletion cost annotation")
				// Publish event for failed update
				a.recorder.Publish(UpdateFailedEvent(podUpdate.Pod, err))
				errorCount++
				continue
			}
			successCount++
		}
	}

	// Record metrics
	PodsUpdatedTotal.Add(float64(successCount), map[string]string{
		resultLabel: "success",
	})
	PodsUpdatedTotal.Add(float64(skippedCount), map[string]string{
		resultLabel: "skipped_customer_managed",
	})
	PodsUpdatedTotal.Add(float64(errorCount), map[string]string{
		resultLabel: "error",
	})

	// Log summary
	if successCount > 0 || errorCount > 0 {
		log.FromContext(ctx).WithValues(
			"success", successCount,
			"skipped", skippedCount,
			"errors", errorCount,
		).V(1).Info("pod deletion cost annotation update completed")
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
// Returns true if:
// - Pod has no deletion cost annotation (we should add it)
// - Pod has deletion cost AND Karpenter management annotation (we manage it)
// Returns false if:
// - Pod has deletion cost but NO Karpenter management annotation (customer-managed)
func shouldUpdatePod(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		// No annotations at all, we can add our annotation
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

	// If pod has deletion cost but no management annotation, it's customer-managed
	if hasDeletionCost && !hasManagedAnnotation {
		return false
	}

	// Otherwise, we should update it
	return true
}

// updatePodAnnotation updates a single pod's deletion cost annotation
// It also adds the Karpenter management tracking annotation
func (a *AnnotationManager) updatePodAnnotation(ctx context.Context, podUpdate PodUpdate) error {
	pod := podUpdate.Pod.DeepCopy()

	// Initialize annotations map if it doesn't exist
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}

	// Set the deletion cost annotation
	pod.Annotations[PodDeletionCostAnnotation] = fmt.Sprintf("%d", podUpdate.NewRank)

	// Add the Karpenter management tracking annotation
	pod.Annotations[KarpenterManagedDeletionCostAnnotation] = "true"

	// Update the pod
	if err := a.kubeClient.Update(ctx, pod); err != nil {
		return err
	}

	return nil
}
