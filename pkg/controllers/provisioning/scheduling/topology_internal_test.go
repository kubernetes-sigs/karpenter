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
	"math/rand/v2"
	"slices"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	testPoolKey       = "example.com/pool"
	testGenerationKey = "example.com/generation"
)

var testPoolTaint = corev1.Taint{Key: testPoolKey, Value: "true", Effect: corev1.TaintEffectNoSchedule}

// buildDomainGroupsReference is the implementation before the NodePool requirements were hoisted out of the loop.
func buildDomainGroupsReference(nodePools []*v1.NodePool, instanceTypes map[string][]*cloudprovider.InstanceType) map[string]TopologyDomainGroup {
	nodePoolIndex := lo.SliceToMap(nodePools, func(np *v1.NodePool) (string, *v1.NodePool) {
		return np.Name, np
	})
	domainGroups := map[string]TopologyDomainGroup{}
	for npName, its := range instanceTypes {
		np := nodePoolIndex[npName]
		for _, it := range its {
			requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(np.Spec.Template.Spec.Requirements...)
			requirements.Add(scheduling.NewLabelRequirements(np.Spec.Template.Labels).Values()...)
			requirements.Add(it.Requirements.Values()...)

			for topologyKey, requirement := range requirements {
				if _, ok := domainGroups[topologyKey]; !ok {
					domainGroups[topologyKey] = NewTopologyDomainGroup()
				}
				for _, domain := range requirement.Values() {
					domainGroups[topologyKey].Insert(domain, np.Spec.Template.Spec.Taints...)
				}
			}
		}

		requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(np.Spec.Template.Spec.Requirements...)
		requirements.Add(scheduling.NewLabelRequirements(np.Spec.Template.Labels).Values()...)
		for key, requirement := range requirements {
			if requirement.Operator() == corev1.NodeSelectorOpIn {
				if _, ok := domainGroups[key]; !ok {
					domainGroups[key] = NewTopologyDomainGroup()
				}
				for _, value := range requirement.Values() {
					domainGroups[key].Insert(value, np.Spec.Template.Spec.Taints...)
				}
			}
		}
	}
	return domainGroups
}

// normalizeDomainGroups sorts taint groups, whose order depends on map iteration, but keeps their multiplicity.
func normalizeDomainGroups(domainGroups map[string]TopologyDomainGroup) map[string]map[string][]string {
	normalized := map[string]map[string][]string{}
	for topologyKey, domainGroup := range domainGroups {
		normalized[topologyKey] = map[string][]string{}
		for domain, taintGroups := range domainGroup {
			rendered := make([]string, 0, len(taintGroups))
			for _, taints := range taintGroups {
				taintStrings := make([]string, 0, len(taints))
				for _, taint := range taints {
					taintStrings = append(taintStrings, fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect))
				}
				sort.Strings(taintStrings)
				rendered = append(rendered, strings.Join(taintStrings, ","))
			}
			sort.Strings(rendered)
			normalized[topologyKey][domain] = rendered
		}
	}
	return normalized
}

func domainsByKey(domainGroups map[string]TopologyDomainGroup) map[string][]string {
	domains := map[string][]string{}
	for topologyKey, domainGroup := range domainGroups {
		domains[topologyKey] = lo.Keys(domainGroup)
		sort.Strings(domains[topologyKey])
	}
	return domains
}

func testNodePool(name string, requirements []v1.NodeSelectorRequirementWithMinValues, labels map[string]string, taints []corev1.Taint) *v1.NodePool {
	return &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.NodePoolSpec{
			Template: v1.NodeClaimTemplate{
				ObjectMeta: v1.ObjectMeta{Labels: labels},
				Spec: v1.NodeClaimTemplateSpec{
					Requirements: requirements,
					Taints:       taints,
				},
			},
		},
	}
}

func testInstanceType(name string, requirements ...*scheduling.Requirement) *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{Name: name, Requirements: scheduling.NewRequirements(requirements...)}
}

