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
	"sync"
	"time"

	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	terminatorevents "sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator/events"
	"sigs.k8s.io/karpenter/pkg/events"
	nodeutil "sigs.k8s.io/karpenter/pkg/utils/node"
	podutil "sigs.k8s.io/karpenter/pkg/utils/pod"
)

type Terminator struct {
	sync.RWMutex
	clock                  clock.Clock
	kubeClient             client.Client
	nodeRestartDeployments map[string]map[string]struct{}
	evictionQueue          *Queue
	recorder               events.Recorder
}

func NewTerminator(clk clock.Clock, kubeClient client.Client, eq *Queue, recorder events.Recorder) *Terminator {
	return &Terminator{
		clock:                  clk,
		kubeClient:             kubeClient,
		nodeRestartDeployments: make(map[string]map[string]struct{}),
		evictionQueue:          eq,
		recorder:               recorder,
	}
}

// Taint idempotently adds a given taint to a node with a NodeClaim
func (t *Terminator) Taint(ctx context.Context, node *corev1.Node, taint corev1.Taint) error {
	stored := node.DeepCopy()
	// If the node already has the correct taint (key and effect), do nothing.
	if _, ok := lo.Find(node.Spec.Taints, func(t corev1.Taint) bool {
		return t.MatchTaint(&taint)
	}); !ok {
		// Otherwise, if the taint key exists (but with a different effect), remove it.
		node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t corev1.Taint, _ int) bool {
			return t.Key == taint.Key
		})
		node.Spec.Taints = append(node.Spec.Taints, taint)
	}
	// Adding this label to the node ensures that the node is removed from the load-balancer target group
	// while it is draining and before it is terminated. This prevents 500s coming prior to health check
	// when the load balancer controller hasn't yet determined that the node and underlying connections are gone
	// https://github.com/aws/aws-node-termination-handler/issues/316
	// https://github.com/aws/karpenter/pull/2518
	node.Labels = lo.Assign(node.Labels, map[string]string{
		corev1.LabelNodeExcludeBalancers: "karpenter",
	})
	if !equality.Semantic.DeepEqual(node, stored) {
		// We use client.MergeFromWithOptimisticLock because patching a list with a JSON merge patch
		// can cause races due to the fact that it fully replaces the list on a change
		// Here, we are updating the taint list
		if err := t.kubeClient.Patch(ctx, node, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		taintValues := []any{
			"taint.Key", taint.Key,
			"taint.Value", taint.Value,
		}
		if len(string(taint.Effect)) > 0 {
			taintValues = append(taintValues, "taint.Effect", taint.Effect)
		}
		log.FromContext(ctx).WithValues(taintValues...).Info("tainted node")
	}
	return nil
}

// Drain evicts pods from the node and returns true when all pods are evicted
// https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
func (t *Terminator) Drain(ctx context.Context, node *corev1.Node, nodeGracePeriodExpirationTime *time.Time) error {
	pods, err := nodeutil.GetPods(ctx, t.kubeClient, node)
	if err != nil {
		return fmt.Errorf("listing pods on node, %w", err)
	}
	podsToDelete := lo.Filter(pods, func(p *corev1.Pod, _ int) bool {
		return podutil.IsWaitingEviction(p, t.clock) && !podutil.IsTerminating(p)
	})
	if err := t.DeleteExpiringPods(ctx, podsToDelete, nodeGracePeriodExpirationTime); err != nil {
		return fmt.Errorf("deleting expiring pods, %w", err)
	}
	// Monitor pods in pod groups that either haven't been evicted or are actively evicting
	podGroups := t.groupPodsByPriority(lo.Filter(pods, func(p *corev1.Pod, _ int) bool { return podutil.IsWaitingEviction(p, t.clock) }))
	for _, group := range podGroups {
		if len(group) > 0 {
			// If the deployment corresponding to the pod has only one pod,
			// or all the pods of the deployment are on this node,
			// restarting the deployment can reduce the service interruption time.
			var drainPods []*corev1.Pod
			var restartDeployments []*appsv1.Deployment
			deletionDeadline := node.GetDeletionTimestamp().Add(5 * time.Minute)

			// 5 minutes is roughly based on (maximum time) = 2 minutes to pull the node + 2 minutes to start the service + 1 minute to terminate.
			// If the restart is not completed after this time, it is possible that the new pod of the deployment cannot be started and the old pod will not be deleted.
			if time.Now().Before(deletionDeadline) {
				restartDeployments, drainPods, err = t.GetRestartdeploymentsAndDrainPods(ctx, group, node.Name)
				if err != nil {
					return fmt.Errorf("get deployment and drain pod from node %w", err)
				}
			} else {
				drainPods = pods
			}

			if err = t.RestartDeployments(ctx, restartDeployments, node.Name); err != nil {
				return fmt.Errorf("restart deployments from node %s, %w", node.Name, err)
			}

			// Only add pods to the eviction queue that haven't been evicted yet
			t.evictionQueue.Add(lo.Filter(drainPods, func(p *corev1.Pod, _ int) bool { return podutil.IsEvictable(p) })...)
			return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", lo.SumBy(podGroups, func(pods []*corev1.Pod) int { return len(pods) })))
		}
	}

	t.Lock()
	delete(t.nodeRestartDeployments, node.Name)
	t.Unlock()
	return nil
}

