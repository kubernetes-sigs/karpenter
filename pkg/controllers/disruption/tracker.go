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

package disruption

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/multierr"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	cache "github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"k8s.io/utils/clock"

	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const decisionTimeout = time.Hour

var (
	// Change ratios are calculated with the following formula:
	//   Value at Beginning / Value at End

	// ClusterCostChangeRatio tracks the ratio of cluster cost before and after a decision
	ClusterCostChangeRatio opmetrics.ObservationMetric

	// ResourceRequestsChangeRatio tracks the ratio of total desired resource requests before and after a decision
	ResourceRequestsChangeRatio opmetrics.ObservationMetric

	// NodeCountChangeRatio tracks the ratio of total node count before and after a decision
	NodeCountChangeRatio opmetrics.ObservationMetric

	// DesiredPodCountChangeRatio tracks the ratio of total desired pod count before and after a decision
	DesiredPodCountChangeRatio opmetrics.ObservationMetric

	// DecisionTrackerErrors tracks the number of internal errors in the decision tracker
	DecisionTrackerErrors opmetrics.CounterMetric

	// DecisionCacheExpirations tracks the number of internal errors in the decision tracker
	DecisionCacheExpirations opmetrics.CounterMetric

	DecisionBucketDimensions []string
)

func initializeMetrics() {
	// Change ratios are calculated with the following formula:
	//   Value at Beginning / Value at End

	// ClusterCostChangeRatio tracks the ratio of cluster cost before and after a decision
	ClusterCostChangeRatio = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "cluster_cost_change_ratio",
			Help:      "ALPHA METRIC. Ratio of cluster cost at decision start to cluster cost at decision end. Labeled by decision dimensions.",
		},
		generateDecisionDimensions(),
	)

	// ResourceRequestsChangeRatio tracks the ratio of total desired resource requests before and after a decision
	ResourceRequestsChangeRatio = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "resource_requests_change_ratio",
			Help:      "ALPHA METRIC. Ratio of total desired pod CPU requests at decision start to total desired pod CPU requests at decision end. Labeled by decision dimensions.",
		},
		append(generateDecisionDimensions(), metrics.ResourceTypeLabel),
	)

	// NodeCountChangeRatio tracks the ratio of total node count before and after a decision
	NodeCountChangeRatio = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "node_count_change_ratio",
			Help:      "ALPHA METRIC. Ratio of total node count at decision start to total node count at decision end. Labeled by decision dimensions.",
		},
		generateDecisionDimensions(),
	)

	// DesiredPodCountChangeRatio tracks the ratio of total desired pod count before and after a decision
	DesiredPodCountChangeRatio = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "desired_pod_count_change_ratio",
			Help:      "ALPHA METRIC. Ratio of total desired pod count at decision start to total desired pod count at decision end. Labeled by decision dimensions.",
		},
		generateDecisionDimensions(),
	)
	// DecisionTrackerErrors tracks the number of internal errors in the decision tracker
	DecisionTrackerErrors = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "internal_errors_total",
			Help:      "ALPHA METRIC. Total Errors during the course of decision tracking",
		},
		[]string{},
	)
	// DecisionCacheExpirations tracks the number of internal errors in the decision tracker
	DecisionCacheExpirations = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: decisionLabel,
			Name:      "cache_expirations_total",
			Help:      "ALPHA METRIC. Total decisions that have taken longer than the timeout during the course of decision tracking",
		},
		[]string{},
	)
}

type TrackerInterface interface {
	AddCommand(ctx context.Context, cmd *Command) error
	FinishCommand(ctx context.Context, nc *karpv1.NodeClaim) error
}

type Tracker struct {
	sync.RWMutex
	cluster             *state.Cluster
	clusterCost         *cost.ClusterCost
	clock               clock.Clock
	decisionCache       *cache.Cache
	disruptionKeyMap    map[string]string
	bucketsThresholdMap map[string]*BucketThresholds
	enabled             bool
}

