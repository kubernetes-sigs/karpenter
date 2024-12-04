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

package termination

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/eviction"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	"sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
)

type nodeDrainError struct {
	error
}

func newNodeDrainError(err error) *nodeDrainError {
	return &nodeDrainError{error: err}
}

func isNodeDrainError(err error) bool {
	if err == nil {
		return false
	}
	var nodeDrainErr *nodeDrainError
	return errors.As(err, &nodeDrainErr)
}

type DrainReconciler struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	recorder      events.Recorder
	evictionQueue *eviction.Queue
}

// nolint:gocyclo
func (d *DrainReconciler) Reconcile(ctx context.Context, n *corev1.Node, nc *v1.NodeClaim) (reconcile.Result, error) {
	if nc.StatusConditions().IsTrue(v1.ConditionTypeDrained) {
		return reconcile.Result{}, nil
	}

	tgpExpirationTime, err := nodeclaim.TerminationGracePeriodExpirationTime(nc)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to terminate node")
		return reconcile.Result{}, nil
	}
	if tgpExpirationTime != nil {
		d.recorder.Publish(NodeTerminationGracePeriodExpiringEvent(n, *tgpExpirationTime))
	}

	if err := d.drain(ctx, n, tgpExpirationTime); err != nil {
		if !isNodeDrainError(err) {
			return reconcile.Result{}, fmt.Errorf("draining node, %w", err)
		}
		// If the underlying NodeClaim no longer exists, we want to delete to avoid trying to gracefully draining
		// on nodes that are no longer alive. We do a check on the Ready condition of the node since, even
		// though the CloudProvider says the instance is not around, we know that the kubelet process is still running
		// if the Node Ready condition is true
		// Similar logic to: https://github.com/kubernetes/kubernetes/blob/3a75a8c8d9e6a1ebd98d8572132e675d4980f184/staging/src/k8s.io/cloud-provider/controllers/nodelifecycle/node_lifecycle_controller.go#L144
		if nodeutils.GetCondition(n, corev1.NodeReady).Status != corev1.ConditionTrue {
			if _, err = d.cloudProvider.Get(ctx, n.Spec.ProviderID); err != nil {
				if cloudprovider.IsNodeClaimNotFoundError(err) {
					return reconcile.Result{}, removeFinalizer(ctx, d.kubeClient, n)
				}
				return reconcile.Result{}, fmt.Errorf("getting nodeclaim, %w", err)
			}
		}
		stored := nc.DeepCopy()
		if nc.StatusConditions().SetFalse(v1.ConditionTypeDrained, "Draining", "Draining") {
			if err := d.kubeClient.Status().Patch(ctx, nc, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
				if apierrors.IsNotFound(err) {
					return reconcile.Result{}, nil
				}
				if apierrors.IsConflict(err) {
					return reconcile.Result{Requeue: true}, nil
				}
				return reconcile.Result{}, err
			}
		}
		d.recorder.Publish(NodeDrainFailedEvent(n, err))
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}

	stored := nc.DeepCopy()
	_ = nc.StatusConditions().SetTrue(v1.ConditionTypeDrained)
	if err := d.kubeClient.Status().Patch(ctx, nc, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); client.IgnoreNotFound(err) != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		if apierrors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}
	NodesDrainedTotal.Inc(map[string]string{
		metrics.NodePoolLabel: n.Labels[v1.NodePoolLabelKey],
	})
	return reconcile.Result{}, nil
}

