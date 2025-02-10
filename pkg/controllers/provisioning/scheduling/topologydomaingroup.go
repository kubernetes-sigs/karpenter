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

// TopologyDomainGroup tracks the domains for a single topology. Additionally, it tracks the taints associated with
// each of these domains. This enables us to determine which domains should be considered by a pod if its
// NodeTaintPolicy is honor.
type TopologyDomainGroup map[string][][]v1.Taint

func NewTopologyDomainGroup() TopologyDomainGroup {
	return map[string][][]v1.Taint{}
}

// Insert either adds a new domain to the TopologyDomainGroup or updates an existing domain.
func (t TopologyDomainGroup) Insert(domain string, taints ...v1.Taint) {
	if _, ok := t[domain]; !ok || len(taints) == 0 {
		t[domain] = [][]v1.Taint{taints}
		return
	}
	if len(t[domain][0]) == 0 {
		// The domain already contains a set of taints which is the empty set, therefore this domain can be considered
		// by all pods, regardless of their tolerations. There is no longer a need to track new sets of taints.
		// This could potentially be generalized by removing any set for which the new set of taints is a proper subset, but
		// for now this is just handled for the empty set.
		return
	}
	t[domain] = append(t[domain], taints)
}

// ForEachDomain calls f on each domain tracked by the topology group
func (t TopologyDomainGroup) ForEachDomain(f func(domain string)) {
	for domain := range t {
		f(domain)
	}
}

// ForEachToleratedDomain calls f on each domain tracked by the TopologyDomainGroup which are also tolerated by the provided pod.
func (t TopologyDomainGroup) ForEachToleratedDomain(pod *v1.Pod, f func(domain string)) {
	// Could potentially improve performance by hashing the pod's tolerations and storing a map from toleration hash to
	// eligible domains.
	for domain, taintGroups := range t {
		for _, taints := range taintGroups {
			if err := scheduling.Taints(taints).ToleratesPod(pod); err == nil {
				f(domain)
				break
			}
		}
	}
}
