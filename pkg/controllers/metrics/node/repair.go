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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

// pastDueRepairs reports how long each node has been past due on repair, as disruption.RepairPastDueSince decides, and
// why, as disruption.RepairPastDueReason decides.
type pastDueRepairs struct {
	// pastDue holds each node's stretch from the last pass, by provider ID. It is in memory, so after a restart a stretch
	// is measured again from the node's conditions, and one that ends while Karpenter is down isn't recorded.
	pastDue map[string]pastDueNode
	// lastPass is when the last pass ran, or zero before the first one.
	lastPass time.Time
}

// pastDueNode is a node's current past-due stretch, which began at since.
type pastDueNode struct {
	// histogramLabels are the node's gauge labels from the last pass, minus node_name and blocked_reason. The stretch is
	// recorded under them, since when it ends the node has no condition and may be gone.
	histogramLabels prometheus.Labels
	since           time.Time
	// reason is the node's blocked reason at this controller's last pass.
	reason string
	// reasonTimes is how long the node has had each reason during the stretch.
	reasonTimes map[string]time.Duration
}

// durationAt returns how long the stretch has lasted as of now, or zero if the clock stepped back.
func (p pastDueNode) durationAt(now time.Time) time.Duration {
	return max(now.Sub(p.since), 0)
}

// longestReason returns the reason the node had for the most time. A tie goes to the reason that the label lists first.
func (p pastDueNode) longestReason() string {
	longest, longestTime := p.reason, time.Duration(-1)
	for _, value := range disruption.RepairBlockedReason.Values {
		if t, ok := p.reasonTimes[value.Name]; ok && t > longestTime {
			longest, longestTime = value.Name, t
		}
	}
	return longest
}

// newPastDueRepairs returns nil when the matcher is nil, since cluster state only has one while the repair disruption
// method runs.
func newPastDueRepairs(matcher *health.RepairPolicyMatcher) *pastDueRepairs {
	if matcher == nil {
		return nil
	}
	return &pastDueRepairs{}
}

// update reports each managed node in a NodePool: how long it has been past due, or 0 while it isn't. It records each
// past-due stretch that ended since the last pass. heldBack is cluster state's RepairBlockedReasons.
func (r *pastDueRepairs) update(nodes state.StateNodes, now time.Time, heldBack map[string]string) map[string][]*metrics.StoreMetric {
	// Add the time since the last pass to the reason each past-due node had then, or nothing if the clock stepped back.
	for _, previous := range r.pastDue {
		previous.reasonTimes[previous.reason] += max(now.Sub(r.lastPass), 0)
	}
	pastDue := map[string]pastDueNode{}
	res := map[string][]*metrics.StoreMetric{}
	for _, n := range nodes {
		// Repair only acts on managed nodes in a NodePool, as disruption.RepairPastDueSince checks first, so no other node
		// can be past due.
		if !n.Managed() || n.Labels()[v1.NodePoolLabelKey] == "" {
			continue
		}
		labels := getNodeLabels(n)
		// Every row needs the condition and blocked reason labels, so they are empty until the node is past due.
		labels[disruption.RepairCondition.Name] = ""
		labels[disruption.RepairBlockedReason.Name] = ""
		var pastDueFor time.Duration
		if result, since, ok := disruption.RepairPastDueSince(n, now); ok {
			var reasonTimes map[string]time.Duration
			if previous, found := r.pastDue[n.ProviderID()]; found {
				// The stretch goes on, even if the condition that started it has recovered since.
				since, reasonTimes = previous.since, previous.reasonTimes
			} else {
				if !r.lastPass.IsZero() {
					// The node wasn't past due at the last pass, such as while do-not-repair blocked repair, so its stretch can't
					// have started before then.
					since = lo.Latest(since, r.lastPass)
				}
				// No pass saw the stretch until now, such as before a restart, so its time so far has no known reason.
				reasonTimes = map[string]time.Duration{disruption.RepairBlockedUnknown.Name: max(now.Sub(since), 0)}
			}
			reason := disruption.RepairPastDueReason(n, heldBack[n.ProviderID()])
			labels[disruption.RepairCondition.Name] = pretty.ToSnakeCase(string(result.Condition))
			labels[disruption.RepairBlockedReason.Name] = reason
			current := pastDueNode{
				histogramLabels: lo.OmitByKeys(labels, []string{nodeNameLabel.Name, disruption.RepairBlockedReason.Name}),
				since:           since,
				reason:          reason,
				reasonTimes:     reasonTimes,
			}
			pastDue[n.ProviderID()] = current
			pastDueFor = current.durationAt(now)
		}
		// Each node gets its own store key, since the store compares every old series under a key with every new one,
		// which gets slow with many nodes.
		res["repairPastDue/"+n.Node.Name] = []*metrics.StoreMetric{{
			GaugeMetric: RepairPastDueTimeSeconds,
			Value:       pastDueFor.Seconds(),
			Labels:      labels,
		}}
	}
	for providerID, previous := range r.pastDue {
		if _, found := pastDue[providerID]; !found {
			// Use the reason the node had longest, since the last one is nearly always deleting for a node repair replaced.
			RepairPastDueDurationSeconds.Observe(previous.durationAt(now).Seconds(),
				lo.Assign(previous.histogramLabels, prometheus.Labels{disruption.RepairBlockedReason.Name: previous.longestReason()}))
		}
	}
	r.pastDue, r.lastPass = pastDue, now
	return res
}