// Drain evicts pods from the node and returns true when all pods are evicted
// https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
func (d *DrainReconciler) drain(ctx context.Context, node *corev1.Node, nodeGracePeriodExpirationTime *time.Time) error {
	pods, err := nodeutils.GetPods(ctx, d.kubeClient, node)
	if err != nil {
		return fmt.Errorf("listing pods on node, %w", err)
	}
	podsToDelete := lo.Filter(pods, func(p *corev1.Pod, _ int) bool {
		return podutils.IsWaitingEviction(p, d.clock) && !podutils.IsTerminating(p)
	})
	if err := d.DeleteExpiringPods(ctx, podsToDelete, nodeGracePeriodExpirationTime); err != nil {
		return fmt.Errorf("deleting expiring pods, %w", err)
	}
	// Monitor pods in pod groups that either haven't been evicted or are actively evicting
	podGroups := d.groupPodsByPriority(lo.Filter(pods, func(p *corev1.Pod, _ int) bool { return podutils.IsWaitingEviction(p, d.clock) }))
	for _, group := range podGroups {
		if len(group) > 0 {
			// Only add pods to the eviction queue that haven't been evicted yet
			d.evictionQueue.Add(lo.Filter(group, func(p *corev1.Pod, _ int) bool { return podutils.IsEvictable(p) })...)
			return newNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", lo.SumBy(podGroups, func(pods []*corev1.Pod) int { return len(pods) })))
		}
	}
	return nil
}

func (d *DrainReconciler) groupPodsByPriority(pods []*corev1.Pod) [][]*corev1.Pod {
	// 1. Prioritize noncritical pods, non-daemon pods https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
	var nonCriticalNonDaemon, nonCriticalDaemon, criticalNonDaemon, criticalDaemon []*corev1.Pod
	for _, pod := range pods {
		if pod.Spec.PriorityClassName == "system-cluster-critical" || pod.Spec.PriorityClassName == "system-node-critical" {
			if podutils.IsOwnedByDaemonSet(pod) {
				criticalDaemon = append(criticalDaemon, pod)
			} else {
				criticalNonDaemon = append(criticalNonDaemon, pod)
			}
		} else {
			if podutils.IsOwnedByDaemonSet(pod) {
				nonCriticalDaemon = append(nonCriticalDaemon, pod)
			} else {
				nonCriticalNonDaemon = append(nonCriticalNonDaemon, pod)
			}
		}
	}
	return [][]*corev1.Pod{nonCriticalNonDaemon, nonCriticalDaemon, criticalNonDaemon, criticalDaemon}
}

func (d *DrainReconciler) DeleteExpiringPods(ctx context.Context, pods []*corev1.Pod, nodeGracePeriodTerminationTime *time.Time) error {
	for _, pod := range pods {
		// check if the node has an expiration time and the pod needs to be deleted
		deleteTime := d.podDeleteTimeWithGracePeriod(nodeGracePeriodTerminationTime, pod)
		if deleteTime != nil && time.Now().After(*deleteTime) {
			// delete pod proactively to give as much of its terminationGracePeriodSeconds as possible for deletion
			// ensure that we clamp the maximum pod terminationGracePeriodSeconds to the node's remaining expiration time in the delete command
			gracePeriodSeconds := lo.ToPtr(int64(time.Until(*nodeGracePeriodTerminationTime).Seconds()))
			d.recorder.Publish(PodDeletedEvent(pod, gracePeriodSeconds, nodeGracePeriodTerminationTime))
			opts := &client.DeleteOptions{
				GracePeriodSeconds: gracePeriodSeconds,
			}
			if err := d.kubeClient.Delete(ctx, pod, opts); err != nil && !apierrors.IsNotFound(err) { // ignore 404, not a problem
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
func (*DrainReconciler) podDeleteTimeWithGracePeriod(nodeGracePeriodExpirationTime *time.Time, pod *corev1.Pod) *time.Time {
	if nodeGracePeriodExpirationTime == nil || pod.Spec.TerminationGracePeriodSeconds == nil { // k8s defaults to 30s, so we should never see a nil TerminationGracePeriodSeconds
		return nil
	}

	// calculate the time the pod should be deleted to allow it's full grace period for termination, equal to its terminationGracePeriodSeconds before the node's expiration time
	// eg: if a node will be force terminated in 30m, but the current pod has a grace period of 45m, we return a time of 15m ago
	deleteTime := nodeGracePeriodExpirationTime.Add(time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second * -1)
	return &deleteTime
}