func (t *Terminator) groupPodsByPriority(pods []*corev1.Pod) [][]*corev1.Pod {
	// 1. Prioritize noncritical pods, non-daemon pods https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
	var nonCriticalNonDaemon, nonCriticalDaemon, criticalNonDaemon, criticalDaemon []*corev1.Pod
	for _, pod := range pods {
		if pod.Spec.PriorityClassName == "system-cluster-critical" || pod.Spec.PriorityClassName == "system-node-critical" {
			if podutil.IsOwnedByDaemonSet(pod) {
				criticalDaemon = append(criticalDaemon, pod)
			} else {
				criticalNonDaemon = append(criticalNonDaemon, pod)
			}
		} else {
			if podutil.IsOwnedByDaemonSet(pod) {
				nonCriticalDaemon = append(nonCriticalDaemon, pod)
			} else {
				nonCriticalNonDaemon = append(nonCriticalNonDaemon, pod)
			}
		}
	}
	return [][]*corev1.Pod{nonCriticalNonDaemon, nonCriticalDaemon, criticalNonDaemon, criticalDaemon}
}

func (t *Terminator) DeleteExpiringPods(ctx context.Context, pods []*corev1.Pod, nodeGracePeriodTerminationTime *time.Time) error {
	for _, pod := range pods {
		// check if the node has an expiration time and the pod needs to be deleted
		deleteTime := t.podDeleteTimeWithGracePeriod(nodeGracePeriodTerminationTime, pod)
		if deleteTime != nil && time.Now().After(*deleteTime) {
			// delete pod proactively to give as much of its terminationGracePeriodSeconds as possible for deletion
			// ensure that we clamp the maximum pod terminationGracePeriodSeconds to the node's remaining expiration time in the delete command
			gracePeriodSeconds := lo.ToPtr(int64(time.Until(*nodeGracePeriodTerminationTime).Seconds()))
			t.recorder.Publish(terminatorevents.DisruptPodDelete(pod, gracePeriodSeconds, nodeGracePeriodTerminationTime))
			opts := &client.DeleteOptions{
				GracePeriodSeconds: gracePeriodSeconds,
			}
			if err := t.kubeClient.Delete(ctx, pod, opts); err != nil && !apierrors.IsNotFound(err) { // ignore 404, not a problem
				return fmt.Errorf("deleting pod, %w", err) // otherwise, bubble up the error
			}
			log.FromContext(ctx).WithValues(
				"namespace", pod.Namespace,
				"name", pod.Name,
				"pod.terminationGracePeriodSeconds", *pod.Spec.TerminationGracePeriodSeconds,
				"delete.gracePeriodSeconds", *gracePeriodSeconds,
				"nodeclaim.terminationTime", *nodeGracePeriodTerminationTime,
			).V(1).Info("deleting pod")
		}
	}
	return nil
}

// if a pod should be deleted to give it the full terminationGracePeriodSeconds of time before the node will shut down, return the time the pod should be deleted
func (t *Terminator) podDeleteTimeWithGracePeriod(nodeGracePeriodExpirationTime *time.Time, pod *corev1.Pod) *time.Time {
	if nodeGracePeriodExpirationTime == nil || pod.Spec.TerminationGracePeriodSeconds == nil { // k8s defaults to 30s, so we should never see a nil TerminationGracePeriodSeconds
		return nil
	}

	// calculate the time the pod should be deleted to allow it's full grace period for termination, equal to its terminationGracePeriodSeconds before the node's expiration time
	// eg: if a node will be force terminated in 30m, but the current pod has a grace period of 45m, we return a time of 15m ago
	deleteTime := nodeGracePeriodExpirationTime.Add(time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second * -1)
	return &deleteTime
}

func (t *Terminator) GetDeploymentFromPod(ctx context.Context, pod *corev1.Pod) (*appsv1.Deployment, error) {
	rs, err := t.getOwnerReplicaSet(ctx, pod)
	if err != nil {
		return nil, fmt.Errorf("failed to get ReplicaSet from Pod: %w", err)
	}
	if rs == nil {
		return nil, nil
	}

	deployment, err := t.getOwnerDeployment(ctx, rs)
	if err != nil {
		return nil, fmt.Errorf("failed to get Deployment from ReplicaSet: %w", err)
	}
	return deployment, nil

}

