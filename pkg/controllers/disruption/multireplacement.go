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
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	minMultiNodeMultiReplacementSources = 3
	maxMultiNodeMultiReplacements       = 2
)

var (
	errMultiNodeMultiReplacementIneligible = errors.New("multi-node multi-replacement is ineligible")
	errMultiNodeMultiReplacementPrice      = errors.New("multi-node multi-replacement is not cheaper")
)

func prepareMultiNodeMultiReplacement(candidates []*Candidate, nodeClaims []*pscheduling.NodeClaim) ([]*pscheduling.NodeClaim, error) {
	if err := validateMultiNodeMultiReplacementBoundary(candidates, nodeClaims, false); err != nil {
		return nil, err
	}

	sourceInstanceType := candidates[0].instanceType.Name
	sourceZone := candidates[0].zone
	for _, nodeClaim := range nodeClaims {
		nodeClaim.Requirements.Add(
			scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, sourceZone),
		)
		nodeClaim.InstanceTypeOptions = nodeClaim.InstanceTypeOptions.Compatible(nodeClaim.Requirements)
		nodeClaim.InstanceTypeOptions = filterInstanceTypes(nodeClaim.InstanceTypeOptions, func(instanceType *cloudprovider.InstanceType) bool {
			return instanceType.Name != sourceInstanceType
		})
	}
	if err := validateMultiNodeMultiReplacementBoundary(candidates, nodeClaims, true); err != nil {
		return nil, err
	}
	return filterMultiNodeMultiReplacementByPrice(nodeClaims, sumCandidatePrices(candidates))
}

//nolint:gocyclo
func validateMultiNodeMultiReplacementBoundary(candidates []*Candidate, nodeClaims []*pscheduling.NodeClaim, requireReplacementConstraints bool) error {
	if len(candidates) < minMultiNodeMultiReplacementSources {
		return fmt.Errorf("%w, requires at least %d source nodes", errMultiNodeMultiReplacementIneligible, minMultiNodeMultiReplacementSources)
	}
	if len(nodeClaims) != maxMultiNodeMultiReplacements {
		return fmt.Errorf("%w, requires exactly %d replacement nodeclaims", errMultiNodeMultiReplacementIneligible, maxMultiNodeMultiReplacements)
	}

	first := candidates[0]
	if first.NodePool == nil || first.OwnedByStaticNodePool() || first.instanceType == nil || first.capacityType != v1.CapacityTypeOnDemand || first.zone == "" {
		return fmt.Errorf("%w, source nodes must be dynamic, homogeneous, and on-demand", errMultiNodeMultiReplacementIneligible)
	}
	for _, candidate := range candidates[1:] {
		if candidate.NodePool == nil ||
			candidate.OwnedByStaticNodePool() ||
			candidate.NodePool.Name != first.NodePool.Name ||
			candidate.NodePool.UID != first.NodePool.UID ||
			candidate.instanceType == nil ||
			candidate.instanceType.Name != first.instanceType.Name ||
			candidate.capacityType != v1.CapacityTypeOnDemand ||
			candidate.zone != first.zone {
			return fmt.Errorf("%w, source nodes must share a dynamic nodepool, instance type, zone, and on-demand capacity type", errMultiNodeMultiReplacementIneligible)
		}
	}
	for _, nodeClaim := range nodeClaims {
		if nodeClaim == nil ||
			nodeClaim.IsStaticNodeClaim ||
			nodeClaim.NodePoolName != first.NodePool.Name ||
			nodeClaim.NodePoolUUID != first.NodePool.UID {
			return fmt.Errorf("%w, replacements must use source nodepool %q", errMultiNodeMultiReplacementIneligible, first.NodePool.Name)
		}
		if requireReplacementConstraints {
			capacityTypeRequirement := nodeClaim.Requirements.Get(v1.CapacityTypeLabelKey)
			zoneRequirement := nodeClaim.Requirements.Get(corev1.LabelTopologyZone)
			if capacityTypeRequirement.Len() != 1 ||
				!capacityTypeRequirement.Has(v1.CapacityTypeOnDemand) ||
				zoneRequirement.Len() != 1 ||
				!zoneRequirement.Has(first.zone) {
				return fmt.Errorf("%w, replacements must be on-demand in source zone %q", errMultiNodeMultiReplacementIneligible, first.zone)
			}
		}
	}
	return nil
}

