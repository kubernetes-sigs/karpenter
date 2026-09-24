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

package deletioncost

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

const (
	reconcileInterval = time.Minute
	// maxNodesPerCycle bounds the Groups B/C/D nodes actually annotated per
	// reconcile. Group A nodes are exempt. With ~30 pods/node this bounds
	// worst-case per-cycle pod writes near the RFC's 1,500 write target.
	maxNodesPerCycle = 50
)

// Controller ranks Karpenter-managed nodes by consolidation preference each
// cycle and enqueues per-pod annotation writes on the fire-and-forget Queue.
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cluster       *state.Cluster
	queue         *Queue

	lastConsolidationState time.Time
}

func NewController(
	clk clock.Clock,
	kubeClient client.Client,
	cloudProvider cloudprovider.CloudProvider,
	cluster *state.Cluster,
	queue *Queue,
) *Controller {
	return &Controller{
		clock:         clk,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		cluster:       cluster,
		queue:         queue,
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

func (c *Controller) Name() string {
	return "pod.deletioncost"
}

// Reconcile ranks the cluster's nodes and enqueues annotation writes on the
// Queue. Annotation writes are fire-and-forget: this Reconcile does not wait
// for the Queue to drain.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	if !c.cluster.Synced(ctx) {
		return reconciler.Result{RequeueAfter: time.Second}, nil
	}

	// Advance the cursor only after enqueueing succeeds so a mid-reconcile
	// error retries against the same state (see the assignment near the end
	// of this method).
	currentState := c.cluster.ConsolidationState()
	if currentState.Equal(c.lastConsolidationState) {
		log.FromContext(ctx).V(1).Info("no changes detected, skipping pod deletion cost update")
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	// Best-effort snapshot of state.Cluster: pointer aliases only. Torn
	// reads are acceptable because annotation writes are best-effort and
	// the next reconcile picks up any drift.
	var nodes []*state.StateNode
	for n := range c.cluster.Nodes() {
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	// Shared with consolidation so both resolve instance types identically.
	nodePoolMap, nodePoolToInstanceTypesMap, err := disruption.BuildNodePoolMap(ctx, c.kubeClient, c.cloudProvider)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("building node pool map, %w", err)
	}

	groupA, groupBC, groupD, err := RankNodes(ctx, c.kubeClient, c.clock, nodes, nodePoolMap, nodePoolToInstanceTypesMap)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("ranking nodes, %w", err)
	}
	perNodePool := c.enqueueAnnotationWrites(ctx, groupA, groupBC, groupD)

	// Reset before Set so pools whose count fell to zero don't linger.
	nodesWithPendingAnnotationWrites.Reset()
	total := 0
	for np, count := range perNodePool {
		nodesWithPendingAnnotationWrites.Set(float64(count), map[string]string{metrics.NodePoolLabel: np})
		total += count
	}

	c.lastConsolidationState = currentState

	if total > 0 {
		log.FromContext(ctx).V(1).WithValues("nodeCount", total).Info("enqueued pod deletion cost annotation writes")
	}
	return reconciler.Result{RequeueAfter: reconcileInterval}, nil
}

// enqueueAnnotationWrites pushes per-pod annotation writes onto the Queue.
// Group A is exempt from the per-cycle cap so disrupted-tainted and
// marked-for-deletion nodes always annotate promptly. Nodes whose pods
// already carry the planned state are skipped and do not consume the cap.
// Returns per-nodepool counts of nodes annotated.
func (c *Controller) enqueueAnnotationWrites(ctx context.Context, groupA, groupBC, groupD []*state.StateNode) map[string]int {
	perNodePool := map[string]int{}
	for _, node := range groupA {
		c.tryEnqueueNode(ctx, node, math.MinInt32, false, perNodePool)
	}
	n := len(groupBC)
	written := c.enqueueCapped(ctx, groupBC, maxNodesPerCycle, perNodePool, func(i int) (int, bool) {
		return RankForBC(i, n), false
	})
	c.enqueueCapped(ctx, groupD, maxNodesPerCycle-written, perNodePool, func(_ int) (int, bool) {
		return 0, true
	})
	return perNodePool
}

func (c *Controller) tryEnqueueNode(ctx context.Context, node *state.StateNode, rank int, cleanup bool, perNodePool map[string]int) bool {
	pods, _ := node.Pods(ctx, c.kubeClient)
	if !nodeMutatesAnyPod(pods, rank, cleanup) {
		return false
	}
	for _, pod := range pods {
		c.queue.Add(pod, rank, cleanup)
	}
	perNodePool[node.Labels()[v1.NodePoolLabelKey]]++
	return true
}

// enqueueCapped spends budget on nodes that actually mutate a pod; no-op
// nodes do not consume a slot.
func (c *Controller) enqueueCapped(ctx context.Context, nodes []*state.StateNode, budget int, perNodePool map[string]int, rankAt func(i int) (int, bool)) int {
	if budget <= 0 {
		return 0
	}
	count := 0
	for i, node := range nodes {
		if count >= budget {
			break
		}
		rank, cleanup := rankAt(i)
		if c.tryEnqueueNode(ctx, node, rank, cleanup, perNodePool) {
			count++
		}
	}
	return count
}

func nodeMutatesAnyPod(pods []*corev1.Pod, rank int, cleanup bool) bool {
	for _, pod := range pods {
		if !podHasDesiredAnnotation(pod, rank, cleanup) {
			return true
		}
	}
	return false
}

// podHasDesiredAnnotation reports whether the pod already carries the
// intended pod-deletion-cost state. Shared with Queue.matchesDesired so the
// controller's no-op guard and the queue's idempotency short-circuit read the
// same rule.
func podHasDesiredAnnotation(pod *corev1.Pod, rank int, cleanup bool) bool {
	if cleanup {
		_, has := pod.Annotations[corev1.PodDeletionCost]
		return !has
	}
	return pod.Annotations[corev1.PodDeletionCost] == strconv.Itoa(rank)
}