func (t *Terminator) getOwnerReplicaSet(ctx context.Context, pod *corev1.Pod) (*appsv1.ReplicaSet, error) {
	for _, ownerRef := range pod.GetOwnerReferences() {
		if ownerRef.Controller != nil && ownerRef.Kind == "ReplicaSet" {
			rs := &appsv1.ReplicaSet{}
			if err := t.kubeClient.Get(ctx, client.ObjectKey{Name: ownerRef.Name, Namespace: pod.Namespace}, rs); err != nil {
				return nil, fmt.Errorf("get ReplicaSet: %w", err)
			}
			return rs, nil
		}
	}

	return nil, nil
}

func (t *Terminator) getOwnerDeployment(ctx context.Context, rs *appsv1.ReplicaSet) (*appsv1.Deployment, error) {
	for _, ownerRef := range rs.GetOwnerReferences() {
		if ownerRef.Controller != nil && ownerRef.Kind == "Deployment" {
			deployment := &appsv1.Deployment{}
			if err := t.kubeClient.Get(ctx, client.ObjectKey{Name: ownerRef.Name, Namespace: rs.Namespace}, deployment); err != nil {
				return nil, fmt.Errorf("get Deployment: %w", err)
			}
			return deployment, nil
		}
	}

	return nil, nil
}

func (t *Terminator) RestartDeployments(ctx context.Context, deployments []*appsv1.Deployment, nodeName string) error {
	var updateErrors []error

	for _, deployment := range deployments {
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = make(map[string]string)
		}
		restartedNode, exists := deployment.Spec.Template.Annotations[v1.DeploymentRestartNodeAnnotationKey]
		if exists && restartedNode == nodeName {
			continue
		}

		t.Lock()
		if t.nodeRestartDeployments[nodeName] == nil {
			t.nodeRestartDeployments[nodeName] = make(map[string]struct{})
		}
		t.nodeRestartDeployments[nodeName][deployment.Namespace+"/"+deployment.Name] = struct{}{}
		t.Unlock()

		deployment.Spec.Template.Annotations[v1.DeploymentRestartNodeAnnotationKey] = nodeName
		if err := t.kubeClient.Update(ctx, deployment); err != nil {
			updateErrors = append(updateErrors, err)
			continue
		}

	}

	if len(updateErrors) > 0 {
		return fmt.Errorf("failed to restart some deployment: %v", updateErrors)
	}

	return nil
}

func (t *Terminator) GetRestartdeploymentsAndDrainPods(ctx context.Context, pods []*corev1.Pod, nodeName string) ([]*appsv1.Deployment, []*corev1.Pod, error) {
	var drainPods []*corev1.Pod
	var restartDeployments []*appsv1.Deployment
	nodeDeploymentReplicas := make(map[string]int32)
	deploymentCache := make(map[string]*appsv1.Deployment)
	uniqueDeployments := make(map[string]struct{})

	for _, pod := range pods {
		deployment, err := t.getDeploymentFromCache(ctx, pod, deploymentCache)
		if err != nil {
			return nil, nil, err
		}
		if deployment != nil {
			nodeDeploymentReplicas[deployment.Namespace+"/"+deployment.Name]++
		}
	}

	for _, pod := range pods {
		deployment := deploymentCache[pod.Namespace+"/"+pod.Name]

		if deployment != nil {
			key := deployment.Namespace + "/" + deployment.Name
			if nodeDeploymentReplicas[key] >= *deployment.Spec.Replicas {
				// If a deployment has multiple pods on this node, there will be multiple deployments here, and deduplication is required.
				if _, exists := uniqueDeployments[key]; !exists {
					uniqueDeployments[key] = struct{}{}
					restartDeployments = append(restartDeployments, deployment)
				}
				continue
			} else {
				// If a deployment has multiple copies, all of which are on this node,
				// when the restart begins, the number of copies of the deployment on this node will gradually decrease.
				// This situation needs to be judged separately.
				t.RLock()
				_, exists := t.nodeRestartDeployments[nodeName][key]
				t.RUnlock()
				if exists {
					continue
				}
			}
		}

		drainPods = append(drainPods, pod)
	}

	return restartDeployments, drainPods, nil
}

func (t *Terminator) getDeploymentFromCache(ctx context.Context, pod *corev1.Pod, cache map[string]*appsv1.Deployment) (*appsv1.Deployment, error) {
	key := pod.Namespace + "/" + pod.Name
	if deployment, exists := cache[key]; exists {
		return deployment, nil
	}

	deployment, err := t.GetDeploymentFromPod(ctx, pod)
	if err != nil {
		return nil, err
	}

	cache[key] = deployment
	return deployment, nil
}