func filterInstanceTypes(instanceTypes cloudprovider.InstanceTypes, keep func(*cloudprovider.InstanceType) bool) cloudprovider.InstanceTypes {
	filtered := make(cloudprovider.InstanceTypes, 0, len(instanceTypes))
	for _, instanceType := range instanceTypes {
		if keep(instanceType) {
			filtered = append(filtered, instanceType)
		}
	}
	return filtered
}

type pricedInstanceTypePrefix struct {
	instanceTypes cloudprovider.InstanceTypes
	maxPrice      float64
}

//nolint:gocyclo
func filterMultiNodeMultiReplacementByPrice(nodeClaims []*pscheduling.NodeClaim, sourcePrice float64) ([]*pscheduling.NodeClaim, error) {
	if len(nodeClaims) != maxMultiNodeMultiReplacements || sourcePrice <= 0 || math.IsNaN(sourcePrice) || math.IsInf(sourcePrice, 0) {
		return nil, fmt.Errorf("%w, invalid source or replacement count", errMultiNodeMultiReplacementPrice)
	}

	prefixes := make([][]pricedInstanceTypePrefix, len(nodeClaims))
	for i, nodeClaim := range nodeClaims {
		instanceTypes := append(cloudprovider.InstanceTypes{}, nodeClaim.InstanceTypeOptions...)
		sort.SliceStable(instanceTypes, func(a, b int) bool {
			return worstCaseInstanceTypePrice(instanceTypes[a], nodeClaim.Requirements) < worstCaseInstanceTypePrice(instanceTypes[b], nodeClaim.Requirements)
		})
		for end := 1; end <= len(instanceTypes); end++ {
			prefix := instanceTypes[:end]
			maxPrice := worstCaseInstanceTypePrice(prefix[len(prefix)-1], nodeClaim.Requirements)
			if maxPrice == math.MaxFloat64 {
				continue
			}
			if _, _, err := prefix.SatisfiesMinValues(nodeClaim.Requirements); err != nil {
				continue
			}
			prefixes[i] = append(prefixes[i], pricedInstanceTypePrefix{
				instanceTypes: append(cloudprovider.InstanceTypes{}, prefix...),
				maxPrice:      maxPrice,
			})
		}
	}

	bestLeft, bestRight := -1, -1
	bestFlexibility := -1
	bestPrice := math.MaxFloat64
	for left, leftPrefix := range prefixes[0] {
		for right, rightPrefix := range prefixes[1] {
			aggregatePrice := leftPrefix.maxPrice + rightPrefix.maxPrice
			if aggregatePrice >= sourcePrice {
				continue
			}
			flexibility := len(leftPrefix.instanceTypes) + len(rightPrefix.instanceTypes)
			if flexibility > bestFlexibility || (flexibility == bestFlexibility && aggregatePrice < bestPrice) {
				bestLeft, bestRight = left, right
				bestFlexibility = flexibility
				bestPrice = aggregatePrice
			}
		}
	}
	if bestLeft == -1 {
		return nil, fmt.Errorf("%w, aggregate replacement price must be strictly below source price", errMultiNodeMultiReplacementPrice)
	}

	nodeClaims[0].InstanceTypeOptions = prefixes[0][bestLeft].instanceTypes
	nodeClaims[1].InstanceTypeOptions = prefixes[1][bestRight].instanceTypes
	return nodeClaims, nil
}

func worstCaseInstanceTypePrice(instanceType *cloudprovider.InstanceType, requirements scheduling.Requirements) float64 {
	if instanceType == nil {
		return math.MaxFloat64
	}
	price := instanceType.Offerings.Available().WorstLaunchPrice(requirements)
	if price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return math.MaxFloat64
	}
	return price
}

//nolint:gocyclo
func aggregateWorstCaseReplacementPrice(replacements []*Replacement, simulated []*pscheduling.NodeClaim) float64 {
	matched, ok := matchReplacementPairs(replacements, simulated)
	if !ok {
		return math.MaxFloat64
	}
	total := 0.0
	for i, replacement := range replacements {
		if replacement == nil || replacement.NodeClaim == nil || len(replacement.InstanceTypeOptions) == 0 {
			return math.MaxFloat64
		}
		refreshedByName := make(map[string]*cloudprovider.InstanceType, len(matched[i].InstanceTypeOptions))
		for _, instanceType := range matched[i].InstanceTypeOptions {
			refreshedByName[instanceType.Name] = instanceType
		}
		maxPrice := 0.0
		for _, instanceType := range replacement.InstanceTypeOptions {
			refreshed, ok := refreshedByName[instanceType.Name]
			if !ok {
				return math.MaxFloat64
			}
			price := worstCaseInstanceTypePrice(refreshed, replacement.Requirements)
			if price > maxPrice {
				maxPrice = price
			}
		}
		if maxPrice == math.MaxFloat64 || math.IsNaN(maxPrice) || math.IsInf(maxPrice, 0) {
			return math.MaxFloat64
		}
		total += maxPrice
	}
	return total
}

