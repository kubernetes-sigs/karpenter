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

package nodepool

import (
	"context"
	"fmt"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

// missingAfterFailedUpdate is in the help of every repair breaker metric.
const missingAfterFailedUpdate = "The series is missing before the first update of these metrics and after a failed one."

// registeringNodesNotCounted is in the help of the repair breaker metrics that report a trip or how long it has lasted.
const registeringNodesNotCounted = "Without --legacy-node-repair, some nodes that are still registering count for node repair but not here, so repair can be stopped while this shows it isn't, or the other way around."

// Each NodePool gauge has a series for every managed NodePool, so a missing series means the NodePool isn't managed or
// no longer exists, or the last update failed or hasn't run yet.
var (
	RepairBreakerTripped = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "repair_breaker_tripped",
			Help:      "Returns 1 if the nodepool's repair breaker is tripped, because more than " + health.UnhealthyThreshold + " of its nodes, rounded up, are unhealthy, and 0 otherwise. Nodes count as unhealthy even before their repair toleration passes. " + registeringNodesNotCounted + " " + missingAfterFailedUpdate + " Labeled by nodepool.",
		},
		[]opmetrics.Label{metrics.NodePool},
		opmetrics.Alpha,
	)
	RepairBreakerTrippedTimeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "repair_breaker_tripped_time_seconds",
			Help:      "How long the nodepool's repair breaker has been tripped, because more than " + health.UnhealthyThreshold + " of its nodes, rounded up, are unhealthy, or 0 if it isn't tripped. If the breaker was already tripped when Karpenter restarted or changed leader, the time counts from the first update after that. " + registeringNodesNotCounted + " " + missingAfterFailedUpdate + " Labeled by nodepool.",
		},
		[]opmetrics.Label{metrics.NodePool},
		opmetrics.Alpha,
	)
	RepairBreakerTrippedNodesWithUnhealthyCondition = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "repair_breaker_tripped_nodes_with_unhealthy_condition",
			Help:      "The number of nodes in the nodepool that have a condition a repair policy treats as unhealthy, counted while karpenter_nodepools_repair_breaker_tripped is 1, and 0 otherwise. A node counts whether or not that condition has lasted past the policy's toleration. With --legacy-node-repair, only policies that replace nodes count, and only when their reason pattern matches. Without --legacy-node-repair, node repair also counts some unhealthy nodes that are still registering, but this doesn't. " + missingAfterFailedUpdate + " Labeled by nodepool.",
		},
		[]opmetrics.Label{metrics.NodePool},
		opmetrics.Alpha,
	)
	RepairBreakerNodes = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "repair_breaker_nodes",
			Help:      "The number of nodes in the nodepool that these breaker metrics count. Without --legacy-node-repair, node repair also counts some nodes that are still registering, but this doesn't. " + missingAfterFailedUpdate + " Labeled by nodepool.",
		},
		[]opmetrics.Label{metrics.NodePool},
		opmetrics.Alpha,
	)
	// RepairBreakerLongestTrippedTimeSeconds gives one value per cluster, so alerts across many clusters can count
	// clusters rather than NodePools.
	RepairBreakerLongestTrippedTimeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: "cluster",
			Name:      "repair_breaker_longest_tripped_time_seconds",
			Help:      "The largest karpenter_nodepools_repair_breaker_tripped_time_seconds across nodepools, or 0 if no nodepool's repair breaker is tripped. With --legacy-node-repair, it doesn't cover nodes with no nodepool, which legacy repair checks against the whole cluster, or nodes whose nodepool no longer exists. " + registeringNodesNotCounted + " " + missingAfterFailedUpdate,
		},
		[]opmetrics.Label{},
		opmetrics.Alpha,
	)
)

