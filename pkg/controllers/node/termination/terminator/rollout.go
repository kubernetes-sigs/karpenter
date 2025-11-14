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

package terminator

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	podutil "sigs.k8s.io/karpenter/pkg/utils/pod"
)

const (
	rolloutAnnotationKey = "karpenter.sh/rollout-trigger"
	rolloutTimeout       = 5 * time.Minute
	rolloutCheckInterval = 5 * time.Second
)

// RolloutStrategy handles graceful rollouts of workloads during node consolidation
type RolloutStrategy struct {
	kubeClient client.Client
}

// NewRolloutStrategy creates a new RolloutStrategy
func NewRolloutStrategy(kubeClient client.Client) *RolloutStrategy {
	return &RolloutStrategy{
		kubeClient: kubeClient,
	}
}

// TriggerGracefulRollout triggers a rolling update for the pod's controller
// Returns true if rollout was triggered, false if should fall back to eviction
func (r *RolloutStrategy) TriggerGracefulRollout(ctx context.Context, pod *corev1.Pod) (bool, error) {
	// Skip DaemonSets - they don't support rolling updates the same way
	if podutil.IsOwnedByDaemonSet(pod) {
		log.FromContext(ctx).V(1).Info("skipping graceful rollout for DaemonSet pod", "pod", pod.Name)
		return false, nil
	}

	// Get the owner reference
	ownerRef := metav1.GetControllerOf(pod)
	if ownerRef == nil {
		// Standalone pod - fall back to eviction
		log.FromContext(ctx).V(1).Info("pod has no controller, falling back to eviction", "pod", pod.Name)
		return false, nil
	}

	log.FromContext(ctx).V(1).Info("attempting graceful rollout", "pod", pod.Name, "controller", ownerRef.Kind)

	switch ownerRef.Kind {
	case "Deployment":
		// For Deployment-owned pods, we need to get through the ReplicaSet
		return r.rolloutViaReplicaSet(ctx, pod, ownerRef)
	case "StatefulSet":
		return r.rolloutStatefulSet(ctx, pod, ownerRef)
	case "ReplicaSet":
		// Check if ReplicaSet has a Deployment parent
		return r.rolloutReplicaSet(ctx, pod, ownerRef)
	default:
		// Unsupported controller - fall back to eviction
		log.FromContext(ctx).V(1).Info("unsupported controller type, falling back to eviction", "pod", pod.Name, "controller", ownerRef.Kind)
		return false, nil
	}
}

func (r *RolloutStrategy) rolloutViaReplicaSet(ctx context.Context, pod *corev1.Pod, rsOwnerRef *metav1.OwnerReference) (bool, error) {
	// Get the ReplicaSet
	rs := &appsv1.ReplicaSet{}
	if err := r.kubeClient.Get(ctx, types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      rsOwnerRef.Name,
	}, rs); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting replicaset: %w", err)
	}

	// Get the Deployment
	deploymentOwner := metav1.GetControllerOf(rs)
	if deploymentOwner == nil || deploymentOwner.Kind != "Deployment" {
		log.FromContext(ctx).V(1).Info("replicaset has no deployment parent", "replicaset", rs.Name)
		return false, nil
	}

	return r.rolloutDeployment(ctx, pod, deploymentOwner)
}

func (r *RolloutStrategy) rolloutDeployment(ctx context.Context, pod *corev1.Pod, deploymentOwnerRef *metav1.OwnerReference) (bool, error) {
	deployment := &appsv1.Deployment{}
	if err := r.kubeClient.Get(ctx, types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      deploymentOwnerRef.Name,
	}, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting deployment: %w", err)
	}

	// Check if deployment has only 1 replica and no maxUnavailable allows for surge
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 1 {
		// Check the rolling update strategy
		if deployment.Spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType &&
			deployment.Spec.Strategy.RollingUpdate != nil {
			maxSurge := deployment.Spec.Strategy.RollingUpdate.MaxSurge
			// Only proceed if maxSurge allows creating a new pod (surge > 0)
			if maxSurge != nil && (maxSurge.IntVal > 0 || (maxSurge.Type == 1 && maxSurge.StrVal != "0%")) {
				log.FromContext(ctx).V(1).Info("single replica deployment with surge, proceeding with rollout",
					"deployment", deployment.Name, "maxSurge", maxSurge.String())
			} else {
				log.FromContext(ctx).V(1).Info("single replica deployment without surge, falling back to eviction",
					"deployment", deployment.Name)
				return false, nil
			}
		}
	}

	// Trigger rolling update by adding/updating a restart annotation
	stored := deployment.DeepCopy()
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}

	// Add karpenter-specific annotation to trigger rollout
	deployment.Spec.Template.Annotations[rolloutAnnotationKey] = time.Now().Format(time.RFC3339)

	if err := r.kubeClient.Patch(ctx, deployment, client.MergeFrom(stored)); err != nil {
		return false, fmt.Errorf("patching deployment for rollout: %w", err)
	}

	log.FromContext(ctx).Info("triggered deployment rollout", "deployment", deployment.Name, "namespace", deployment.Namespace)
	return true, nil
}