func replacementPairsMatch(replacements []*Replacement, simulated []*pscheduling.NodeClaim) bool {
	_, ok := matchReplacementPairs(replacements, simulated)
	return ok
}

func matchReplacementPairs(replacements []*Replacement, simulated []*pscheduling.NodeClaim) ([]*pscheduling.NodeClaim, bool) {
	if len(replacements) != maxMultiNodeMultiReplacements || len(simulated) != maxMultiNodeMultiReplacements {
		return nil, false
	}
	if replacementMatches(replacements[0], simulated[0]) && replacementMatches(replacements[1], simulated[1]) {
		return simulated, true
	}
	if replacementMatches(replacements[0], simulated[1]) && replacementMatches(replacements[1], simulated[0]) {
		return []*pscheduling.NodeClaim{simulated[1], simulated[0]}, true
	}
	return nil, false
}

func replacementMatches(replacement *Replacement, simulated *pscheduling.NodeClaim) bool {
	return replacement != nil &&
		replacement.NodeClaim != nil &&
		simulated != nil &&
		replacement.NodePoolName == simulated.NodePoolName &&
		replacement.NodePoolUUID == simulated.NodePoolUUID &&
		replacement.NodePoolWeight == simulated.NodePoolWeight &&
		replacement.IsStaticNodeClaim == simulated.IsStaticNodeClaim &&
		instanceTypesAreSubset(replacement.InstanceTypeOptions, simulated.InstanceTypeOptions) &&
		nodeClaimTemplateMatches(replacement.NodeClaim, simulated) &&
		podGroupsMatch(replacement.Pods, simulated.Pods)
}

func nodeClaimTemplateMatches(replacement, simulated *pscheduling.NodeClaim) bool {
	replacementAPI := replacement.DeepCopy()
	simulatedAPI := simulated.DeepCopy()
	replacementAPI.Spec.Requirements = nil
	simulatedAPI.Spec.Requirements = nil
	return reflect.DeepEqual(replacementAPI.Labels, simulatedAPI.Labels) &&
		reflect.DeepEqual(replacementAPI.Annotations, simulatedAPI.Annotations) &&
		reflect.DeepEqual(replacementAPI.Spec, simulatedAPI.Spec) &&
		reflect.DeepEqual(normalizedRequirements(replacement.Requirements), normalizedRequirements(simulated.Requirements))
}

func normalizedRequirements(requirements scheduling.Requirements) []v1.NodeSelectorRequirementWithMinValues {
	normalized := requirements.NodeSelectorRequirements()
	normalized = filterRequirements(normalized, func(requirement v1.NodeSelectorRequirementWithMinValues) bool {
		return requirement.Key != corev1.LabelHostname
	})
	for i := range normalized {
		sort.Strings(normalized[i].Values)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Key != normalized[j].Key {
			return normalized[i].Key < normalized[j].Key
		}
		if normalized[i].Operator != normalized[j].Operator {
			return normalized[i].Operator < normalized[j].Operator
		}
		return strings.Join(normalized[i].Values, "\x00") < strings.Join(normalized[j].Values, "\x00")
	})
	return normalized
}

func filterRequirements(requirements []v1.NodeSelectorRequirementWithMinValues, keep func(v1.NodeSelectorRequirementWithMinValues) bool) []v1.NodeSelectorRequirementWithMinValues {
	filtered := make([]v1.NodeSelectorRequirementWithMinValues, 0, len(requirements))
	for _, requirement := range requirements {
		if keep(requirement) {
			filtered = append(filtered, requirement)
		}
	}
	return filtered
}

func podGroupsMatch(replacementPods, simulatedPods []*corev1.Pod) bool {
	return reflect.DeepEqual(podGroupKeys(replacementPods), podGroupKeys(simulatedPods))
}

func podGroupKeys(pods []*corev1.Pod) []string {
	keys := make([]string, 0, len(pods))
	for _, pod := range pods {
		if pod.UID != "" {
			keys = append(keys, string(pod.UID))
		} else {
			keys = append(keys, pod.Namespace+"/"+pod.Name)
		}
	}
	sort.Strings(keys)
	return keys
}
