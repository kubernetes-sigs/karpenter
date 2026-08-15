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
// https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/
func (t *Terminator) Drain(ctx context.Context, node *corev1.Node, nodeGracePeriodExpirationTime *time.Time) error {
	pods, err := nodeutils.GetPods(ctx, t.kubeClient, node.Name)
	if err != nil {
		return fmt.Errorf("listing pods on node, %w", err)
	}
	waiting := lo.Filter(pods, func(p *corev1.Pod, _ int) bool { return podutil.IsWaitingEviction(p, t.clock) })

	// Split waiting pods into two disjoint sets:
	//   - deleteEligible: past the force-delete threshold. Enqueue across ALL tiers
	//     immediately — at the hard deadline we must not gate deletion behind
	//     graceful eviction of an earlier tier (a PDB on a noncritical pod would
	//     otherwise hold up a past-deadline critical pod).
	//   - gracefulCandidates: candidates for the PDB-respecting eviction API.
	//     Tier-gated so noncritical/non-daemon pods drain before critical/daemon
	//     ones (k8s graceful shutdown ordering).
	// The queue re-evaluates needsForceDelete on every reconcile, so a graceful
	// candidate whose deadline crosses while it's enqueued upgrades to force-delete
	// naturally.
	var deleteEligible, gracefulCandidates []*corev1.Pod
	for _, p := range waiting {
		if needsForceDelete(p, nodeGracePeriodExpirationTime, t.clock) {
			deleteEligible = append(deleteEligible, p)
		} else {
			gracefulCandidates = append(gracefulCandidates, p)
		}
	}
	if len(deleteEligible) > 0 {
		t.evictionQueue.Add(nodeGracePeriodExpirationTime, deleteEligible...)
	}
	podGroups := t.groupPodsByPriority(gracefulCandidates)
	for _, group := range podGroups {
		if len(group) > 0 {
			t.evictionQueue.Add(nodeGracePeriodExpirationTime, group...)
			return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", len(waiting)))
		}
	}
	// No graceful candidates remain, but we may still be waiting on the
	// delete-eligible batch to finish being removed.
	if len(deleteEligible) > 0 {
		return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", len(waiting)))
	}
	return nil
}

func (t *Terminator) groupPodsByPriority(pods []*corev1.Pod) [][]*corev1.Pod {
	// 1. Prioritize noncritical pods, non-daemon pods https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/
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
