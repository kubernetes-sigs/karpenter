/*
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

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/scheduling"
	podutil "github.com/aws/karpenter-core/pkg/utils/pod"
)

type Terminator struct {
	clock         clock.Clock
	kubeClient    client.Client
	evictionQueue *EvictionQueue
}

func NewTerminator(clk clock.Clock, kubeClient client.Client, eq *EvictionQueue) *Terminator {
	return &Terminator{
		clock:         clk,
		kubeClient:    kubeClient,
		evictionQueue: eq,
	}
}

// Cordon cordons a node
func (t *Terminator) Cordon(ctx context.Context, node *v1.Node) error {
	stored := node.DeepCopy()
	node.Spec.Unschedulable = true
	// Adding this label to the node ensures that the node is removed from the load-balancer target group
	// while it is draining and before it is terminated. This prevents 500s coming prior to health check
	// when the load balancer controller hasn't yet determined that the node and underlying connections are gone
	// https://github.com/aws/aws-node-termination-handler/issues/316
	// https://github.com/aws/karpenter/pull/2518
	node.Labels = lo.Assign(node.Labels, map[string]string{
		v1.LabelNodeExcludeBalancers: "karpenter",
	})
	if !equality.Semantic.DeepEqual(node, stored) {
		if err := t.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return err
		}
		logging.FromContext(ctx).Infof("cordoning")
	}
	return nil
}

// Drain evicts pods from the node and returns true when all pods are evicted
// https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
func (t *Terminator) Drain(ctx context.Context, node *v1.Node) error {
	// Grace period is up, we can no longer drain
	if t.clock.Now().After(node.DeletionTimestamp.Time) {
		return nil
	}

	podList := &v1.PodList{}
	if err := t.kubeClient.List(ctx, podList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return fmt.Errorf("listing pods on node, %w", err)
	}
	pods := lo.Map(podList.Items, func(pod v1.Pod, _ int) *v1.Pod { return lo.ToPtr(pod) })
	evictable := lo.Reject(pods, func(p *v1.Pod, _ int) bool {
		return podutil.IsTerminal(p) ||
			(podutil.IsTerminating(p) && t.clock.Now().After(p.DeletionTimestamp.Time.Add(1*time.Minute))) ||
			podutil.ToleratesUnschedulableTaint(p) ||
			podutil.IsOwnedByNode(p)
	})
	// We no longer have time to evict while still respecting pod
	// TerminationGracePeriodSeconds, so taint the node with NoExecute.
	if t.withinGracefulShutdownPeriod(node, pods) || len(evictable) == 0 {
		if err := t.taintNoExecute(ctx, node); err != nil {
			return fmt.Errorf("tainting no execute, %w", err)
		}
		return NewNodeDrainError(fmt.Errorf("%d pods are gracefully shutting down", len(pods)))
	}
	// Evict remaining pods
	t.evict(evictable)
	return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", len(evictable)))
}
func (t *Terminator) withinGracefulShutdownPeriod(node *v1.Node, pods []*v1.Pod) bool {
	gracePeriodSeconds := lo.Max(lo.Map(pods, func(pod *v1.Pod, _ int) int64 { return lo.FromPtr(pod.Spec.TerminationGracePeriodSeconds) }))
	gracePeriod := time.Duration(gracePeriodSeconds) * time.Second
	return t.clock.Now().Add(gracePeriod).After(node.DeletionTimestamp.Time)
}

func (t *Terminator) taintNoExecute(ctx context.Context, node *v1.Node) error {
	stored := node.DeepCopy()
	node.Spec.Taints = scheduling.Taints(node.Spec.Taints).Merge(scheduling.Taints{{
		Key:    v1beta1.TaintKeyTerminating,
		Effect: v1.TaintEffectNoExecute,
	}})
	if !equality.Semantic.DeepEqual(node, stored) {
		if err := t.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return err
		}
		logging.FromContext(ctx).Infof("tainting no execute")
	}
	return nil
}

func (t *Terminator) evict(pods []*v1.Pod) {
	// 1. Prioritize noncritical pods https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
	critical := []*v1.Pod{}
	nonCritical := []*v1.Pod{}
	for _, pod := range pods {
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		if pod.Spec.PriorityClassName == "system-cluster-critical" || pod.Spec.PriorityClassName == "system-node-critical" {
			critical = append(critical, pod)
		} else {
			nonCritical = append(nonCritical, pod)
		}
	}
	// 2. Evict critical pods if all noncritical are evicted
	if len(nonCritical) == 0 {
		t.evictionQueue.Add(critical...)
	} else {
		t.evictionQueue.Add(nonCritical...)
	}
}