func (r *RolloutStrategy) rolloutStatefulSet(ctx context.Context, pod *corev1.Pod, ownerRef *metav1.OwnerReference) (bool, error) {
	statefulSet := &appsv1.StatefulSet{}
	if err := r.kubeClient.Get(ctx, types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      ownerRef.Name,
	}, statefulSet); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting statefulset: %w", err)
	}

	// StatefulSets with OnDelete update strategy won't automatically rollout
	if statefulSet.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		log.FromContext(ctx).V(1).Info("statefulset uses OnDelete strategy, falling back to eviction",
			"statefulset", statefulSet.Name)
		return false, nil
	}

	// Trigger rolling update
	stored := statefulSet.DeepCopy()
	if statefulSet.Spec.Template.Annotations == nil {
		statefulSet.Spec.Template.Annotations = make(map[string]string)
	}

	statefulSet.Spec.Template.Annotations[rolloutAnnotationKey] = time.Now().Format(time.RFC3339)

	if err := r.kubeClient.Patch(ctx, statefulSet, client.MergeFrom(stored)); err != nil {
		return false, fmt.Errorf("patching statefulset for rollout: %w", err)
	}

	log.FromContext(ctx).Info("triggered statefulset rollout", "statefulset", statefulSet.Name, "namespace", statefulSet.Namespace)
	return true, nil
}

func (r *RolloutStrategy) rolloutReplicaSet(ctx context.Context, pod *corev1.Pod, ownerRef *metav1.OwnerReference) (bool, error) {
	rs := &appsv1.ReplicaSet{}
	if err := r.kubeClient.Get(ctx, types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      ownerRef.Name,
	}, rs); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting replicaset: %w", err)
	}

	// Check if it has a Deployment parent
	deploymentOwner := metav1.GetControllerOf(rs)
	if deploymentOwner != nil && deploymentOwner.Kind == "Deployment" {
		return r.rolloutDeployment(ctx, pod, deploymentOwner)
	}

	// Standalone ReplicaSet - fall back to eviction
	log.FromContext(ctx).V(1).Info("standalone replicaset, falling back to eviction", "replicaset", rs.Name)
	return false, nil
}

// WaitForNewPodReady waits for a new pod to be created and ready on a different node
// Returns nil when a new pod is ready, or an error on timeout
func (r *RolloutStrategy) WaitForNewPodReady(ctx context.Context, pod *corev1.Pod, originalNodeName string) error {
	log.FromContext(ctx).V(1).Info("waiting for new pod to be ready on different node",
		"originalPod", pod.Name, "originalNode", originalNodeName)

	// Create a timeout context
	timeoutCtx, cancel := context.WithTimeout(ctx, rolloutTimeout)
	defer cancel()

	ticker := time.NewTicker(rolloutCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for new pod to be ready after %v", rolloutTimeout)
		case <-ticker.C:
			// Check if there's a ready pod from the same controller on a different node
			ready, err := r.hasReadyPodOnDifferentNode(ctx, pod, originalNodeName)
			if err != nil {
				log.FromContext(ctx).Error(err, "error checking for ready pod on different node")
				continue
			}
			if ready {
				log.FromContext(ctx).Info("new pod is ready on different node", "originalPod", pod.Name)
				return nil
			}
		}
	}
}

func (r *RolloutStrategy) hasReadyPodOnDifferentNode(ctx context.Context, originalPod *corev1.Pod, originalNodeName string) (bool, error) {
	podList := &corev1.PodList{}

	// Build label selector from the original pod's labels
	// Remove pod-specific labels that won't match siblings
	matchLabels := make(map[string]string)
	for k, v := range originalPod.Labels {
		// Skip pod-template-hash and controller-revision-hash as they're pod-specific
		if k != "pod-template-hash" && k != "controller-revision-hash" {
			matchLabels[k] = v
		}
	}

	// If we have no labels to match, we can't find sibling pods
	if len(matchLabels) == 0 {
		return false, nil
	}

	listOpts := []client.ListOption{
		client.InNamespace(originalPod.Namespace),
		client.MatchingLabels(matchLabels),
	}

	if err := r.kubeClient.List(ctx, podList, listOpts...); err != nil {
		return false, fmt.Errorf("listing pods: %w", err)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]

		// Skip the original pod
		if pod.UID == originalPod.UID {
			continue
		}

		// Check if it's on a different node and ready
		if pod.Spec.NodeName != originalNodeName && pod.Spec.NodeName != "" {
			// Check if pod is ready
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					log.FromContext(ctx).V(1).Info("found ready pod on different node",
						"newPod", pod.Name, "newNode", pod.Spec.NodeName,
						"originalPod", originalPod.Name, "originalNode", originalNodeName)
					return true, nil
				}
			}
		}
	}

	return false, nil
}