// Breakers checks each NodePool's breaker for the breaker metrics, using node repair's rule.
type Breakers interface {
	// NodePoolBreakers returns the breaker of each NodePool that has nodes.
	NodePoolBreakers(ctx context.Context) (map[string]health.Breaker, error)
	// NodePoolBreakerTrippedAt dates the NodePool's trip from node data, or returns zero if it isn't tripped. It reads
	// the nodes again, so it can disagree with an earlier NodePoolBreakers.
	NodePoolBreakerTrippedAt(ctx context.Context, nodePool string) (time.Time, error)
}

// clusterBreakers checks the breakers from cluster state. Unlike the repair method, which lists Nodes, it leaves out
// some Nodes that are still registering.
type clusterBreakers struct {
	cluster *state.Cluster
}

func (b clusterBreakers) NodePoolBreakers(context.Context) (map[string]health.Breaker, error) {
	return b.cluster.RepairBreakers(), nil
}

func (b clusterBreakers) NodePoolBreakerTrippedAt(_ context.Context, nodePool string) (time.Time, error) {
	return b.cluster.RepairBreakerTrippedAt(nodePool), nil
}

// RepairController sets the repair breaker metrics on a timer, since repair only checks its breaker in passes that have
// nodes to repair.
type RepairController struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	breakers      Breakers
	// cluster is set when the breakers come from cluster state, so Reconcile can wait for it to sync.
	cluster     *state.Cluster
	metricStore *metrics.Store
	// trips is what the last successful update saw for each managed NodePool. It starts empty after a restart.
	trips map[string]nodePoolTrip
}

// nodePoolTrip is what the controller remembers about one NodePool between updates.
type nodePoolTrip struct {
	// trippedSince is when the breaker tripped, kept while the NodePool stays tripped, and zero while it isn't.
	trippedSince time.Time
	// untrippedAt is when the last update saw the NodePool untripped, and zero while it is tripped.
	untrippedAt time.Time
}

// trippedFor returns how long the breaker has been tripped at now, or 0 if it isn't tripped.
func (t nodePoolTrip) trippedFor(now time.Time) time.Duration {
	if t.trippedSince.IsZero() {
		return 0
	}
	return now.Sub(t.trippedSince)
}

// NewRepairController constructs a controller that checks the breakers from cluster state. It panics when cluster state
// has no repair policy matcher, since then no node would count as unhealthy.
func NewRepairController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster) *RepairController {
	if cluster.RepairPolicyMatcher() == nil {
		panic("repair breaker metrics require cluster state built with a repair policy matcher")
	}
	return newRepairController(clk, kubeClient, cloudProvider, clusterBreakers{cluster: cluster}, cluster)
}

// NewLegacyRepairController constructs a controller that reads the breakers from the legacy repair controller, which
// runs with --legacy-node-repair.
func NewLegacyRepairController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, legacy Breakers) *RepairController {
	return newRepairController(clk, kubeClient, cloudProvider, legacy, nil)
}

func newRepairController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, breakers Breakers, cluster *state.Cluster) *RepairController {
	return &RepairController{
		clock:         clk,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		breakers:      breakers,
		cluster:       cluster,
		metricStore:   metrics.NewStore(),
		trips:         map[string]nodePoolTrip{},
	}
}

func (c *RepairController) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	// Cluster state may be missing Nodes until another controller, such as disruption, first marks it synced, so its
	// counts could show a trip that isn't there, or miss one that is.
	if c.cluster != nil && !c.cluster.HasSynced() {
		return reconciler.Result{RequeueAfter: time.Second}, nil
	}
	if err := c.update(ctx); err != nil {
		// A failed update sets no values, so remove every series rather than keep old ones. Trip starts stay, so one
		// failed update doesn't restart every trip.
		c.metricStore.ReplaceAll(nil)
		return reconciler.Result{}, err
	}
	return reconciler.Result{RequeueAfter: 10 * time.Second}, nil
}