// deepCopyDomainGroupInput gives the reference its own input, so a mutation by either implementation is not hidden.
func deepCopyDomainGroupInput(nodePools []*v1.NodePool, instanceTypes map[string][]*cloudprovider.InstanceType) ([]*v1.NodePool, map[string][]*cloudprovider.InstanceType) {
	copiedNodePools := make([]*v1.NodePool, 0, len(nodePools))
	for _, nodePool := range nodePools {
		copiedNodePools = append(copiedNodePools, nodePool.DeepCopy())
	}
	copiedInstanceTypes := map[string][]*cloudprovider.InstanceType{}
	for nodePoolName, its := range instanceTypes {
		copied := make([]*cloudprovider.InstanceType, 0, len(its))
		for _, it := range its {
			requirements := scheduling.NewRequirements()
			for _, requirement := range it.Requirements.Values() {
				requirements.Add(requirement.DeepCopy())
			}
			copied = append(copied, &cloudprovider.InstanceType{Name: it.Name, Requirements: requirements})
		}
		copiedInstanceTypes[nodePoolName] = copied
	}
	return copiedNodePools, copiedInstanceTypes
}

func expectMatchesReference(nodePools []*v1.NodePool, instanceTypes map[string][]*cloudprovider.InstanceType) map[string]TopologyDomainGroup {
	referenceNodePools, referenceInstanceTypes := deepCopyDomainGroupInput(nodePools, instanceTypes)
	reference := normalizeDomainGroups(buildDomainGroupsReference(referenceNodePools, referenceInstanceTypes))
	domainGroups := buildDomainGroups(nodePools, instanceTypes)
	ExpectWithOffset(1, normalizeDomainGroups(domainGroups)).To(Equal(reference))
	return domainGroups
}

