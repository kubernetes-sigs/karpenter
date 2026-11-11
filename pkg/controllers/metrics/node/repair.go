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
	"context"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/node/legacyrepair"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

// RepairPastDue counts the nodes whose repair is late.
var RepairPastDue = opmetrics.NewPrometheusGauge(
	crmetrics.Registry,
	prometheus.GaugeOpts{
		Namespace: metrics.Namespace,
		Subsystem: metrics.NodeSubsystem,
		Name:      "repair_past_due",
		Help:      "Number of unhealthy nodes whose repair is late: a repair condition has outlasted its policy's toleration plus the time repair gets to act, which is the node registration timeout plus the drain bound repair applies. Nodes that repair holds back, such as for disruption budgets, still count. Labeled by the repair condition.",
	},
	[]opmetrics.Label{disruption.RepairCondition},
	opmetrics.Alpha,
)

// pastDueRepairs finds the nodes whose repair is late. It decides the way the running repair does: the repair
// disruption method, or the legacy controller under --legacy-node-repair.
type pastDueRepairs struct {
	// labels maps each repair condition to its label value. All are reported, so a condition with no late node shows 0
	// rather than disappearing.
	labels map[corev1.NodeConditionType]string
	// legacy is set when --legacy-node-repair runs the legacy controller in place of repair.
	legacy         bool
	legacyPolicies legacyrepair.ReplacePolicies
}

// newPastDueRepairs returns nil when node repair is off, since then no node is ever late.
func newPastDueRepairs(ctx context.Context, cloudProvider cloudprovider.CloudProvider) *pastDueRepairs {
	if !options.FromContext(ctx).FeatureGates.NodeRepair {
		return nil
	}
	policies := cloudProvider.RepairPolicies()
	r := &pastDueRepairs{legacy: options.FromContext(ctx).LegacyNodeRepair}
	conditionTypes := lo.Map(policies, func(policy cloudprovider.RepairPolicy, _ int) corev1.NodeConditionType {
		return policy.ConditionType
	})
	if r.legacy {
		r.legacyPolicies = legacyrepair.NewReplacePolicies(policies)
		// The legacy controller only replaces nodes, so a condition with only other actions is never repaired.
		conditionTypes = r.legacyPolicies.ConditionTypes()
	}
	r.labels = lo.SliceToMap(conditionTypes, func(conditionType corev1.NodeConditionType) (corev1.NodeConditionType, string) {
		return conditionType, pretty.ToSnakeCase(string(conditionType))
	})
	return r
}

// buildPastDueMetrics counts each late node once, under the condition whose repair is late.
func (r *pastDueRepairs) buildPastDueMetrics(nodes state.StateNodes, now time.Time) []*metrics.StoreMetric {
	counts := lo.SliceToMap(lo.Values(r.labels), func(label string) (string, int) { return label, 0 })
	for _, n := range nodes {
		if condition, ok := r.lateCondition(n, now); ok {
			counts[r.labels[condition]]++
		}
	}
	return lo.MapToSlice(counts, func(condition string, count int) *metrics.StoreMetric {
		return &metrics.StoreMetric{
			GaugeMetric: RepairPastDue,
			Value:       float64(count),
			Labels:      prometheus.Labels{disruption.RepairCondition.Name: condition},
		}
	})
}

// lateCondition returns the condition whose repair is late on the node, if any.
func (r *pastDueRepairs) lateCondition(n *state.StateNode, now time.Time) (corev1.NodeConditionType, bool) {
	if !n.Managed() {
		return "", false
	}
	if r.legacy {
		return r.legacyLateCondition(n.Node, now)
	}
	// Repair needs a NodePool, and do-not-repair vetoes it.
	if n.Labels()[v1.NodePoolLabelKey] == "" || disruption.DoNotRepair(n) {
		return "", false
	}
	// A reboot in progress is repair acting. The reboot controller bounds it, and the toleration restarts once it ends.
	if n.RebootInProgress() {
		return "", false
	}
	current := n.GetRepairResult(now)
	if current.Action == "" {
		return "", false
	}
	// Repair gets the node registration timeout to act, plus the drain bound to drain the node.
	allowance := lifecycle.RegistrationTimeout + lo.FromPtr(disruption.RepairDrainBound(n.NodeClaim, current))
	// The node is late if a policy was already eligible that long ago.
	late := n.GetRepairResult(now.Add(-allowance))
	return late.Condition, late.Action != ""
}

// legacyLateCondition follows the legacy controller. It deletes the node without waiting on a drain once the toleration
// ends, so it only gets the node registration timeout to remove the node.
func (r *pastDueRepairs) legacyLateCondition(node *corev1.Node, now time.Time) (corev1.NodeConditionType, bool) {
	condition, tolerationEnd := r.legacyPolicies.UnhealthyCondition(node)
	if condition == nil {
		return "", false
	}
	// The controller can't act before the node exists, so count from no earlier than its creation.
	eligibleAt := lo.Latest(tolerationEnd, node.CreationTimestamp.Time)
	return condition.Type, !now.Before(eligibleAt.Add(lifecycle.RegistrationTimeout))
}
