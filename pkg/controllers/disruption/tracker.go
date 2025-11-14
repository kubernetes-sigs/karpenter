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

	cache "github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

type Tracker struct {
	sync.RWMutex
	decisionCache       *cache.Cache
	disruptionKeyMap    map[string]string
	bucketsThresholdMap map[string]*BucketThresholds
	enabled             bool

	kubeClient  client.Client
	cluster     *state.Cluster
	clusterCost *cost.ClusterCost
	clock       clock.Clock
}

// NewTracker creates a queue that will asynchronously track and emit metrics on decisions
func NewTracker(cluster *state.Cluster, clock clock.Clock, decisionCache *cache.Cache, bucketsThresholdMap map[string]*BucketThresholds, enabled bool) *Tracker {
	decisionCache.OnEvicted(DecisionExpired)
	bucketsThresholdMapCopy := map[string]*BucketThresholds{}
	if bucketsThresholdMap != nil {
		maps.Copy(bucketsThresholdMapCopy, bucketsThresholdMap)
		for _, bt := range bucketsThresholdMapCopy {
			sort.Slice(bt.thresholds, func(i, j int) bool {
				return bt.thresholds[i].lowerBound < bt.thresholds[j].lowerBound
			})
		}
		for _, b := range DecisionDimensions {
			_, exists := bucketsThresholdMapCopy[b]
			if !exists {
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
		clock:               clock,
	}
	return tracker
}

func (t *Tracker) GatherClusterState(ctx context.Context) (*ClusterStateSnapshot, error) {
	deployments := appsv1.DeploymentList{}
	err := t.kubeClient.List(ctx, &deployments)
	if err != nil {
		return nil, err
	}

	totalDesiredPodCount := 0
	totalDesiredPodCount = lo.SumBy(deployments.Items, func(d appsv1.Deployment) int {
		return int(*d.Spec.Replicas)
	})

	rs := lo.Map(deployments.Items, func(d appsv1.Deployment, _ int) corev1.ResourceList {
		requests := lo.Map(d.Spec.Template.Spec.Containers, func(c corev1.Container, _ int) corev1.ResourceList {
			return c.Resources.Requests
		})

		totalContainerResources := resources.Merge(requests...)

		for i, r := range totalContainerResources {
			r.Set(r.Value() * int64(*d.Spec.Replicas))
			totalContainerResources[i] = r
		}

		return totalContainerResources
	})

	totalDesiredPodResources := resources.Merge(rs...)

	return &ClusterStateSnapshot{
		clusterCost:              t.clusterCost.GetClusterCost(),
		podResources:             t.cluster.GetTotalPodResourceRequests(),
		totalNodes:               len(t.cluster.DeepCopyNodes()),
		totalDesiredPodCount:     totalDesiredPodCount,
		totalDesiredPodResources: &totalDesiredPodResources,
	}, nil
}

func (t *Tracker) AddCommand(ctx context.Context, cmd *Command) {
	if !t.enabled {
		return
	}
	s, err := t.GatherClusterState(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "gathering cluster state", "cmd", cmd)
		DecisionTrackerErrors.Inc(map[string]string{})
		return
	}
	nodepoolName := "unknown"
	if len(cmd.Candidates) > 0 && cmd.Candidates[0].NodePool != nil {
		nodepoolName = cmd.Candidates[0].NodePool.Name
	}

	d := TrackedDecision{
		clusterState:      s,
		startTime:         t.clock.Now(),
		otherKeys:         lo.Map(cmd.Candidates, func(c *Candidate, _ int) string { return c.NodeClaim.Name }),
		consolidationType: cmd.ConsolidationType(),
		nodepoolName:      nodepoolName,
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
}

func (t *Tracker) FinishCommand(ctx context.Context, nc *v1.NodeClaim) {
	if !t.enabled {
		return
	}
	s, err := t.GatherClusterState(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "gathering cluster state", "nc", nc)
		DecisionTrackerErrors.Inc(map[string]string{})
		return
	}
	t.Lock()

	d, exists := t.decisionCache.Get(nc.Name)
	if !exists {
		log.FromContext(ctx).Error(fmt.Errorf("decision not found in cache for NodeClaim"), "nc", nc)
		DecisionTrackerErrors.Inc(map[string]string{})
		t.Unlock()
		return
	}
	decision, ok := d.(*TrackedDecision)
	if !ok {
		log.FromContext(ctx).Error(fmt.Errorf("failed to cast cached item to Decision for NodeClaim"), "nc", nc)
		DecisionTrackerErrors.Inc(map[string]string{})
		t.Unlock()
		return
	}
	for _, k := range decision.otherKeys {
		_, exists := t.decisionCache.Get(k)
		if exists {
			// decision isn't done yet, lets wait
			t.Unlock()
			return
		}
	}
	// we know the decision is over, lets emit metrics
	t.Unlock()
	t.EmitMetrics(decision, s)
}

func DecisionExpired(k string, d interface{}) {
	decision, ok := d.(*TrackedDecision)
	if !ok {
		log.FromContext(context.Background()).Error(fmt.Errorf("failed to cast cached item to Decision for key"), "key", k)
		DecisionTrackerErrors.Inc(map[string]string{})
		return
	}
	log.FromContext(context.Background()).Info("A decision expired", "decision", decision)
	DecisionTrackerCacheExpirations.Inc(map[string]string{})
}

func (t *Tracker) EmitMetrics(start *TrackedDecision, end *ClusterStateSnapshot) {
	// Emit all the metrics with the correct dimensions

	// Calculate change ratios (Value at Beginning / Value at End)
	var clusterCostRatio, cpuRequestsRatio, memoryRequestsRatio, nodeCountRatio, desiredPodCountRatio float64

	// Cluster cost change ratio
	if end.clusterCost != 0 {
		clusterCostRatio = start.clusterState.clusterCost / end.clusterCost
	}

	// CPU requests change ratio
	startCPU := start.clusterState.podResources[corev1.ResourceCPU]
	endCPU := end.podResources[corev1.ResourceCPU]
	if !endCPU.IsZero() {
		cpuRequestsRatio = float64(startCPU.MilliValue()) / float64(endCPU.MilliValue())
	}

	// Memory requests change ratio
	startMemory := start.clusterState.podResources[corev1.ResourceMemory]
	endMemory := end.podResources[corev1.ResourceMemory]
	if !endMemory.IsZero() {
		memoryRequestsRatio = float64(startMemory.Value()) / float64(endMemory.Value())
	}

	// Node count change ratio
	if end.totalNodes != 0 {
		nodeCountRatio = float64(start.clusterState.totalNodes) / float64(end.totalNodes)
	}

	// Desired pod count change ratio
	if end.totalDesiredPodCount != 0 && end.totalDesiredPodCount != -1 {
		desiredPodCountRatio = float64(start.clusterState.totalDesiredPodCount) / float64(end.totalDesiredPodCount)
	}

	// Build labels map
	labels := map[string]string{
		ClusterCostLabel:              convertIntoBucket(start.clusterState.clusterCost, t.bucketsThresholdMap[ClusterCostLabel]),
		TotalCPURequestsLabel:         convertIntoBucket(float64(startCPU.MilliValue()), t.bucketsThresholdMap[TotalCPURequestsLabel]),
		TotalMemoryRequestsLabel:      convertIntoBucket(float64(startMemory.Value()), t.bucketsThresholdMap[TotalMemoryRequestsLabel]),
		TotalNodeCountLabel:           convertIntoBucket(float64(start.clusterState.totalNodes), t.bucketsThresholdMap[TotalNodeCountLabel]),
		TotalDesiredPodCountLabel:     convertIntoBucket(float64(start.clusterState.totalDesiredPodCount), t.bucketsThresholdMap[TotalDesiredPodCountLabel]),
		ConsolidationTypeLabel:        start.consolidationType,
		PodCPURequestChangeRatioLabel: convertIntoBucket(cpuRequestsRatio, t.bucketsThresholdMap[PodCPURequestChangeRatioLabel]),
		PodMemRequestChangeRatioLabel: convertIntoBucket(memoryRequestsRatio, t.bucketsThresholdMap[PodMemRequestChangeRatioLabel]),
	}

	// Emit all the change ratio metrics
	ClusterCostChangeRatio.Set(clusterCostRatio, labels)
	CPURequestsChangeRatio.Set(cpuRequestsRatio, labels)
	MemoryRequestsChangeRatio.Set(memoryRequestsRatio, labels)
	NodeCountChangeRatio.Set(nodeCountRatio, labels)
	DesiredPodCountChangeRatio.Set(desiredPodCountRatio, labels)
}

func convertIntoBucket(metric float64, st *BucketThresholds) string {
	for _, t := range st.thresholds {
		if metric < t.lowerBound {
			return t.size
		}
	}
	return st.biggestName
}