var _ = Describe("buildDomainGroups", func() {
	DescribeTable("should build the same domains as the pre-hoist implementation",
		func(nodePools []*v1.NodePool, instanceTypes map[string][]*cloudprovider.InstanceType, expectedDomains map[string][]string) {
			Expect(domainsByKey(expectMatchesReference(nodePools, instanceTypes))).To(Equal(expectedDomains))
		},
		Entry("intersecting a zone requirement held by both the NodePool and the instance type",
			[]*v1.NodePool{testNodePool("np", []v1.NodeSelectorRequirementWithMinValues{{
				Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"zone-a", "zone-b"},
			}}, nil, nil)},
			map[string][]*cloudprovider.InstanceType{"np": {
				testInstanceType("it-1", scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-b", "zone-c")),
			}},
			// zone-b is the intersection, and the trailing In pass adds the NodePool's own zone-a back.
			map[string][]string{corev1.LabelTopologyZone: {"zone-a", "zone-b"}},
		),
		Entry("carrying the NodePool taints onto every domain",
			[]*v1.NodePool{testNodePool("np", []v1.NodeSelectorRequirementWithMinValues{{
				Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"zone-a"},
			}}, nil, []corev1.Taint{testPoolTaint})},
			map[string][]*cloudprovider.InstanceType{"np": {
				testInstanceType("it-1",
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a"),
					scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "it-1"),
				),
			}},
			map[string][]string{
				corev1.LabelTopologyZone:       {"zone-a"},
				corev1.LabelInstanceTypeStable: {"it-1"},
			},
		),
		Entry("preserving minValues on a requirement held by both",
			[]*v1.NodePool{testNodePool("np", []v1.NodeSelectorRequirementWithMinValues{{
				Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"zone-a", "zone-b"}, MinValues: new(2),
			}}, nil, nil)},
			map[string][]*cloudprovider.InstanceType{"np": {
				testInstanceType("it-1", scheduling.NewRequirementWithFlexibility(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, new(1), "zone-a", "zone-b")),
			}},
			map[string][]string{corev1.LabelTopologyZone: {"zone-a", "zone-b"}},
		),
		Entry("keeping a NodePool In requirement the instance type does not carry",
			[]*v1.NodePool{testNodePool("np", []v1.NodeSelectorRequirementWithMinValues{{
				Key: testPoolKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"pool-a", "pool-b"},
			}}, nil, []corev1.Taint{testPoolTaint})},
			map[string][]*cloudprovider.InstanceType{"np": {
				testInstanceType("it-1", scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a")),
			}},
			map[string][]string{
				testPoolKey:              {"pool-a", "pool-b"},
				corev1.LabelTopologyZone: {"zone-a"},
			},
		),
		Entry("intersecting NodePool labels with the instance type",
			[]*v1.NodePool{testNodePool("np", nil, map[string]string{corev1.LabelTopologyZone: "zone-a"}, nil)},
			map[string][]*cloudprovider.InstanceType{"np": {
				testInstanceType("it-1", scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a", "zone-b")),
			}},
			map[string][]string{corev1.LabelTopologyZone: {"zone-a"}},
		),
		Entry("dropping a domain when the NodePool and instance type requirements do not intersect",
			[]*v1.NodePool{testNodePool("np", []v1.NodeSelectorRequirementWithMinValues{{
				Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"arm64"},
			}}, nil, nil)},
			map[string][]*cloudprovider.InstanceType{"np": {
				testInstanceType("it-1", scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64")),
			}},
			// The intersection is empty, so only the trailing In pass contributes arm64.
			map[string][]string{corev1.LabelArchStable: {"arm64"}},
		),
		// Bounds are the only way to reach the in-place values.Delete in Requirement.Intersection.
		Entry("intersecting a NodePool In requirement with an instance type lower bound",
			[]*v1.NodePool{testNodePool("np", []v1.NodeSelectorRequirementWithMinValues{{
				Key: testGenerationKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"1", "5", "9"},
			}}, nil, nil)},
			map[string][]*cloudprovider.InstanceType{"np": {
				testInstanceType("it-1", scheduling.NewRequirement(testGenerationKey, v1.NodeSelectorOpGte, "5")),
			}},
			map[string][]string{testGenerationKey: {"1", "5", "9"}},
		),
		Entry("intersecting a NodePool lower bound with an instance type In requirement",
			[]*v1.NodePool{testNodePool("np", []v1.NodeSelectorRequirementWithMinValues{{
				Key: testGenerationKey, Operator: v1.NodeSelectorOpGte, Values: []string{"5"},
			}}, nil, nil)},
			map[string][]*cloudprovider.InstanceType{"np": {
				testInstanceType("it-1", scheduling.NewRequirement(testGenerationKey, corev1.NodeSelectorOpIn, "1", "5", "9")),
			}},
			map[string][]string{testGenerationKey: {"5", "9"}},
		),
	)

	It("should not let one instance type's requirements leak into another", func() {
		nodePool := testNodePool("np", []v1.NodeSelectorRequirementWithMinValues{{
			Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"zone-a", "zone-b", "zone-c"},
		}}, nil, nil)
		instanceTypes := []*cloudprovider.InstanceType{
			testInstanceType("it-a", scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-a")),
			testInstanceType("it-b", scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-b")),
			testInstanceType("it-c", scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone-c")),
		}
		itsByNodePool := map[string][]*cloudprovider.InstanceType{"np": instanceTypes}

		domainGroups := expectMatchesReference([]*v1.NodePool{nodePool}, itsByNodePool)
		Expect(domainsByKey(domainGroups)).To(Equal(map[string][]string{
			corev1.LabelTopologyZone: {"zone-a", "zone-b", "zone-c"},
		}))

		Expect(nodePool.Spec.Template.Spec.Requirements[0].Values).To(ConsistOf("zone-a", "zone-b", "zone-c"))
		for i, zone := range []string{"zone-a", "zone-b", "zone-c"} {
			Expect(instanceTypes[i].Requirements.Get(corev1.LabelTopologyZone).Values()).To(ConsistOf(zone))
		}
	})

	It("should be idempotent across repeated calls on the same input", func() {
		nodePools, itsByNodePool := randomDomainGroupInput(rand.New(rand.NewPCG(1, 2))) //nolint:gosec // deterministic seed for reproducible test inputs
		first := normalizeDomainGroups(buildDomainGroups(nodePools, itsByNodePool))
		for range 3 {
			Expect(normalizeDomainGroups(buildDomainGroups(nodePools, itsByNodePool))).To(Equal(first))
		}
	})

	It("should match the pre-hoist implementation over randomized inputs", func() {
		random := rand.New(rand.NewPCG(0x5EED, 0x1234)) //nolint:gosec // deterministic seed for reproducible test inputs
		for range 200 {
			nodePools, itsByNodePool := randomDomainGroupInput(random)
			expectMatchesReference(nodePools, itsByNodePool)
		}
	})
})

