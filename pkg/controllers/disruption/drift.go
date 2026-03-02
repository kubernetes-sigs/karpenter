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
	"errors"
	"slices"
	"sort"

	"github.com/samber/lo"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
)

// Drift is a subreconciler that deletes drifted candidates.
type Drift struct {
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
	recorder    events.Recorder
	clock       clock.Clock
}

func NewDrift(kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner, recorder events.Recorder, clk clock.Clock) *Drift {
	return &Drift{
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: provisioner,
		recorder:    recorder,
		clock:       clk,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (d *Drift) ShouldDisrupt(ctx context.Context, c *Candidate) bool {
	return !c.OwnedByStaticNodePool() && c.NodeClaim.StatusConditions().Get(string(d.Reason())).IsTrue()
}

// ComputeCommand generates a disruption command given candidates
func (d *Drift) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].NodeClaim.StatusConditions().Get(string(d.Reason())).LastTransitionTime.Time.Before(
			candidates[j].NodeClaim.StatusConditions().Get(string(d.Reason())).LastTransitionTime.Time)
	})

	candidates = d.filterBySequentialTopology(candidates)

	emptyCandidates, nonEmptyCandidates := lo.FilterReject(candidates, func(c *Candidate, _ int) bool {
		return len(c.reschedulablePods) == 0
	})

	// Prioritize empty candidates since we want them to get priority over non-empty candidates if the budget is constrained.
	// Disrupting empty candidates first also helps reduce the overall churn because if a non-empty candidate is disrupted first,
	// the pods from that node can reschedule on the empty nodes and will need to move again when those nodes get disrupted.
	for _, candidate := range slices.Concat(emptyCandidates, nonEmptyCandidates) {
		// If the disruption budget doesn't allow this candidate to be disrupted,
		// continue to the next candidate. We don't need to decrement any budget
		// counter since drift commands can only have one candidate.
		if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
			continue
		}
		// Check if we need to create any NodeClaims.
		results, err := SimulateScheduling(ctx, d.kubeClient, d.cluster, d.provisioner, candidate)
		if err != nil {
			// if a candidate is now deleting, just retry
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			return []Command{}, err
		}
		// Emit an event that we couldn't reschedule the pods on the node.
		if !results.AllNonPendingPodsScheduled() {
			d.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, pretty.Sentence(results.NonPendingPodSchedulingErrors()))...)
			continue
		}

		cmd := Command{
			Candidates:   []*Candidate{candidate},
			Replacements: replacementsFromNodeClaims(results.NewNodeClaims...),
			Results:      results,
		}
		return []Command{cmd}, nil

	}
	return []Command{}, nil
}

func (d *Drift) Reason() v1.DisruptionReason {
	return v1.DisruptionReasonDrifted
}

func (d *Drift) Class() string {
	return EventualDisruptionClass
}

func (d *Drift) ConsolidationType() string {
	return ""
}

