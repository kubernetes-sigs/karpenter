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

package node

import (
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

var (
	// RepairPastDue counts the nodes that are past due on repair now.
	RepairPastDue = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "repair_past_due",
			Help:      "Number of unhealthy nodes whose repair is past due: more time has passed since the first of the node's repair policies became eligible than repair gets to act. That time is the 15 minute node registration timeout plus the drain bound at that point, the shortest termination grace period that the NodeClaim and the policies eligible by then set, or nothing if none sets one. Policies that become eligible later don't change it. Nodes that repair holds back, such as for disruption budgets, still count. Labeled by the node's nodepool and the condition repair acts on. Not reported for a nodepool with no nodes, when node repair is off, with --legacy-node-repair, or before cluster state first syncs.",
		},
		[]opmetrics.Label{metrics.NodePool, disruption.RepairCondition},
		opmetrics.Alpha,
	)
	// RepairPastDueDurationSeconds records how long a node was past due each time it stops being past due.
	RepairPastDueDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "repair_past_due_duration_seconds",
			Help:      "How long a node was past due on repair, recorded each time it stops being past due: it is removed, repair starts a reboot, it recovers, or repair stops acting on it, such as for do-not-repair. Labeled by the node's nodepool and the condition repair acted on last. Not recorded when node repair is off, with --legacy-node-repair, or before cluster state first syncs.",
			Buckets:   prometheus.ExponentialBuckets(60, 2, 12), // 1m, 2m, 4m, ... up to about 34h
		},
		[]opmetrics.Label{metrics.NodePool, disruption.RepairCondition},
		opmetrics.Alpha,
	)
	// RepairPastDueTimeSeconds reports how long each past-due node has been past due.
	RepairPastDueTimeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "repair_past_due_time_seconds",
			Help:      "How long a node has been past due on repair, reported while it is still past due. Labeled by the node name, the node's nodepool and the condition repair acts on. Not reported when node repair is off, with --legacy-node-repair, or before cluster state first syncs.",
		},
		[]opmetrics.Label{nodeNameLabel, metrics.NodePool, disruption.RepairCondition},
		opmetrics.Alpha,
	)
)

// pastDueRepairs reports the nodes that are past due on repair, as disruption.RepairPastDueSince decides.
type pastDueRepairs struct {
	// labels maps each repair condition to its label value.
	labels map[corev1.NodeConditionType]string
	// pastDue holds each node's stretch from the last pass, by provider ID. It is in memory, so after a restart a stretch
	// is measured again from the node's conditions, and one that ends while Karpenter is down isn't recorded.
	pastDue map[string]pastDueNode
	// lastPass is when the last pass ran, or zero before the first one.
	lastPass time.Time
}

// pastDueSeries holds the labels that every past-due metric shares.
type pastDueSeries struct {
	nodePool  string
	condition string
}

func (s pastDueSeries) metricLabels() prometheus.Labels {
	return prometheus.Labels{metrics.NodePool.Name: s.nodePool, disruption.RepairCondition.Name: s.condition}
}

// pastDueNode is a node's current past-due stretch, which began at since.
type pastDueNode struct {
	series pastDueSeries
	since  time.Time
}

// durationAt returns how long the stretch has lasted as of now, or zero if the clock stepped back.
func (p pastDueNode) durationAt(now time.Time) time.Duration {
	return max(now.Sub(p.since), 0)
}

// newPastDueRepairs returns nil when the matcher is nil, since cluster state only has one while the repair disruption
// method runs.
func newPastDueRepairs(matcher *health.RepairPolicyMatcher) *pastDueRepairs {
	if matcher == nil {
		return nil
	}
	return &pastDueRepairs{labels: lo.SliceToMap(matcher.ConditionTypes(), func(conditionType corev1.NodeConditionType) (corev1.NodeConditionType, string) {
		return conditionType, pretty.ToSnakeCase(string(conditionType))
	})}
}

// update counts the nodes that are past due now and reports how long each has been past due. For each node whose
// past-due stretch ended since the last pass, it records how long the stretch was.
func (r *pastDueRepairs) update(nodes state.StateNodes, now time.Time) map[string][]*metrics.StoreMetric {
	nodePools := sets.New[string]()
	counts := map[pastDueSeries]int{}
	pastDue := map[string]pastDueNode{}
	res := map[string][]*metrics.StoreMetric{}
	for _, n := range nodes {
		nodePool := n.Labels()[v1.NodePoolLabelKey]
		if n.Managed() && nodePool != "" {
			nodePools.Insert(nodePool)
		}
		result, since, ok := disruption.RepairPastDueSince(n, now)
		if !ok {
			continue
		}
		if previous, found := r.pastDue[n.ProviderID()]; found {
			// The stretch goes on, even if the condition that started it has recovered since.
			since = previous.since
		} else if !r.lastPass.IsZero() {
			// The node wasn't past due at the last pass, such as while do-not-repair blocked repair, so its stretch can't
			// have started before then.
			since = lo.Latest(since, r.lastPass)
		}
		series := pastDueSeries{nodePool: nodePool, condition: r.labels[result.Condition]}
		counts[series]++
		current := pastDueNode{series: series, since: since}
		pastDue[n.ProviderID()] = current
		// Each node gets its own store key, since the store compares every old series under a key with every new one,
		// which gets slow when many nodes are past due.
		res["repairPastDue/"+n.Node.Name] = []*metrics.StoreMetric{{
			GaugeMetric: RepairPastDueTimeSeconds,
			Value:       current.durationAt(now).Seconds(),
			Labels:      lo.Assign(series.metricLabels(), prometheus.Labels{nodeNameLabel.Name: n.Node.Name}),
		}}
	}
	for providerID, previous := range r.pastDue {
		if _, found := pastDue[providerID]; !found {
			RepairPastDueDurationSeconds.Observe(previous.durationAt(now).Seconds(), previous.series.metricLabels())
		}
	}
	r.pastDue, r.lastPass = pastDue, now
	// Each NodePool with managed nodes reports every condition, even at 0, so its series stay while it has nodes. Like
	// each node, it gets its own store key.
	for nodePool := range nodePools {
		res["repairPastDueCount/"+nodePool] = lo.MapToSlice(r.labels, func(_ corev1.NodeConditionType, condition string) *metrics.StoreMetric {
			series := pastDueSeries{nodePool: nodePool, condition: condition}
			return &metrics.StoreMetric{GaugeMetric: RepairPastDue, Value: float64(counts[series]), Labels: series.metricLabels()}
		})
	}
	return res
}
