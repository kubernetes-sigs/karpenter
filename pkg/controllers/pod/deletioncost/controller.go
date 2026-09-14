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
// Reconcile is serialized by the singleton reconciler adapter.
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

	currentState := c.cluster.ConsolidationState()
	if c.consolidationStateUnchanged(ctx, currentState) {
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

	// Delegate map construction to the disruption package so PDC and
	// consolidation share instance-type lookups.
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
	nodesRanked.Reset()
	total := 0
	for np, count := range perNodePool {
		nodesRanked.Set(float64(count), map[string]string{metrics.NodePoolLabel: np})
		total += count
	}

	// Advance the skip cursor only after enqueueing succeeded.
	c.lastConsolidationState = currentState

	if total > 0 {
		log.FromContext(ctx).V(1).WithValues("nodeCount", total).Info("enqueued pod deletion cost annotation writes")
	}
	return reconciler.Result{RequeueAfter: reconcileInterval}, nil
}

// enqueueAnnotationWrites walks the ranked groups and pushes per-pod
// annotation writes onto the Queue. Ranks are derived from position within
// groupBC via RankForBC. Group A is uncapped; Groups B/C/D share a per-
// cycle cap of maxNodesPerCycle. Nodes whose pods already carry the planned
// annotation state are skipped so they don't consume the cap. Pods are
// read from the informer cache on demand rather than cached in a struct.
// Returns per-nodepool counts of nodes annotated (drives the nodes_ranked
// gauge).
func (c *Controller) enqueueAnnotationWrites(ctx context.Context, groupA, groupBC, groupD []*state.StateNode) map[string]int {
	perNodePool := map[string]int{}
	c.enqueueGroupA(ctx, groupA, perNodePool)
	written := c.enqueueRankedBC(ctx, groupBC, maxNodesPerCycle, perNodePool)
	c.enqueueCleanup(ctx, groupD, maxNodesPerCycle-written, perNodePool)
	return perNodePool
}

// enqueueGroupA writes the math.MinInt32 sentinel to every non-no-op Group
// A node. Group A is uncapped; disrupted-tainted or marked-for-deletion
// nodes always annotate promptly.
func (c *Controller) enqueueGroupA(ctx context.Context, nodes []*state.StateNode, perNodePool map[string]int) {
	for _, node := range nodes {
		pods, _ := node.Pods(ctx, c.kubeClient)
		if !nodeMutatesAnyPod(pods, math.MinInt32, false) {
			continue
		}
		for _, pod := range pods {
			c.queue.Add(pod, math.MinInt32, false)
		}
		perNodePool[node.Labels()[v1.NodePoolLabelKey]]++
	}
}

// enqueueRankedBC writes sequential ranks to Groups B and C, stopping when
// budget non-no-op nodes have been enqueued. Rank per position is
// RankForBC(i, len(nodes)).
func (c *Controller) enqueueRankedBC(ctx context.Context, nodes []*state.StateNode, budget int, perNodePool map[string]int) int {
	if budget <= 0 {
		return 0
	}
	n := len(nodes)
	count := 0
	for i, node := range nodes {
		if count >= budget {
			break
		}
		rank := RankForBC(i, n)
		pods, _ := node.Pods(ctx, c.kubeClient)
		if !nodeMutatesAnyPod(pods, rank, false) {
			continue
		}
		for _, pod := range pods {
			c.queue.Add(pod, rank, false)
		}
		perNodePool[node.Labels()[v1.NodePoolLabelKey]]++
		count++
	}
	return count
}

// enqueueCleanup clears the pod-deletion-cost annotation on Group D nodes,
// stopping when budget non-no-op nodes have been enqueued.
func (c *Controller) enqueueCleanup(ctx context.Context, nodes []*state.StateNode, budget int, perNodePool map[string]int) int {
	if budget <= 0 {
		return 0
	}
	count := 0
	for _, node := range nodes {
		if count >= budget {
			break
		}
		pods, _ := node.Pods(ctx, c.kubeClient)
		if !nodeMutatesAnyPod(pods, 0, true) {
			continue
		}
		for _, pod := range pods {
			c.queue.Add(pod, 0, true)
		}
		perNodePool[node.Labels()[v1.NodePoolLabelKey]]++
		count++
	}
	return count
}

// nodeMutatesAnyPod reports whether at least one pod on the node would see
// its pod-deletion-cost annotation change. cleanup=true means "clear if
// present"; cleanup=false means "match rank".
func nodeMutatesAnyPod(pods []*corev1.Pod, rank int, cleanup bool) bool {
	if cleanup {
		for _, pod := range pods {
			if _, ok := pod.Annotations[corev1.PodDeletionCost]; ok {
				return true
			}
		}
		return false
	}
	value := strconv.Itoa(rank)
	for _, pod := range pods {
		if pod.Annotations[corev1.PodDeletionCost] != value {
			return true
		}
	}
	return false
}

// consolidationStateUnchanged compares currentState to the cursor advanced
// at the end of the last successful reconcile. It does not mutate the
// cursor; Reconcile advances lastConsolidationState only after enqueueing
// succeeds so a mid-reconcile error retries against the same state.
func (c *Controller) consolidationStateUnchanged(ctx context.Context, currentState time.Time) bool {
	if currentState.Equal(c.lastConsolidationState) {
		log.FromContext(ctx).V(1).Info("no changes detected, skipping pod deletion cost update")
		return true
	}
	return false
}
