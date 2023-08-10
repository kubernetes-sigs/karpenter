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
		logging.FromContext(ctx).Infof("cordoned")
	}
	return nil
}

// Drain evicts pods from the node and returns true when all pods are evicted
// https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
func (t *Terminator) Drain(ctx context.Context, node *v1.Node) error {
	podList := &v1.PodList{}
	if err := t.kubeClient.List(ctx, podList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return fmt.Errorf("listing pods on node, %w", err)
	}

	pods := lo.Map(podList.Items, func(pod v1.Pod, _ int) *v1.Pod { return lo.ToPtr(pod) })
	pods = lo.Reject(pods, func(p *v1.Pod, _ int) bool { return podutil.IsNotDrainable(p) }) // Ignore pods that can't be drained
	pods = lo.Reject(pods, func(p *v1.Pod, _ int) bool { return t.isStuckTerminating(p) })   // Ignore pods that are stuck terminating

	// Successfully Drained
	if len(pods) == 0 {
		return nil
	}
	// Find any evictable pods, and evict them
	if evictable := lo.Reject(pods, func(p *v1.Pod, _ int) bool { return podutil.IsNotEvictable(p) }); len(evictable) > 0 {
		t.evict(evictable)
		return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", len(evictable)))
	}

	// Taint NoExecute to trigger graceful deletion for remaining pods
	if err := t.Taint(ctx, node, v1beta1.TaintShuttingDown); err != nil {
		return fmt.Errorf("tainting node %s, %w", v1beta1.TaintShuttingDown.ToString(), err)
	}
	return NewNodeDrainError(fmt.Errorf("%d pods are gracefully shutting down", len(pods)))
}

func (t *Terminator) Taint(ctx context.Context, node *v1.Node, taint v1.Taint) error {
	mutated := node.DeepCopy()
	mutated.Spec.Taints = scheduling.Taints(node.Spec.Taints).Merge(scheduling.Taints{taint})
	if !equality.Semantic.DeepEqual(mutated, node) {
		if err := t.kubeClient.Patch(ctx, mutated, client.MergeFrom(node)); err != nil {
			return client.IgnoreNotFound(err)
		}
		logging.FromContext(ctx).Infof("tainted %s", taint.ToString())
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

func (t *Terminator) isStuckTerminating(pod *v1.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}
	return t.clock.Now().After(pod.DeletionTimestamp.Time.Add(1 * time.Minute))
}
