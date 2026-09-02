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

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

const (
	reconcileInterval = time.Minute
	// maxNodesPerCycle bounds the Group B/C/D nodes actually annotated per
	// reconcile. The pre-filter drops no-op nodes before the cap, so this
	// is a ceiling on nodes-that-mutate. With ~30 pods/node this bounds
	// worst-case per-cycle pod writes near the RFC's 1,500 write target.
	// Group A nodes are exempt (see capNodeRanks).
	maxNodesPerCycle = 50
)

// Controller ranks Karpenter-managed nodes by consolidation preference each
// cycle and enqueues per-pod annotation writes on the fire-and-forget Queue.
// Reconcile is serialized by the singleton reconciler adapter, so the
// per-controller fields below are written without explicit synchronization.
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
// Queue. Feature-gate enforcement is at registration (see
// pkg/controllers/controllers.go); if the gate is off this method is never
// invoked. Annotation writes are fire-and-forget: this Reconcile does not
// wait for the Queue to drain, so a stuck annotation write does not stall
// the ranking loop.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	if !c.cluster.Synced(ctx) {
		return reconciler.Result{RequeueAfter: time.Second}, nil
	}

	currentState := c.cluster.ConsolidationState()
	if c.consolidationStateUnchanged(ctx, currentState) {
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	// Best-effort snapshot: iterate the state.Cluster under its RLock and
	// accumulate pointer aliases. state.Cluster occasionally mutates
	// StateNode fields in place (e.g. clearing .Node on delete), so torn
	// reads are possible mid-cycle. That is acceptable here because
	// annotation writes are best-effort — the next reconcile picks up any
	// drift.
	var nodes []*state.StateNode
	for n := range c.cluster.Nodes() {
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return reconciler.Result{RequeueAfter: reconcileInterval}, nil
	}

	// Delegate map construction to the disruption package so the two
	// controllers share instance-type lookups and stay in lockstep on which
	// NodePools/instance types feed price + reschedule-cost math.
	nodePoolMap, nodePoolToInstanceTypesMap, err := disruption.BuildNodePoolMap(ctx, c.kubeClient, c.cloudProvider)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("building node pool map, %w", err)
	}

	nodeRanks, err := RankNodes(ctx, c.kubeClient, c.clock, nodes, nodePoolMap, nodePoolToInstanceTypesMap)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("ranking nodes, %w", err)
	}
	nodeRanks = filterNoOpNodes(nodeRanks)
	nodeRanks = capNodeRanks(nodeRanks, maxNodesPerCycle)
	nodesRanked.Set(float64(len(nodeRanks)), noLabels)

	c.enqueueAnnotationWrites(nodeRanks)

	// Advance the skip cursor only after enqueueing succeeded. If a future
	// error path is introduced above this line, this ordering preserves
	// retry-on-same-state semantics.
	c.lastConsolidationState = currentState

	if len(nodeRanks) > 0 {
		log.FromContext(ctx).V(1).WithValues("nodeCount", len(nodeRanks)).Info("enqueued pod deletion cost annotation writes")
	}
	return reconciler.Result{RequeueAfter: reconcileInterval}, nil
}

// enqueueAnnotationWrites hands each pod's desired annotation state off to
// the fire-and-forget Queue. The Queue's Reconcile method decides
// per-pod whether to write or skip and handles retry via controller-runtime.
func (c *Controller) enqueueAnnotationWrites(nodeRanks []NodeRank) {
	for i := range nodeRanks {
		nr := &nodeRanks[i]
		for _, pod := range nr.Pods {
			c.queue.Add(pod, nr.Rank, nr.HasDoNotDisrupt)
		}
	}
}

// filterNoOpNodes drops entries whose per-pod annotations already match the
// planned state. Otherwise these no-op nodes would consume slots in the
// per-cycle cap without mutating any pod. Group A nodes carry the
// math.MinInt32 sentinel and are always admitted so they annotate promptly.
func filterNoOpNodes(nodeRanks []NodeRank) []NodeRank {
	filtered := nodeRanks[:0]
	for _, nr := range nodeRanks {
		if nr.Rank == math.MinInt32 || nodeMutatesAnyPod(nr) {
			filtered = append(filtered, nr)
		}
	}
	return filtered
}

func nodeMutatesAnyPod(nr NodeRank) bool {
	if nr.HasDoNotDisrupt {
		for _, pod := range nr.Pods {
			if _, ok := pod.Annotations[corev1.PodDeletionCost]; ok {
				return true
			}
		}
		return false
	}
	value := strconv.Itoa(nr.Rank)
	for _, pod := range nr.Pods {
		if pod.Annotations[corev1.PodDeletionCost] != value {
			return true
		}
	}
	return false
}

// consolidationStateUnchanged compares currentState to the cursor advanced at
// the end of the last successful reconcile. It does not mutate the cursor —
// Reconcile advances lastConsolidationState only after enqueueing succeeds so
// a mid-reconcile error retries against the same state next cycle.
func (c *Controller) consolidationStateUnchanged(ctx context.Context, currentState time.Time) bool {
	if currentState.Equal(c.lastConsolidationState) {
		log.FromContext(ctx).V(1).Info("no changes detected, skipping pod deletion cost update")
		reconcileSkippedTotal.Add(1, noLabels)
		return true
	}
	return false
}

// capNodeRanks admits every Group A node (Rank == math.MinInt32) and caps the
// remaining groups (B/C/D) at limit. Group A nodes are already tainted for
// disruption and expected to be stable once labeled, so labeling churn stays
// bounded even when Group A exceeds limit.
func capNodeRanks(nodeRanks []NodeRank, limit int) []NodeRank {
	groupACount := 0
	for _, r := range nodeRanks {
		if r.Rank != math.MinInt32 {
			break
		}
		groupACount++
	}
	tail := nodeRanks[groupACount:]
	if len(tail) > limit {
		tail = tail[:limit]
	}
	return nodeRanks[:groupACount+len(tail)]
}
