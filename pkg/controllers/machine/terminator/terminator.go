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
	node.Labels = lo.Assign(node.Labels, map[string]string{
		v1.LabelNodeExcludeBalancers: "karpenter",
	})
	if !equality.Semantic.DeepEqual(node, stored) {
		logging.FromContext(ctx).Infof("cordoned node")
		if err := t.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return err
		}
	}
	return nil
}

// Drain evicts pods from the node and returns true when all pods are evicted
// https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
func (t *Terminator) Drain(ctx context.Context, node *v1.Node) error {
	// Get evictable pods
	pods, err := t.getPods(ctx, node)
	if err != nil {
		return fmt.Errorf("listing pods for node, %w", err)
	}
	var podsToEvict []*v1.Pod
	// Skip node due to pods that are not able to be evicted
	for _, p := range pods {
		// Ignore if unschedulable is tolerated, since they will reschedule
		if podutil.ToleratesUnschedulableTaint(p) {
			continue
		}
		// Ignore static mirror pods
		if podutil.IsOwnedByNode(p) {
			continue
		}
		podsToEvict = append(podsToEvict, p)
	}
	// Enqueue for eviction
	t.evict(podsToEvict)

	if len(podsToEvict) > 0 {
		return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", len(podsToEvict)))
	}
	return nil
}

// getPods returns a list of evictable pods for the node
func (t *Terminator) getPods(ctx context.Context, node *v1.Node) ([]*v1.Pod, error) {
	podList := &v1.PodList{}
	if err := t.kubeClient.List(ctx, podList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return nil, fmt.Errorf("listing pods on node, %w", err)
	}
	var pods []*v1.Pod
	for _, p := range podList.Items {
		// Ignore if the pod is complete and doesn't need to be evicted
		if podutil.IsTerminal(lo.ToPtr(p)) {
			continue
		}
		// Ignore if kubelet is partitioned and pods are beyond graceful termination window
		if t.isStuckTerminating(lo.ToPtr(p)) {
			continue
		}
		pods = append(pods, lo.ToPtr(p))
	}
	return pods, nil
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
		t.evictionQueue.Add(critical)
	} else {
		t.evictionQueue.Add(nonCritical)
	}
}

func (t *Terminator) isStuckTerminating(pod *v1.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}
	return t.clock.Now().After(pod.DeletionTimestamp.Time.Add(1 * time.Minute))
}
