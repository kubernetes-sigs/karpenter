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

	corev1 "k8s.io/api/core/v1"

	cache "github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"k8s.io/utils/clock"

	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/state/podresources"
)

type TrackerInterface interface {
	AddCommand(ctx context.Context, cmd *Command) error
	FinishCommand(ctx context.Context, nc *v1.NodeClaim) error
}

type Tracker struct {
	sync.RWMutex
	decisionCache       *cache.Cache
	disruptionKeyMap    map[string]string
	bucketsThresholdMap map[string]*BucketThresholds
	enabled             bool

	cluster      *state.Cluster
	clusterCost  *cost.ClusterCost
	podResources *podresources.PodResources
	clock        clock.Clock
}

// NewTracker creates a queue that will asynchronously track and emit metrics on decisions
func NewTracker(cluster *state.Cluster, clusterCost *cost.ClusterCost, podResources *podresources.PodResources, clock clock.Clock, decisionCache *cache.Cache, bucketsThresholdMap map[string]*BucketThresholds, enabled bool) *Tracker {
	bucketsThresholdMapCopy := map[string]*BucketThresholds{}
	if bucketsThresholdMap != nil {
		maps.Copy(bucketsThresholdMapCopy, bucketsThresholdMap)
		for _, bt := range bucketsThresholdMapCopy {
			sort.Slice(bt.Thresholds, func(i, j int) bool {
				return bt.Thresholds[i].LowerBound < bt.Thresholds[j].LowerBound
			})
		}
		for _, b := range DecisionBucketDimensions {
			_, exists := bucketsThresholdMapCopy[b]
			if !exists && enabled {
				panic(fmt.Errorf("missing thresholds for necessary bucket %s", b))
			}
		}
	}
	tracker := &Tracker{
		// nolint:staticcheck
		decisionCache:       decisionCache,
		disruptionKeyMap:    make(map[string]string),
		bucketsThresholdMap: bucketsThresholdMapCopy,
		enabled:             enabled,
		cluster:             cluster,
		clusterCost:         clusterCost,
		podResources:        podResources,
		clock:               clock,
	}
	return tracker
}

func (t *Tracker) GatherClusterState(ctx context.Context) (*ClusterStateSnapshot, error) {
	return &ClusterStateSnapshot{
		ClusterCost:              t.clusterCost.GetClusterCost(),
		PodResources:             t.cluster.GetTotalPodResourceRequests(),
		TotalNodes:               len(t.cluster.DeepCopyNodes()),
		TotalDesiredPodCount:     t.podResources.GetTotalPodCount(),
		TotalDesiredPodResources: t.podResources.GetTotalPodResourceRequests(),
	}, nil
}

func (t *Tracker) AddCommand(ctx context.Context, cmd *Command) error {
	if !t.enabled {
		return nil
	}
	s, err := t.GatherClusterState(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "gathering cluster state", "cmd", cmd)
		DecisionTrackerErrors.Inc(map[string]string{})
		return err
	}

	if len(cmd.Candidates) == 0 || cmd.Candidates[0].NodePool == nil {
		log.FromContext(ctx).Error(fmt.Errorf("getting nodepool"), "while adding command")
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
	for _, k := range d.otherKeys {
		err := t.decisionCache.Add(k, d, cache.DefaultExpiration)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to add decision to cache", "key", k)
			DecisionTrackerErrors.Inc(map[string]string{})
			continue
		}
	}
	return nil
}

func (t *Tracker) FinishCommand(ctx context.Context, nc *v1.NodeClaim) error {
	if !t.enabled {
		return nil
	}
	s, err := t.GatherClusterState(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "gathering cluster state", "nc", nc)
		DecisionTrackerErrors.Inc(map[string]string{})
		return err
	}
	t.Lock()
	defer t.Unlock()

	d, exists := t.decisionCache.Get(nc.Name)
	if !exists {
		log.FromContext(ctx).Error(fmt.Errorf("decision not found in cache for NodeClaim"), "finishing decision", "nc", nc)
		DecisionTrackerErrors.Inc(map[string]string{})
		return err
	}
	t.decisionCache.Delete(nc.Name)
	decision, ok := d.(TrackedDecision)
	if !ok {
		log.FromContext(ctx).Error(fmt.Errorf("failed to cast cached item to Decision for NodeClaim"), "finishing decision", "nc", nc)
		DecisionTrackerErrors.Inc(map[string]string{})
		return err
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
	var clusterCostRatio, cpuRequestsRatio, memoryRequestsRatio, nodeCountRatio, desiredPodCountRatio float64

	// Cluster cost change ratio
	if end.ClusterCost != 0 {
		clusterCostRatio = start.clusterState.ClusterCost / end.ClusterCost
	}

	// CPU requests change ratio
	startCPU := start.clusterState.PodResources[corev1.ResourceCPU]
	endCPU := end.PodResources[corev1.ResourceCPU]
	if !endCPU.IsZero() {
		cpuRequestsRatio = float64(startCPU.MilliValue()) / float64(endCPU.MilliValue())
	}

	// Memory requests change ratio
	startMemory := start.clusterState.PodResources[corev1.ResourceMemory]
	endMemory := end.PodResources[corev1.ResourceMemory]
	if !endMemory.IsZero() {
		memoryRequestsRatio = float64(startMemory.Value()) / float64(endMemory.Value())
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
		ClusterCostLabel:              ConvertIntoBucket(start.clusterState.ClusterCost, t.bucketsThresholdMap[ClusterCostLabel]),
		TotalCPURequestsLabel:         ConvertIntoBucket(float64(startCPU.MilliValue()), t.bucketsThresholdMap[TotalCPURequestsLabel]),
		TotalMemoryRequestsLabel:      ConvertIntoBucket(float64(startMemory.Value()), t.bucketsThresholdMap[TotalMemoryRequestsLabel]),
		TotalNodeCountLabel:           ConvertIntoBucket(float64(start.clusterState.TotalNodes), t.bucketsThresholdMap[TotalNodeCountLabel]),
		TotalDesiredPodCountLabel:     ConvertIntoBucket(float64(start.clusterState.TotalDesiredPodCount), t.bucketsThresholdMap[TotalDesiredPodCountLabel]),
		ConsolidationTypeLabel:        start.consolidationType,
		NodePoolNameLabel:             start.nodepoolName,
		PodCPURequestChangeRatioLabel: ConvertIntoBucket(cpuRequestsRatio, t.bucketsThresholdMap[PodCPURequestChangeRatioLabel]),
		PodMemRequestChangeRatioLabel: ConvertIntoBucket(memoryRequestsRatio, t.bucketsThresholdMap[PodMemRequestChangeRatioLabel]),
	}

	// Emit all the change ratio metrics
	ClusterCostChangeRatio.Set(clusterCostRatio, labels)
	CPURequestsChangeRatio.Set(cpuRequestsRatio, labels)
	MemoryRequestsChangeRatio.Set(memoryRequestsRatio, labels)
	NodeCountChangeRatio.Set(nodeCountRatio, labels)
	DesiredPodCountChangeRatio.Set(desiredPodCountRatio, labels)
}

func ConvertIntoBucket(metric float64, st *BucketThresholds) string {
	for _, t := range st.Thresholds {
		if metric < t.LowerBound {
			return t.Size
		}
	}
	return st.BiggestName
}
