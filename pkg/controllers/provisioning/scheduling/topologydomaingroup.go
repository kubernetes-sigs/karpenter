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
	"strings"

	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// DomainSource tracks the requirements and taints for a specific domain provided by a NodePool.
// This allows us to determine if a pod can use this domain based on its nodeSelector and nodeAffinity.
type DomainSource struct {
	// NodePoolRequirements contains the combined requirements from the NodePool's labels and requirements.
	// This includes both user-defined labels and instance type requirements (e.g., karpenter.k8s.aws/instance-size).
	NodePoolRequirements scheduling.Requirements
	// Taints are the taints associated with this domain from the NodePool.
	Taints []v1.Taint
}

// TopologyDomainGroup tracks the domains for a single topology. Additionally, it tracks the requirements and taints
// associated with each of these domains from the NodePools that provide them. This enables us to determine which
// domains should be considered by a pod based on its nodeSelector, nodeAffinity, and tolerations.
type TopologyDomainGroup struct {
	// domains maps each domain to the list of NodePool sources that can provide it
	domains map[string][]DomainSource
}

func NewTopologyDomainGroup() TopologyDomainGroup {
	return TopologyDomainGroup{
		domains: map[string][]DomainSource{},
	}
}

func domainSourceKey(requirements scheduling.Requirements, taints []v1.Taint) string {
	var b strings.Builder
	b.WriteString(requirements.String())
	b.WriteString("|taints=")
	for i := range taints {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(taints[i].Key)
		b.WriteString("=")
		b.WriteString(taints[i].Value)
		b.WriteString(":")
		b.WriteString(string(taints[i].Effect))
	}
	return b.String()
}

// Insert adds a domain to the TopologyDomainGroup with its associated NodePool requirements and taints.
// This tracks which NodePools can provide this domain, allowing us to filter domains based on pod requirements.
func (t TopologyDomainGroup) Insert(domain string, nodePoolRequirements scheduling.Requirements, taints ...v1.Taint) {
	if t.domains[domain] == nil {
		t.domains[domain] = []DomainSource{}
	}

	// Create a new domain source for this NodePool
	source := DomainSource{
		NodePoolRequirements: nodePoolRequirements,
		Taints:               taints,
	}

	// NOTE: We must not optimize/override sources based solely on taints.
	// Even if a NodePool has no taints, it may be incompatible with a pod's nodeSelector/nodeAffinity.
	// Keeping all sources ensures we can correctly include a domain if *any* NodePool providing it is compatible.
	key := domainSourceKey(nodePoolRequirements, taints)
	for _, existing := range t.domains[domain] {
		if domainSourceKey(existing.NodePoolRequirements, existing.Taints) == key {
			return
		}
	}
	t.domains[domain] = append(t.domains[domain], source)
}

// ForEachDomain calls f on each domain tracked by the topology group that is compatible with the pod's requirements.
// It filters domains based on:
//  1. Pod's requirements (from nodeSelector and any nodeAffinity term) don't conflict with NodePool requirements.
//     podRequirementSets contains one Requirements per nodeAffinity term (OR'd together), each combined with nodeSelector.
//     A domain is compatible if ANY of these requirement sets intersects with the NodePool.
//  2. Pod's tolerations tolerate the NodePool's taints (if taintHonorPolicy is honor)
func (t TopologyDomainGroup) ForEachDomain(pod *v1.Pod, podRequirementSets []scheduling.Requirements, taintHonorPolicy v1.NodeInclusionPolicy, f func(domain string)) {
	for domain, sources := range t.domains {
		if t.isDomainCompatible(pod, sources, podRequirementSets, taintHonorPolicy) {
			f(domain)
		}
	}
}

// isDomainCompatible checks if any source for a domain is compatible with any of the pod's requirement sets.
func (t TopologyDomainGroup) isDomainCompatible(pod *v1.Pod, sources []DomainSource, podRequirementSets []scheduling.Requirements, taintHonorPolicy v1.NodeInclusionPolicy) bool {
	for _, source := range sources {
		// Check if any of the pod's requirement sets (OR'd nodeAffinity terms) intersects
		// with this NodePool's requirements. If podRequirementSets is empty (e.g., for pod
		// affinity/anti-affinity where no nodeFilter is applied), skip the requirements check
		// and consider all domains as compatible from a requirements perspective.
		requirementsMatch := len(podRequirementSets) == 0
		for _, podReqs := range podRequirementSets {
			if err := source.NodePoolRequirements.Intersects(podReqs); err == nil {
				requirementsMatch = true
				break
			}
		}
		if !requirementsMatch {
			continue
		}

		// If taint policy is ignore, we don't need to check taints
		if taintHonorPolicy == v1.NodeInclusionPolicyIgnore {
			return true
		}

		// Check if pod tolerates this NodePool's taints
		if err := scheduling.Taints(source.Taints).ToleratesPod(pod); err == nil {
			return true
		}
	}
	return false
}
