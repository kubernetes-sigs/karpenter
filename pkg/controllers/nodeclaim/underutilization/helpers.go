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

package underutilization

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const MinInstanceTypesForSpotToSpotConsolidation = 15

// Compute command to execute spot-to-spot consolidation if:
//  1. The SpotToSpotConsolidation feature flag is set to true.
//  2. For single-node consolidation:
//     a. There are at least 15 cheapest instance type replacement options to consolidate.
//     b. The current candidate is NOT part of the first 15 cheapest instance types inorder to avoid repeated consolidation.
func (c *Controller) validateSpotToSpotConsolidation(ctx context.Context, candidates []*state.StateNode, results pscheduling.Results,
	candidatePrice float64) (bool, error) {

	// Spot consolidation is turned off.
	if !options.FromContext(ctx).FeatureGates.SpotToSpotConsolidation {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "SpotToSpotConsolidation is disabled, can't replace a spot node with a spot node")...)
		}
		return false, nil
	}

	// Since we are sure that the replacement nodeclaim considered for the spot candidates are spot, we will enforce it through the requirements.
	results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, v1beta1.CapacityTypeSpot))

	var err error
	results.NewNodeClaims[0], err = results.NewNodeClaims[0].RemoveInstanceTypeOptionsByPriceAndMinValues(results.NewNodeClaims[0].Requirements, candidatePrice)

	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) == 0 {
		if len(candidates) == 1 {
			if err != nil {
				c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("minValues requirement not met, %s", err.Error()))...)
			} else {
				c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "Can't replace spot node with a cheaper spot node")...)
			}
		}
		// no instance types remain after filtering by price
		return false, nil
	}

	// For multi-node consolidation:
	// We don't have any requirement to check the remaining instance type flexibility, so exit early in this case.
	if len(candidates) > 1 {
		return true, nil
	}

	// For single-node consolidation:

	// We check whether we have 15 cheaper instances than the current candidate instance. If this is the case, we know the following things:
	//   1) The current candidate is not in the set of the 15 cheapest instance types and
	//   2) There were at least 15 options cheaper than the current candidate.
	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) < MinInstanceTypesForSpotToSpotConsolidation {
		c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("SpotToSpotConsolidation requires %d cheaper instance type options than the current candidate to consolidate, got %d",
			MinInstanceTypesForSpotToSpotConsolidation, len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions)))...)
		return false, nil
	}

	return true, nil
}
