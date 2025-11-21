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

type topologyDomain struct {
	taints       [][]v1.Taint
	requirements []scheduling.Requirements
}

// TopologyDomainGroup tracks the domains for a single topology. Additionally, it tracks the taints associated with
// each of these domains as well as the requirements that produce them. This enables us to determine which domains
// should be considered by a pod if its NodeTaintPolicy or NodeAffinityPolicy is honor.
type TopologyDomainGroup map[string]*topologyDomain

func NewTopologyDomainGroup() TopologyDomainGroup {
	return map[string]*topologyDomain{}
}

// Insert either adds a new domain to the TopologyDomainGroup or updates an existing domain. The provided requirements
// describe the set of constraints that yield this domain.
func (t TopologyDomainGroup) Insert(domain string, requirements scheduling.Requirements, taints ...v1.Taint) {
	entry, ok := t[domain]
	if !ok {
		entry = &topologyDomain{}
		t[domain] = entry
	}

	// If the domain is not currently tracked, insert it with the associated taints. Additionally, if there are no taints
	// provided, override the taints associated with the domain. Generally, we could remove any sets of taints for which
	// the provided set is a proper subset. This is because if a pod tolerates the supersets, it will also tolerate the
	// proper subset, and removing the superset reduces the number of taint sets we need to traverse. For now we only
	// implement the simplest case, the empty set, but we could do additional performance testing to determine if
	// implementing the general case is worth the precomputation cost.
	if len(entry.taints) == 0 || len(taints) == 0 {
		entry.taints = [][]v1.Taint{taints}
	} else if len(entry.taints[0]) != 0 {
		entry.taints = append(entry.taints, taints)
	}

	entry.requirements = append(entry.requirements, requirements.Clone())
}

// ForEachDomain calls f on each domain tracked by the topology group. The provided filter determines which domains
// should be included based on the pod's topology spread policies.
func (t TopologyDomainGroup) ForEachDomain(pod *v1.Pod, filter TopologyNodeFilter, f func(domain string)) {
	for domain, entry := range t {
		if entry.matches(filter, pod) {
			f(domain)
		}
	}
}

func (d *topologyDomain) matches(filter TopologyNodeFilter, pod *v1.Pod) bool {
	if filter.TaintPolicy == v1.NodeInclusionPolicyHonor {
		taintMatched := false
		for _, taints := range d.taints {
			if err := scheduling.Taints(taints).ToleratesPod(pod); err == nil {
				taintMatched = true
				break
			}
		}
		if !taintMatched {
			return false
		}
	}

	if filter.AffinityPolicy == v1.NodeInclusionPolicyHonor {
		for _, reqs := range d.requirements {
			if filter.matchesRequirements(reqs) {
				return true
			}
		}
		return false
	}

	return true
}
