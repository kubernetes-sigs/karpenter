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

package scheduling

import (
	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// topologyNodePool holds the scheduling metadata for a single NodePool referenced by TopologyDomainGroups. One
// instance is constructed per NodePool and shared by pointer across every domain it can produce — storing
// requirements (or copies of them) per domain is what caused the memory (#2779) and CPU (#2954) regressions in
// previous attempts to filter topology domains.
type topologyNodePool struct {
	// requirements are the NodePool's template requirements and labels, plus the karpenter.sh/nodepool label the
	// NodePool would apply to its nodes. They deliberately exclude instance type requirements: labels derived from
	// instance types are undefined here and are treated as compatible with any pod requirement, which errs on the
	// side of including too many domains (the pre-existing behavior) rather than too few.
	requirements scheduling.Requirements
	taints       []v1.Taint
}

// TopologyDomainGroup tracks the domains for a single topology, along with the NodePools which can produce each
// domain. This enables us to determine which domains should be considered by a pod: a domain is considered if any
// NodePool producing it passes the pod's NodeTaintsPolicy (the pod tolerates the NodePool's taints) and
// NodeAffinityPolicy (the NodePool's requirements are compatible with the pod's node selector and required node
// affinity).
type TopologyDomainGroup map[string][]*topologyNodePool

func NewTopologyDomainGroup() TopologyDomainGroup {
	return TopologyDomainGroup{}
}

// Insert records that the given NodePool can produce the given domain. Inserts for a single NodePool are contiguous
// (buildDomainGroups fully processes one NodePool before moving to the next), so comparing against the last recorded
// producer is sufficient to deduplicate.
func (t TopologyDomainGroup) Insert(domain string, nodePool *topologyNodePool) {
	producers := t[domain]
	if len(producers) != 0 && producers[len(producers)-1] == nodePool {
		return
	}
	t[domain] = append(producers, nodePool)
}

// ForEachDomain calls f on each domain tracked by the topology group which is eligible for the provided pod. Each
// NodePool's eligibility is computed at most once per call and memoized, keeping the per-pod filtering cost at one
// taint/requirement evaluation per NodePool rather than per domain.
func (t TopologyDomainGroup) ForEachDomain(pod *v1.Pod, nodeFilter TopologyNodeFilter, f func(domain string)) {
	eligible := map[*topologyNodePool]bool{}
	for domain, producers := range t {
		for _, np := range producers {
			ok, seen := eligible[np]
			if !seen {
				ok = np.eligible(pod, nodeFilter)
				eligible[np] = ok
			}
			if ok {
				f(domain)
				break
			}
		}
	}
}

// eligible returns true if nodes produced by this NodePool may participate in the topology for the given pod under
// the node filter's taint and affinity policies.
func (np *topologyNodePool) eligible(pod *v1.Pod, nodeFilter TopologyNodeFilter) bool {
	if nodeFilter.TaintPolicy != v1.NodeInclusionPolicyIgnore {
		if err := scheduling.Taints(np.taints).ToleratesPod(pod); err != nil {
			return false
		}
	}
	if nodeFilter.AffinityPolicy == v1.NodeInclusionPolicyHonor && !matchesAnyRequirements(np.requirements, nodeFilter.Requirements) {
		return false
	}
	return true
}

// matchesAnyRequirements returns true if the NodePool requirements are compatible with any of the pod's requirement
// sets (one per required node affinity term, OR'd together). Undefined well-known labels are treated as compatible
// since instance types may provide them; undefined custom labels are incompatible since nodes owned by the NodePool
// can never have them.
func matchesAnyRequirements(nodePoolRequirements scheduling.Requirements, podRequirementSets []scheduling.Requirements) bool {
	if len(podRequirementSets) == 0 {
		return true
	}
	for _, req := range podRequirementSets {
		if err := nodePoolRequirements.Compatible(req, scheduling.AllowUndefinedWellKnownLabels); err == nil {
			return true
		}
	}
	return false
}