// NewTracker creates a queue that will asynchronously track and emit metrics on decisions
func NewTracker(cluster *state.Cluster, clusterCost *cost.ClusterCost, clock clock.Clock, decisionCache *cache.Cache, bucketsThresholdMap map[string]*BucketThresholds, enabled bool) *Tracker {
	bucketsThresholdMapCopy := map[string]*BucketThresholds{}
	if bucketsThresholdMap != nil {
		maps.Copy(bucketsThresholdMapCopy, bucketsThresholdMap)
		for _, bt := range bucketsThresholdMapCopy {
			sort.Slice(bt.Thresholds, func(i, j int) bool {
				return bt.Thresholds[i].LowerBound < bt.Thresholds[j].LowerBound
			})
		}
		for _, b := range generateBucketDimensions() {
			_, exists := bucketsThresholdMapCopy[b]
			if !exists && enabled {
				panic(fmt.Errorf("missing thresholds for necessary bucket %s", b))
			}
		}
	}
	decisionCache.OnEvicted(func(s string, i interface{}) {
		decision, ok := i.(TrackedDecision)
		if !ok {
			if clock.Since(decision.startTime) >= decisionTimeout {
				DecisionCacheExpirations.Inc(make(map[string]string))
			}
		}
	})
	initializeMetrics()
	tracker := &Tracker{
		// nolint:staticcheck
		decisionCache:       decisionCache,
		disruptionKeyMap:    make(map[string]string),
		bucketsThresholdMap: bucketsThresholdMap,
		enabled:             enabled,
		cluster:             cluster,
		clusterCost:         clusterCost,
		clock:               clock,
	}
	return tracker
}

func (t *Tracker) GatherClusterState(ctx context.Context) (*ClusterStateSnapshot, error) {
	return &ClusterStateSnapshot{
		ClusterCost:              t.clusterCost.GetClusterCost(),
		PodResources:             t.cluster.GetTotalPodResourceRequests(),
		TotalNodes:               len(t.cluster.DeepCopyNodes()),
		TotalDesiredPodCount:     t.cluster.GetTotalPodCount(),
		TotalDesiredPodResources: t.cluster.GetTotalPodResourceRequests(),
	}, nil
}

func (t *Tracker) AddCommand(ctx context.Context, cmd *Command) error {
	if !t.enabled {
		return nil
	}
	s, err := t.GatherClusterState(ctx)
	if err != nil {
		DecisionTrackerErrors.Inc(map[string]string{})
		return err
	}

	if len(cmd.Candidates) == 0 || cmd.Candidates[0].NodePool == nil {
		DecisionTrackerErrors.Inc(map[string]string{})
		return err
	}

	d := TrackedDecision{
		clusterState:      s,
		startTime:         t.clock.Now(),
		otherKeys:         lo.Map(cmd.Candidates, func(c *Candidate, _ int) string { return c.NodeClaim.Name }),
		consolidationType: cmd.ConsolidationType(),
		nodepoolName:      cmd.Candidates[0].NodePool.Name,
	}
	t.Lock()
	defer t.Unlock()
	var errors error
	for _, k := range d.otherKeys {
		err := t.decisionCache.Add(k, d, decisionTimeout)
		if err != nil {
			DecisionTrackerErrors.Inc(map[string]string{})
			errors = multierr.Append(errors, serrors.Wrap(err, "action", "failed to add decision key to cache", "key", k))
			continue
		}
	}
	return errors
}

func (t *Tracker) FinishCommand(ctx context.Context, nc *karpv1.NodeClaim) error {
	if !t.enabled {
		return nil
	}
	s, err := t.GatherClusterState(ctx)
	if err != nil {
		DecisionTrackerErrors.Inc(map[string]string{})
		return serrors.Wrap(err, "action", "gathering cluster state", "nc", nc)
	}
	t.Lock()
	defer t.Unlock()

	d, exists := t.decisionCache.Get(nc.Name)
	if !exists {
		DecisionTrackerErrors.Inc(map[string]string{})
		return serrors.Wrap(fmt.Errorf("decision not found in cache for NodeClaim"), "nodeclaim", nc)
	}
	t.decisionCache.Delete(nc.Name)
	decision, ok := d.(TrackedDecision)
	if !ok {
		DecisionTrackerErrors.Inc(map[string]string{})
		return serrors.Wrap(fmt.Errorf("failed to cast cached item to Decision for NodeClaim"), "nodeclaim", nc)
	}
	for _, k := range decision.otherKeys {
		_, exists := t.decisionCache.Get(k)
		if exists {
			// decision isn't done yet, lets wait
			return nil
		}
	}
	// we know the decision is over, lets emit metrics
	t.EmitMetrics(&decision, s)
	return nil
}