// randomDomainGroupInput uses a small key and value space so requirements collide often.
func randomDomainGroupInput(random *rand.Rand) ([]*v1.NodePool, map[string][]*cloudprovider.InstanceType) {
	keys := []string{corev1.LabelTopologyZone, corev1.LabelArchStable, corev1.LabelOSStable, testPoolKey, testGenerationKey}
	setOperators := []corev1.NodeSelectorOperator{
		corev1.NodeSelectorOpIn,
		corev1.NodeSelectorOpNotIn,
		corev1.NodeSelectorOpExists,
		corev1.NodeSelectorOpDoesNotExist,
	}
	boundsOperators := []corev1.NodeSelectorOperator{
		corev1.NodeSelectorOpGt,
		corev1.NodeSelectorOpLt,
		v1.NodeSelectorOpGte,
		v1.NodeSelectorOpLte,
	}
	operatorsFor := func(key string) []corev1.NodeSelectorOperator {
		if key == testGenerationKey {
			return append(append([]corev1.NodeSelectorOperator{}, setOperators...), boundsOperators...)
		}
		return setOperators
	}
	isBounds := func(operator corev1.NodeSelectorOperator) bool {
		return slices.Contains(boundsOperators, operator)
	}
	valuesFor := func(key string) []string {
		if key == testGenerationKey {
			return []string{"1", "3", "5", "9"}
		}
		return []string{"a", "b", "c", "d"}
	}
	randomValues := func(key string) []string {
		space := valuesFor(key)
		picked := map[string]struct{}{}
		for range random.IntN(len(space)) + 1 {
			picked[space[random.IntN(len(space))]] = struct{}{}
		}
		values := lo.Keys(picked)
		sort.Strings(values)
		return values
	}

	nodePools := make([]*v1.NodePool, 0, 3)
	itsByNodePool := map[string][]*cloudprovider.InstanceType{}
	for npIndex := range random.IntN(3) + 1 {
		var requirements []v1.NodeSelectorRequirementWithMinValues
		for _, key := range keys {
			if random.IntN(3) == 0 {
				continue
			}
			legal := operatorsFor(key)
			operator := legal[random.IntN(len(legal))]
			requirement := v1.NodeSelectorRequirementWithMinValues{Key: key, Operator: operator}
			switch {
			case isBounds(operator):
				space := valuesFor(key)
				requirement.Values = []string{space[random.IntN(len(space))]}
			case operator == corev1.NodeSelectorOpIn || operator == corev1.NodeSelectorOpNotIn:
				requirement.Values = randomValues(key)
				if random.IntN(2) == 0 {
					requirement.MinValues = new(random.IntN(len(requirement.Values)) + 1)
				}
			}
			requirements = append(requirements, requirement)
		}
		labels := map[string]string{}
		if random.IntN(2) == 0 {
			key := keys[random.IntN(len(keys))]
			space := valuesFor(key)
			labels[key] = space[random.IntN(len(space))]
		}
		var taints []corev1.Taint
		if random.IntN(2) == 0 {
			taints = []corev1.Taint{{
				Key:    fmt.Sprintf("example.com/taint-%d", random.IntN(2)),
				Value:  "true",
				Effect: corev1.TaintEffectNoSchedule,
			}}
		}
		name := fmt.Sprintf("np-%d", npIndex)
		nodePools = append(nodePools, testNodePool(name, requirements, labels, taints))

		instanceTypes := make([]*cloudprovider.InstanceType, 0, 4)
		for itIndex := range random.IntN(4) + 1 {
			var instanceTypeRequirements []*scheduling.Requirement
			for _, key := range keys {
				if random.IntN(3) == 0 {
					continue
				}
				legal := operatorsFor(key)
				operator := legal[random.IntN(len(legal))]
				switch {
				case isBounds(operator):
					space := valuesFor(key)
					instanceTypeRequirements = append(instanceTypeRequirements, scheduling.NewRequirement(key, operator, space[random.IntN(len(space))]))
				case operator == corev1.NodeSelectorOpIn || operator == corev1.NodeSelectorOpNotIn:
					values := randomValues(key)
					var minValues *int
					if random.IntN(2) == 0 {
						minValues = new(random.IntN(len(values)) + 1)
					}
					instanceTypeRequirements = append(instanceTypeRequirements, scheduling.NewRequirementWithFlexibility(key, operator, minValues, values...))
				default:
					instanceTypeRequirements = append(instanceTypeRequirements, scheduling.NewRequirement(key, operator))
				}
			}
			instanceTypes = append(instanceTypes, testInstanceType(fmt.Sprintf("%s-it-%d", name, itIndex), instanceTypeRequirements...))
		}
		itsByNodePool[name] = instanceTypes
	}
	return nodePools, itsByNodePool
}
