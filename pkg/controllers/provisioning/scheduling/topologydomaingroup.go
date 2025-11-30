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

	// Check if we should override existing sources based on taint optimization
	// If there are no taints, this NodePool makes the domain available to all pods (regarding taints),
	// so we can replace all existing sources
	if len(taints) == 0 {
		t.domains[domain] = []DomainSource{source}
		return
	}

	// If we already have a source with no taints, no need to add this one
	if len(t.domains[domain]) > 0 && len(t.domains[domain][0].Taints) == 0 {
		return
	}

	// Add this source to the list
	t.domains[domain] = append(t.domains[domain], source)
}

// ForEachDomain calls f on each domain tracked by the topology group that is compatible with the pod's requirements.
// It filters domains based on:
// 1. Pod's requirements don't conflict with NodePool requirements
// 2. Pod's tolerations tolerate the NodePool's taints (if taintHonorPolicy is honor)
func (t TopologyDomainGroup) ForEachDomain(pod *v1.Pod, podRequirements scheduling.Requirements, taintHonorPolicy v1.NodeInclusionPolicy, f func(domain string)) {
	for domain, sources := range t.domains {
		// Check if any source for this domain is compatible with the pod
		isCompatible := false

		for _, source := range sources {
			// Check if pod requirements don't conflict with NodePool requirements
			// We use Intersects to check if they can coexist (no conflicts)
			if err := source.NodePoolRequirements.Intersects(podRequirements); err != nil {
				continue
			}

			// If taint policy is ignore, we don't need to check taints
			if taintHonorPolicy == v1.NodeInclusionPolicyIgnore {
				isCompatible = true
				break
			}

			// Check if pod tolerates this NodePool's taints
			if err := scheduling.Taints(source.Taints).ToleratesPod(pod); err == nil {
				isCompatible = true
				break
			}
		}

		if isCompatible {
			f(domain)
			continue
		}
	}
}