// update checks the breaker of every managed NodePool and sets the metrics to what it finds.
func (c *RepairController) update(ctx context.Context) error {
	nodePools, err := nodepoolutils.ListManaged(ctx, c.kubeClient, c.cloudProvider, client.UnsafeDisableDeepCopy)
	if err != nil {
		return fmt.Errorf("listing nodepools, %w", err)
	}
	breakers, err := c.breakers.NodePoolBreakers(ctx)
	if err != nil {
		return err
	}
	// Trip starts from node data are wall clock times, so drop the monotonic reading to time every trip on one clock.
	now := c.clock.Now().UTC()
	storeMetrics := map[string][]*metrics.StoreMetric{}
	trips := map[string]nodePoolTrip{}
	var longest time.Duration
	for _, np := range nodePools {
		breaker := breakers[np.Name]
		trip, err := c.trip(ctx, np.Name, breaker, now)
		if err != nil {
			return err
		}
		trips[np.Name] = trip
		trippedFor := trip.trippedFor(now)
		storeMetrics[client.ObjectKeyFromObject(np).String()] = buildRepairBreakerMetrics(np.Name, breaker, trippedFor)
		longest = max(longest, trippedFor)
	}
	storeMetrics["clusterLongestTrip"] = []*metrics.StoreMetric{{GaugeMetric: RepairBreakerLongestTrippedTimeSeconds, Value: longest.Seconds()}}
	// Keeping only the NodePools this update saw forgets the ones that are gone, and ReplaceAll deletes their series.
	c.trips = trips
	c.metricStore.ReplaceAll(storeMetrics)
	return nil
}

// trip returns what to remember about the NodePool. It works out when a trip started only if no start is kept.
func (c *RepairController) trip(ctx context.Context, nodePool string, breaker health.Breaker, now time.Time) (nodePoolTrip, error) {
	if !breaker.Tripped {
		return nodePoolTrip{untrippedAt: now}, nil
	}
	previous, seen := c.trips[nodePool]
	// A condition's transition time resets whenever its status changes, so keep the start the trip began with.
	start := previous.trippedSince
	if start.IsZero() && !seen {
		// No update has seen the NodePool untripped, as after a restart or when it's created again under the same name.
		// Node data could date this trip far too early, so it starts now.
		start = now
	}
	if start.IsZero() {
		trippedAt, err := c.breakers.NodePoolBreakerTrippedAt(ctx, nodePool)
		if err != nil {
			return nodePoolTrip{}, err
		}
		// The nodes may have changed since they were counted. If they no longer show a trip, date it to now.
		if trippedAt.IsZero() {
			trippedAt = now
		}
		// Node data can date a trip far too early, for example when the NodePool tripped because it shrank. So the trip
		// starts no earlier than the last update that saw the NodePool untripped.
		start = lo.Latest(trippedAt, previous.untrippedAt)
	}
	// A node's clock can run ahead of the controller's, and the clock can step back, so never keep a start later than now.
	return nodePoolTrip{trippedSince: lo.Earliest(start, now)}, nil
}

func buildRepairBreakerMetrics(nodePool string, breaker health.Breaker, trippedFor time.Duration) []*metrics.StoreMetric {
	labels := map[string]string{metrics.NodePool.Name: nodePool}
	return []*metrics.StoreMetric{
		{GaugeMetric: RepairBreakerTripped, Value: lo.Ternary(breaker.Tripped, 1.0, 0.0), Labels: labels},
		{GaugeMetric: RepairBreakerTrippedTimeSeconds, Value: trippedFor.Seconds(), Labels: labels},
		{GaugeMetric: RepairBreakerTrippedNodesWithUnhealthyCondition, Value: float64(lo.Ternary(breaker.Tripped, breaker.Unhealthy, 0)), Labels: labels},
		{GaugeMetric: RepairBreakerNodes, Value: float64(breaker.Nodes), Labels: labels},
	}
}

func (c *RepairController) Name() string {
	return "metrics.nodepool.repair"
}

func (c *RepairController) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		// A failed update leaves no series, so retry it within 10 seconds, as the timer does, rather than back off for up
		// to 1000 seconds.
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](100*time.Millisecond, 10*time.Second),
		}).
		Complete(singleton.AsReconciler(c))
}
