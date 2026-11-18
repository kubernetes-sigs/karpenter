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
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

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
			Help:      "Number of unhealthy nodes whose repair is past due: for one of the node's eligible repair policies, the toleration ended longer ago than the time repair gets to act. That time is the 15 minute node registration timeout plus the drain bound, the shorter of the policy's and the NodeClaim's termination grace period, or nothing if neither sets one. Nodes that repair holds back, such as for disruption budgets, still count. Labeled by the condition repair acts on. Not reported when node repair is off, with --legacy-node-repair, or before cluster state first syncs.",
		},
		[]opmetrics.Label{disruption.RepairCondition},
		opmetrics.Alpha,
	)
	// RepairPastDueDurationSeconds records how long a node was past due each time it stops being past due.
	RepairPastDueDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "repair_past_due_duration_seconds",
			Help:      "How long a node was past due on repair, recorded each time it stops being past due: it is removed, repair starts a reboot, it recovers, or repair stops acting on it, such as for do-not-repair. Labeled by the condition repair acted on last. Not recorded when node repair is off or with --legacy-node-repair.",
			Buckets:   prometheus.ExponentialBuckets(60, 2, 12), // 1m, 2m, 4m, ... up to about 34h
		},
		[]opmetrics.Label{disruption.RepairCondition},
		opmetrics.Alpha,
	)
	// RepairPastDueTimeSeconds reports how long each past-due node has been past due.
	RepairPastDueTimeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "repair_past_due_time_seconds",
			Help:      "How long a node has been past due on repair, reported while it is still past due. Labeled by the node name and the condition repair acts on. Not reported when node repair is off, with --legacy-node-repair, or before cluster state first syncs.",
		},
		[]opmetrics.Label{nodeNameLabel, disruption.RepairCondition},
		opmetrics.Alpha,
	)
)

// pastDueRepairs reports the nodes that are past due on repair, as disruption.RepairPastDueSince decides.
type pastDueRepairs struct {
	// labels maps each repair condition to its label value. Every condition reports 0 while no node is past due on it, so
	// the series don't disappear.
	labels map[corev1.NodeConditionType]string
	// pastDue holds each node's stretch from the last pass, by provider ID. It is in memory, so after a restart a stretch
	// is measured again from the node's conditions, and one that ends while Karpenter is down isn't recorded.
	pastDue map[string]pastDueNode
	// lastPass is when the last pass ran, or zero before the first one.
	lastPass time.Time
}

// pastDueNode is a node's current past-due stretch, which began at since.
type pastDueNode struct {
	label string
	since time.Time
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
	counts := lo.SliceToMap(lo.Values(r.labels), func(label string) (string, int) { return label, 0 })
	pastDue := map[string]pastDueNode{}
	res := map[string][]*metrics.StoreMetric{}
	for _, n := range nodes {
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
		current := pastDueNode{label: r.labels[result.Condition], since: since}
		counts[current.label]++
		pastDue[n.ProviderID()] = current
		// Each node gets its own store key, since the store compares every old series under a key with every new one,
		// which gets slow when many nodes are past due.
		res["repairPastDue/"+n.Node.Name] = []*metrics.StoreMetric{{
			GaugeMetric: RepairPastDueTimeSeconds,
			Value:       current.durationAt(now).Seconds(),
			Labels:      prometheus.Labels{nodeNameLabel.Name: n.Node.Name, disruption.RepairCondition.Name: current.label},
		}}
	}
	for providerID, previous := range r.pastDue {
		if _, found := pastDue[providerID]; !found {
			RepairPastDueDurationSeconds.Observe(previous.durationAt(now).Seconds(), map[string]string{disruption.RepairCondition.Name: previous.label})
		}
	}
	r.pastDue, r.lastPass = pastDue, now
	res["repairPastDue"] = lo.MapToSlice(counts, func(condition string, count int) *metrics.StoreMetric {
		return &metrics.StoreMetric{
			GaugeMetric: RepairPastDue,
			Value:       float64(count),
			Labels:      prometheus.Labels{disruption.RepairCondition.Name: condition},
		}
	})
	return res
}
