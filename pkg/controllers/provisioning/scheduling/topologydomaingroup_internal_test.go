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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("TopologyDomainGroup Internals", func() {
	// TopologyDomainGroup.Insert deduplicates producers by comparing only against the last recorded one, relying on
	// buildDomainGroups fully processing one NodePool before moving to the next. This spec guards that assumption:
	// if buildDomainGroups is ever restructured to interleave NodePools, producer lists grow duplicates. Duplicates
	// would not change which domains are selected (ForEachDomain stops at the first eligible producer and memoizes
	// per producer), but they would waste the memory and iteration work this design exists to avoid.
	It("should not record duplicate producers for a domain when building domain groups", func() {
		var nodePools []*v1.NodePool
		instanceTypes := map[string][]*cloudprovider.InstanceType{}
		// Multiple NodePools with overlapping zones and labels, each offering multiple instance types which all
		// produce the same zone domains, so every NodePool inserts every one of its domains repeatedly.
		for i := range 3 {
			np := test.NodePool(v1.NodePool{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pool-%d", i)},
				Spec: v1.NodePoolSpec{
					Template: v1.NodeClaimTemplate{
						ObjectMeta: v1.ObjectMeta{
							Labels: map[string]string{"team": fmt.Sprintf("team-%d", i%2)},
						},
						Spec: v1.NodeClaimTemplateSpec{
							Requirements: []v1.NodeSelectorRequirementWithMinValues{
								{
									Key:      corev1.LabelTopologyZone,
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"test-zone-1", "test-zone-2"},
								},
							},
						},
					},
				},
			})
			nodePools = append(nodePools, np)
			instanceTypes[np.Name] = fake.InstanceTypesAssorted()
		}

		domainGroups := buildDomainGroups(nodePools, instanceTypes)
		Expect(domainGroups).ToNot(BeEmpty())
		for topologyKey, domainGroup := range domainGroups {
			for domain, producers := range domainGroup {
				seen := map[*topologyNodePool]struct{}{}
				for _, producer := range producers {
					Expect(seen).ToNot(HaveKey(producer), "domain %q of topology key %q has a duplicate producer", domain, topologyKey)
					seen[producer] = struct{}{}
				}
			}
		}
	})
})
