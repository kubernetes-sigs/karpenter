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

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
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

// Taint idempotently adds the karpenter.sh/disruption taint to a node with a NodeClaim
func (t *Terminator) Taint(ctx context.Context, node *v1.Node) error {
	stored := node.DeepCopy()
	// If the taint already has the karpenter.sh/disruption=disrupting:NoSchedule taint, do nothing.
	if _, ok := lo.Find(node.Spec.Taints, func(t v1.Taint) bool {
		return v1beta1.IsDisruptingTaint(t)
	}); !ok {
		// If the taint key exists (but with a different value or effect), remove it.
		node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t v1.Taint, _ int) bool {
			return t.Key == v1beta1.DisruptionTaintKey
		})
		node.Spec.Taints = append(node.Spec.Taints, v1beta1.DisruptionNoScheduleTaint)
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
		if err := t.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return err
		}
		logging.FromContext(ctx).Infof("tainted node")
	}
	return nil
}

// Drain evicts pods from the node and returns true when all pods are evicted
// https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
func (t *Terminator) Drain(ctx context.Context, node *v1.Node) error {
	// Get evictable pods
	pods := &v1.PodList{}
	if err := t.kubeClient.List(ctx, pods, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return fmt.Errorf("listing pods on node, %w", err)
	}

	// Skip node due to pods that are not able to be evicted
	podsToEvict := lo.FilterMap(pods.Items, func(po v1.Pod, _ int) (*v1.Pod, bool) {
		p := lo.ToPtr(po)
		// Ignore pods that tolerate the karpenter.sh/disruption taint.
		if podutil.ToleratesDisruptionNoScheduleTaint(p) ||
			// Ignore static mirror pods
			podutil.IsOwnedByNode(p) ||
			// Ignore if the pod is complete and doesn't need to be evicted
			podutil.IsTerminal(p) ||
			// Ignore if kubelet is partitioned and pods are beyond graceful termination window
			t.isStuckTerminating(p) {
			return nil, false
		}
		return p, true
	})
	// Enqueue for eviction
	t.Evict(podsToEvict)

	if len(podsToEvict) > 0 {
		return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", len(podsToEvict)))
	}
	return nil
}

func (t *Terminator) Evict(pods []*v1.Pod) {
	// 1. Prioritize noncritical pods, non-daemon pods https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
	var criticalNonDaemon, criticalDaemon, nonCriticalNonDaemon, nonCriticalDaemon []*v1.Pod
	for _, pod := range pods {
		if !pod.DeletionTimestamp.IsZero() {
			continue
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
	if len(nonCriticalNonDaemon) != 0 {
		t.evictionQueue.Add(nonCriticalNonDaemon...)
	} else if len(nonCriticalDaemon) != 0 {
		t.evictionQueue.Add(nonCriticalDaemon...)
	} else if len(criticalNonDaemon) != 0 {
		t.evictionQueue.Add(criticalNonDaemon...)
	} else if len(criticalDaemon) != 0 {
		t.evictionQueue.Add(criticalDaemon...)
	}
}

func (t *Terminator) isStuckTerminating(pod *v1.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}
	return t.clock.Now().After(pod.DeletionTimestamp.Time.Add(1 * time.Minute))
}