func (t *Tracker) EmitMetrics(start *TrackedDecision, end *ClusterStateSnapshot) {
	// Emit all the metrics with the correct dimensions

	// Calculate change ratios (Value at Beginning / Value at End)
	var clusterCostRatio, nodeCountRatio, desiredPodCountRatio float64

	// Cluster cost change ratio
	if end.ClusterCost != 0 {
		clusterCostRatio = start.clusterState.ClusterCost / end.ClusterCost
	}

	// Node count change ratio
	if end.TotalNodes != 0 {
		nodeCountRatio = float64(start.clusterState.TotalNodes) / float64(end.TotalNodes)
	}

	// Desired pod count change ratio
	if end.TotalDesiredPodCount != 0 && end.TotalDesiredPodCount != -1 {
		desiredPodCountRatio = float64(start.clusterState.TotalDesiredPodCount) / float64(end.TotalDesiredPodCount)
	}

	// Build labels map
	labels := map[string]string{
		ClusterCostLabel:          ConvertIntoBucket(start.clusterState.ClusterCost, t.bucketsThresholdMap[ClusterCostLabel]),
		TotalNodeCountLabel:       ConvertIntoBucket(float64(start.clusterState.TotalNodes), t.bucketsThresholdMap[TotalNodeCountLabel]),
		TotalDesiredPodCountLabel: ConvertIntoBucket(float64(start.clusterState.TotalDesiredPodCount), t.bucketsThresholdMap[TotalDesiredPodCountLabel]),
		ConsolidationTypeLabel:    start.consolidationType,
		NodePoolNameLabel:         start.nodepoolName,
	}

	// TODO: Add more resources to this, or allow resource injection
	for r := range karpv1.WellKnownResources {
		startQuantity, exists := start.clusterState.PodResources[r]
		if !exists {
			continue
		}
		labels[GenerateChangeRatioResourceLabel(r)] = ConvertIntoBucket(convertToChangeRatio(r, start.clusterState.PodResources[r], end.PodResources[r]), t.bucketsThresholdMap[GenerateChangeRatioResourceLabel(r)])
		labels[GenerateTotalRequestsResourceLabel(r)] = ConvertIntoBucket(startQuantity.AsApproximateFloat64(), t.bucketsThresholdMap[GenerateTotalRequestsResourceLabel(r)])
	}

	for n := range karpv1.WellKnownResources {
		startQuantity, exists := start.clusterState.PodResources[n]
		if !exists {
			continue
		}
		labelsCopy := map[string]string{}
		maps.Copy(labelsCopy, labels)
		labelsCopy[metrics.ResourceTypeLabel] = resources.ResourceNameToString(n)
		ResourceRequestsChangeRatio.Observe(convertToChangeRatio(n, startQuantity, end.PodResources[n]), labelsCopy)
	}

	// Emit all the change ratio metrics
	ClusterCostChangeRatio.Observe(clusterCostRatio, labels)
	NodeCountChangeRatio.Observe(nodeCountRatio, labels)
	DesiredPodCountChangeRatio.Observe(desiredPodCountRatio, labels)
}

func convertToChangeRatio(n v1.ResourceName, start resource.Quantity, end resource.Quantity) float64 {
	if !end.IsZero() {
		out := float64(start.Value()) / float64(end.Value())
		if n == v1.ResourceCPU {
			out = out / float64(1000)
		}
		return out
	} else {
		return 0.0
	}
}

func ConvertIntoBucket(metric float64, st *BucketThresholds) string {
	for _, t := range st.Thresholds {
		if metric < t.LowerBound {
			return t.Size
		}
	}
	return st.BiggestName
}

func generateDecisionDimensions() []string {
	return append([]string{
		ConsolidationTypeLabel,
		NodePoolNameLabel,
	}, generateBucketDimensions()...)
}

func generateBucketDimensions() []string {
	return append([]string{
		ClusterCostLabel,
		TotalNodeCountLabel,
		TotalDesiredPodCountLabel,
	}, generateResourceLabels()...)
}

func generateResourceLabels() []string {
	return lo.FlatMap(karpv1.WellKnownResources.UnsortedList(), func(n v1.ResourceName, _ int) []string {
		return []string{GenerateChangeRatioResourceLabel(n), GenerateTotalRequestsResourceLabel(n)}
	})
}

func GenerateChangeRatioResourceLabel(n v1.ResourceName) string {
	return fmt.Sprintf("pod_%s_request_change_ratio", resources.ResourceNameToString(n))
}
func GenerateTotalRequestsResourceLabel(n v1.ResourceName) string {
	return fmt.Sprintf("pod_%s_requests_total", resources.ResourceNameToString(n))
}
