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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/events"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	podutil "sigs.k8s.io/karpenter/pkg/utils/pod"
)

type Terminator struct {
	clock         clock.Clock
	kubeClient    client.Client
	evictionQueue *Queue
	recorder      events.Recorder
}

func NewTerminator(clk clock.Clock, kubeClient client.Client, eq *Queue, recorder events.Recorder) *Terminator {
	return &Terminator{
		clock:         clk,
		kubeClient:    kubeClient,
		evictionQueue: eq,
		recorder:      recorder,
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
	pods, err := nodeutils.GetPods(ctx, t.kubeClient, node)
	if err != nil {
		return fmt.Errorf("listing pods on node, %w", err)
	}
	// Pods whose terminationGracePeriodSeconds would extend past the node's
	// terminationGracePeriod are routed to the eviction queue's force-delete
	// path so they get as much of their grace period as possible before the
	// node is forcefully terminated. This bypasses PDBs and the
	// do-not-disrupt annotation.
	t.evictionQueue.AddForceDelete(nodeGracePeriodExpirationTime, lo.Filter(pods, func(p *corev1.Pod, _ int) bool {
		return podutil.IsWaitingEviction(p, t.clock) &&
			(!podutil.IsTerminating(p) || podutil.IsPodEligibleForForcedEviction(p, nodeGracePeriodExpirationTime)) &&
			t.shouldForceDeleteNow(p, nodeGracePeriodExpirationTime)
	})...)
	// Monitor pods in pod groups that either haven't been evicted or are actively evicting
	podGroups := t.groupPodsByPriority(lo.Filter(pods, func(p *corev1.Pod, _ int) bool { return podutil.IsWaitingEviction(p, t.clock) }))
	for _, group := range podGroups {
		if len(group) > 0 {
			// Only add pods to the eviction queue that haven't been evicted yet. Pods already enqueued
			// for force-delete are skipped by Add, so this won't downgrade them to a PDB-respecting eviction.
			t.evictionQueue.Add(lo.Filter(group, func(p *corev1.Pod, _ int) bool { return podutil.IsEvictable(p, t.clock, t.recorder) })...)
			return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", lo.SumBy(podGroups, func(pods []*corev1.Pod) int { return len(pods) })))
		}
	}
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

// shouldForceDeleteNow reports whether the pod's terminationGracePeriodSeconds
// would extend past the node's grace period expiration if eviction is allowed
// to run its course, meaning the pod must be force-deleted now to give it as
// much of its requested grace period as possible.
func (t *Terminator) shouldForceDeleteNow(pod *corev1.Pod, nodeGracePeriodExpirationTime *time.Time) bool {
	// k8s defaults TerminationGracePeriodSeconds to 30s, so we should never see a nil value.
	if nodeGracePeriodExpirationTime == nil || pod.Spec.TerminationGracePeriodSeconds == nil {
		return false
	}
	deleteTime := nodeGracePeriodExpirationTime.Add(time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second * -1)
	return t.clock.Now().After(deleteTime)
}
