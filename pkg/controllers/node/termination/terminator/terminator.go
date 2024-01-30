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

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodeutil "sigs.k8s.io/karpenter/pkg/utils/node"

	podutil "sigs.k8s.io/karpenter/pkg/utils/pod"
)

type Terminator struct {
	clock         clock.Clock
	kubeClient    client.Client
	evictionQueue *Queue
}

func NewTerminator(clk clock.Clock, kubeClient client.Client, eq *Queue) *Terminator {
	return &Terminator{
		clock:         clk,
		kubeClient:    kubeClient,
		evictionQueue: eq,
	}
}

// Taint idempotently adds an arbitrary taint to a node with a NodeClaim
func (t *Terminator) Taint(ctx context.Context, node *v1.Node, taint v1.Taint) error {
	stored := node.DeepCopy()
	// If the node already has the correct taint (key, value, and effect), do nothing.
	if _, ok := lo.Find(node.Spec.Taints, func(t v1.Taint) bool {
		return t.MatchTaint(&taint) && t.Value == taint.Value && t.Effect == taint.Effect
	}); !ok {
		// Otherwise, if the taint key exists (but with a different value or effect), remove it.
		node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t v1.Taint, _ int) bool {
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
		v1.LabelNodeExcludeBalancers: "karpenter",
	})
	if !equality.Semantic.DeepEqual(node, stored) {
		if err := t.kubeClient.Patch(ctx, node, client.StrategicMergeFrom(stored)); err != nil {
			return err
		}
		logging.FromContext(ctx).With("taint.Key", taint.Key).With("taint.Effect", taint.Effect).With("taint.Value", taint.Value).Infof("tainted node")
	}
	return nil
}

// Drain evicts pods from the node and returns true when all pods are evicted
// https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
func (t *Terminator) Drain(ctx context.Context, node *v1.Node, nodeGracePeriodExpirationTime *time.Time) error {
	pods, err := nodeutil.GetPods(ctx, t.kubeClient, node)
	if err != nil {
		return fmt.Errorf("listing pods on node, %w", err)
	}
	t.EvictOrDelete(pods, nodeGracePeriodExpirationTime)

	// podsWaitingEvictionCount are the number of pods that either haven't had eviction called against them yet
	// or are still actively terminated and haven't exceeded their termination grace period yet
	podsWaitingEvictionCount := lo.CountBy(pods, func(p *v1.Pod) bool { return podutil.IsWaitingEviction(p, t.clock) })
	if podsWaitingEvictionCount > 0 {
		return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", len(pods)))
	}
	return nil
}

func (t *Terminator) EvictOrDelete(pods []*v1.Pod, nodeGracePeriodExpirationTime *time.Time) {
	// 1. Prioritize noncritical pods, non-daemon pods https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
	var criticalNonDaemon, criticalDaemon, nonCriticalNonDaemon, nonCriticalDaemon []*v1.Pod
	for _, pod := range pods {
		// check if the node has an expiration time and the pod needs to be deleted
		deleteTime := t.shouldDeletePodToEnsureGracePeriod(nodeGracePeriodExpirationTime, pod)
		if deleteTime != nil {
			t.evictionQueue.Add(deleteTime, pod)
			continue
		}

		// evictablePods are pods that aren't yet terminating are eligible to have the eviction API called against them
		if !podutil.IsEvictable(pod) {
			continue // skip queueing this pod for eviction
		}

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
	// 2. Evict in order:
	// a. non-critical non-daemonsets
	// b. non-critical daemonsets
	// c. critical non-daemonsets
	// d. critical daemonsets
	t.EvictInOrder([][]*v1.Pod{
		nonCriticalNonDaemon,
		nonCriticalDaemon,
		criticalNonDaemon,
		criticalDaemon,
	})
}

func (t *Terminator) EvictInOrder(pods [][]*v1.Pod) {
	for _, podList := range pods {
		if len(podList) > 0 {
			// evict the first list of pods that is not empty, ignore the rest
			t.evictionQueue.Add(nil, podList...)
			return
		}
	}
}

// if a pod should be deleted to give it the full terminationGracePeriodSeconds of time before the node will shut down, return the time the pod should be deleted
func (t *Terminator) shouldDeletePodToEnsureGracePeriod(nodeGracePeriodExpirationTime *time.Time, pod *v1.Pod) *time.Time {
	if nodeGracePeriodExpirationTime == nil || pod.Spec.TerminationGracePeriodSeconds == nil {
		return nil
	}

	// check if the pod would be force terminated before being allowed its full terminationGracePeriodSeconds in time to gracefully terminate
	// eg: if a node will be force terminated in 30m, but the current pod has a grace period of 45m, we return a time of 15m ago
	deleteTime := nodeGracePeriodExpirationTime.Add(time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second * -1)
	return &deleteTime
}