// filterBySequentialTopology restricts candidates to a single active topology domain
// for NodePools with an active Sequential topology budget.
//
// Algorithm:
//  1. Group candidates by NodePool.
//  2. Pre-resolve each NodePool's active sequential budget.
//  3. Build per-(NodePool, domain) node counts in a single pass over all cluster nodes.
//  4. For each NodePool with an active sequential topology budget:
//     a. If any domain has in-flight disruptions → active domain = that domain
//     (chosen deterministically by sorted key).
//     b. If no in-flight disruptions → active domain = domain of candidates[0]
//     (already sorted oldest-drift-first, so this is the most urgent domain).
//     c. Count disrupting nodes in the active domain, compute remaining budget.
//     d. Return only candidates from the active domain, capped at remaining budget.
func (d *Drift) filterBySequentialTopology(candidates []*Candidate) []*Candidate {
	byNodePool := lo.GroupBy(candidates, func(c *Candidate) string { return c.NodePool.Name })
	allNodes := d.cluster.DeepCopyNodes()

	// Pre-resolve active sequential budgets so we know which topology keys to index.
	activeBudgets := make(map[string]*v1.Budget, len(byNodePool))
	for npName, npCandidates := range byNodePool {
		activeBudgets[npName] = d.findActiveSequentialBudget(npCandidates[0].NodePool)
	}

	// Build per-(NodePool, domain) counts in a single pass over all cluster nodes.
	numByDomain, inFlightByDomain := d.buildTopologyIndex(allNodes, activeBudgets)

	var result []*Candidate
	for npName, npCandidates := range byNodePool {
		budget := activeBudgets[npName]
		if budget == nil {
			result = append(result, npCandidates...)
			continue
		}
		activeDomain := pickActiveDomain(inFlightByDomain[npName], npCandidates, budget.TopologyKey)
		if activeDomain == "" {
			result = append(result, npCandidates...)
			continue
		}

		allowance, err := budget.GetAllowedDisruptions(d.clock, numByDomain[npName][activeDomain])
		if err != nil || allowance == 0 {
			continue
		}
		remaining := lo.Max([]int{allowance - inFlightByDomain[npName][activeDomain], 0})
		if remaining == 0 {
			continue
		}

		domainCandidates := lo.Filter(npCandidates, func(c *Candidate, _ int) bool {
			return c.Labels()[budget.TopologyKey] == activeDomain
		})
		if len(domainCandidates) > remaining {
			domainCandidates = domainCandidates[:remaining]
		}
		result = append(result, domainCandidates...)
	}
	return result
}

// buildTopologyIndex counts total and in-flight (MarkedForDeletion) nodes per
// (NodePool, topology domain) in a single pass over all cluster nodes.
// Only NodePools present in activeBudgets with a non-nil budget are indexed.
func (d *Drift) buildTopologyIndex(allNodes state.StateNodes, activeBudgets map[string]*v1.Budget) (numByDomain, inFlightByDomain map[string]map[string]int) {
	numByDomain = make(map[string]map[string]int)
	inFlightByDomain = make(map[string]map[string]int)
	for _, n := range allNodes {
		if !n.Managed() || !n.Initialized() {
			continue
		}
		npName := n.Labels()[v1.NodePoolLabelKey]
		budget, ok := activeBudgets[npName]
		if !ok || budget == nil {
			continue
		}
		if n.NodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).IsTrue() {
			continue
		}
		domain := n.Labels()[budget.TopologyKey]
		if domain == "" {
			continue
		}
		if numByDomain[npName] == nil {
			numByDomain[npName] = make(map[string]int)
			inFlightByDomain[npName] = make(map[string]int)
		}
		numByDomain[npName][domain]++
		if n.MarkedForDeletion() {
			inFlightByDomain[npName][domain]++
		}
	}
	return numByDomain, inFlightByDomain
}

// pickActiveDomain returns the topology domain that should be disrupted next.
// If any domain already has in-flight disruptions, that domain is continued;
// the domain is chosen deterministically by sorted key.
// Otherwise the domain of the first (oldest-drifted) candidate is chosen.
func pickActiveDomain(inFlightByDomain map[string]int, candidates []*Candidate, topologyKey string) string {
	if len(inFlightByDomain) > 0 {
		domains := make([]string, 0, len(inFlightByDomain))
		for domain := range inFlightByDomain {
			domains = append(domains, domain)
		}
		sort.Strings(domains)
		for _, domain := range domains {
			if inFlightByDomain[domain] > 0 {
				return domain
			}
		}
	}
	if len(candidates) > 0 {
		return candidates[0].Labels()[topologyKey]
	}
	return ""
}

// findActiveSequentialBudget returns the first active budget with TopologyKey +
// Sequential=true that applies to the Drifted reason. Returns nil if none.
func (d *Drift) findActiveSequentialBudget(nodePool *v1.NodePool) *v1.Budget {
	for i := range nodePool.Spec.Disruption.Budgets {
		b := &nodePool.Spec.Disruption.Budgets[i]
		if b.TopologyKey == "" || !b.Sequential {
			continue
		}
		if b.Reasons != nil && !lo.Contains(b.Reasons, v1.DisruptionReasonDrifted) {
			continue
		}
		active, err := b.IsActive(d.clock)
		if err != nil || !active {
			continue
		}
		return b
	}
	return nil
}
