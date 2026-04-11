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
	"k8s.io/apimachinery/pkg/util/intstr"
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

	// Apply topology policy: restrict candidates to the active domain per NodePool.
	// Must run before the empty/non-empty split to preserve the empty-first invariant.
	candidates = d.applyDriftPolicies(candidates)

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
		results, err := SimulateScheduling(ctx, d.kubeClient, d.cluster, d.provisioner, d.clock, d.recorder, candidate)
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

// domainStats tracks node counts for a topology domain.
type domainStats struct {
	total    int
	inFlight int
}

// applyDriftPolicies filters candidates based on each NodePool's DriftPolicy.
// When a DriftPolicy is configured, only candidates from the active topology domain
// are returned and the per-domain concurrency budget is enforced as a gate.
// Candidates without the topology label bypass domain filtering (fall-through).
// Returns candidates unchanged if no NodePool has a DriftPolicy configured.
// Must be called before the empty/non-empty candidate split so that the
// empty-first invariant is preserved within the filtered set.
func (d *Drift) applyDriftPolicies(candidates []*Candidate) []*Candidate {
	// Collect NodePools that have a DriftPolicy to avoid an unnecessary cluster read.
	npPolicies := map[string]*v1.DriftPolicy{}
	for _, c := range candidates {
		p := c.NodePool.Spec.Disruption.DriftPolicy
		if p != nil && p.TopologyKey != "" {
			npPolicies[c.NodePool.Name] = p
		}
	}
	if len(npPolicies) == 0 {
		return candidates
	}

	// Read cluster state once to build domain index for all relevant NodePools.
	index := d.buildDomainIndex(npPolicies)

	// Group candidates by NodePool for per-NodePool active-domain computation.
	// Map iteration order does not affect the output because we filter the
	// already-sorted input slice rather than building result from the map.
	byNodePool := lo.GroupBy(candidates, func(c *Candidate) string { return c.NodePool.Name })

	type policyState struct {
		activeDomain string
		budgetOK     bool
	}
	stateByNodePool := map[string]policyState{}
	for npName, npCandidates := range byNodePool {
		policy, hasPolicy := npPolicies[npName]
		if !hasPolicy {
			stateByNodePool[npName] = policyState{budgetOK: true}
			continue
		}
		active := d.activeTopologyDomain(policy.TopologyKey, index[npName], npCandidates)
		stateByNodePool[npName] = policyState{
			activeDomain: active,
			budgetOK:     active == "" || d.domainBudgetOK(policy, index[npName][active]),
		}
	}

	// Filter the already-sorted input slice, preserving order.
	return lo.Filter(candidates, func(c *Candidate, _ int) bool {
		ps := stateByNodePool[c.NodePool.Name]
		if !ps.budgetOK {
			return false
		}
		if ps.activeDomain == "" {
			return true // no DriftPolicy or no active domain found
		}
		domain := c.Labels()[c.NodePool.Spec.Disruption.DriftPolicy.TopologyKey]
		return domain == "" || domain == ps.activeDomain
	})
}

// domainBudgetOK reports whether the per-domain concurrency limit allows another
// disruption in the given domain. Returns true when no active domain is set (active == "").
func (d *Drift) domainBudgetOK(policy *v1.DriftPolicy, stats domainStats) bool {
	if policy.MaxConcurrentPerDomain == "" {
		return stats.inFlight < 1
	}
	maxAllowed, err := intstr.GetScaledValueFromIntOrPercent(
		lo.ToPtr(v1.GetIntStrFromValue(policy.MaxConcurrentPerDomain)),
		stats.total, true,
	)
	return err == nil && stats.inFlight < maxAllowed
}

// buildDomainIndex iterates all managed cluster nodes once to count total and
// in-flight nodes per (nodePoolName, domain) for the given NodePools.
// Uses the Nodes() iterator (lock-efficient, no deep copy).
func (d *Drift) buildDomainIndex(npPolicies map[string]*v1.DriftPolicy) map[string]map[string]domainStats {
	index := map[string]map[string]domainStats{}
	for n := range d.cluster.Nodes() {
		if !n.Managed() {
			continue
		}
		npName := n.Labels()[v1.NodePoolLabelKey]
		policy, ok := npPolicies[npName]
		if !ok {
			continue
		}
		domain := n.Labels()[policy.TopologyKey]
		if domain == "" {
			continue
		}
		if _, ok := index[npName]; !ok {
			index[npName] = map[string]domainStats{}
		}
		stats := index[npName][domain]
		stats.total++
		if n.MarkedForDeletion() {
			stats.inFlight++
		}
		index[npName][domain] = stats
	}
	return index
}

// activeTopologyDomain returns the topology domain that should be disrupted next.
// If any domain has in-flight disruptions, the alphabetically first such domain
// is returned (ensures stability across controller restarts).
// Otherwise, the alphabetically first domain with drifted candidates is returned.
func (d *Drift) activeTopologyDomain(topologyKey string, domainIndex map[string]domainStats, candidates []*Candidate) string {
	// Prefer the domain already being disrupted (in-flight nodes present).
	inFlightDomains := lo.FilterMap(lo.Keys(domainIndex), func(domain string, _ int) (string, bool) {
		return domain, domainIndex[domain].inFlight > 0
	})
	if len(inFlightDomains) > 0 {
		slices.Sort(inFlightDomains)
		return inFlightDomains[0]
	}
	// No in-flight: pick alphabetically first domain with drifted candidates.
	candidateDomains := lo.Uniq(lo.FilterMap(candidates, func(c *Candidate, _ int) (string, bool) {
		domain := c.Labels()[topologyKey]
		return domain, domain != ""
	}))
	if len(candidateDomains) == 0 {
		return ""
	}
	slices.Sort(candidateDomains)
	return candidateDomains[0]
}
