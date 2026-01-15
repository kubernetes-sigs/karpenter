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
	"fmt"
	"sort"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// consolidationTTL is the TTL between creating a consolidation command and validating that it still works.
const consolidationTTL = 15 * time.Second

// MinInstanceTypesForSpotToSpotConsolidation is the minimum number of instanceTypes in a NodeClaim needed to trigger spot-to-spot single-node consolidation
const MinInstanceTypesForSpotToSpotConsolidation = 15

// consolidation is the base consolidation controller that provides common functionality used across the different
// consolidation methods.
type consolidation struct {
	// Consolidation needs to be aware of the queue for validation
	queue                  *Queue
	clock                  clock.Clock
	cluster                *state.Cluster
	kubeClient             client.Client
	provisioner            *provisioning.Provisioner
	cloudProvider          cloudprovider.CloudProvider
	recorder               events.Recorder
	lastConsolidationState time.Time
}

func MakeConsolidation(clock clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cloudProvider cloudprovider.CloudProvider, recorder events.Recorder, queue *Queue) consolidation {
	return consolidation{
		queue:         queue,
		clock:         clock,
		cluster:       cluster,
		kubeClient:    kubeClient,
		provisioner:   provisioner,
		cloudProvider: cloudProvider,
		recorder:      recorder,
	}
}

// IsConsolidated returns true if nothing has changed since markConsolidated was called.
func (c *consolidation) IsConsolidated() bool {
	return c.lastConsolidationState.Equal(c.cluster.ConsolidationState())
}

// markConsolidated records the current state of the cluster.
func (c *consolidation) markConsolidated() {
	c.lastConsolidationState = c.cluster.ConsolidationState()
}

// ShouldDisrupt is a predicate used to filter candidates
func (c *consolidation) ShouldDisrupt(_ context.Context, cn *Candidate) bool {
	// Disable consolidation for static NodePool
	if cn.OwnedByStaticNodePool() {
		return false
	}
	// We need the following to know what the price of the instance for price comparison. If one of these doesn't exist, we can't
	// compute consolidation decisions for this candidate.
	// 1. Instance Type
	// 2. Capacity Type
	// 3. Zone
	if cn.instanceType == nil {
		c.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("Instance Type %q not found", cn.Labels()[corev1.LabelInstanceTypeStable]))...)
		return false
	}
	if _, ok := cn.Labels()[v1.CapacityTypeLabelKey]; !ok {
		c.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("Node does not have label %q", v1.CapacityTypeLabelKey))...)
		return false
	}
	if _, ok := cn.Labels()[corev1.LabelTopologyZone]; !ok {
		c.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("Node does not have label %q", corev1.LabelTopologyZone))...)
		return false
	}
	if cn.NodePool.Spec.Disruption.ConsolidateAfter.Duration == nil {
		c.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("NodePool %q has consolidation disabled", cn.NodePool.Name))...)
		return false
	}
	// If we don't have the "WhenEmptyOrUnderutilized" policy set, we should not do any of the consolidation methods, but
	// we should also not fire an event here to users since this can be confusing when the field on the NodePool
	// is named "consolidationPolicy"
	if cn.NodePool.Spec.Disruption.ConsolidationPolicy != v1.ConsolidationPolicyWhenEmptyOrUnderutilized {
		c.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("NodePool %q has non-empty consolidation disabled", cn.NodePool.Name))...)
		return false
	}
	// return true if consolidatable
	return cn.NodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable).IsTrue()
}

// sortCandidates sorts candidates by disruption cost (where the lowest disruption cost is first) and returns the result
func (c *consolidation) sortCandidates(candidates []*Candidate) []*Candidate {
	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].DisruptionCost < candidates[j].DisruptionCost
	})
	return candidates
}

// computeConsolidation computes a consolidation action to take
//
// nolint:gocyclo
func (c *consolidation) computeConsolidation(ctx context.Context, candidates ...*Candidate) (Command, error) {
	var err error
	// Run scheduling simulation to compute consolidation option
	results, err := SimulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, candidates...)
	if err != nil {
		// if a candidate node is now deleting, just retry
		if errors.Is(err, errCandidateDeleting) {
			return Command{}, nil
		}
		return Command{}, err
	}

	// if not all of the pods were scheduled, we can't do anything
	if !results.AllNonPendingPodsScheduled() {
		// This method is used by multi-node consolidation as well, so we'll only report in the single node case
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, pretty.Sentence(results.NonPendingPodSchedulingErrors()))...)
		}
		return Command{}, nil
	}

	// were we able to schedule all the pods on the inflight candidates?
	if len(results.NewNodeClaims) == 0 {
		return Command{
			Candidates: candidates,
			Results:    results,
		}, nil
	}

	// we're not going to turn a single node into multiple candidates
	if len(results.NewNodeClaims) != 1 {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("Can't remove without creating %d candidates", len(results.NewNodeClaims)))...)
		}
		return Command{}, nil
	}

	// get the current node price based on the offering
	// fallback if we can't find the specific zonal pricing data
	candidatePrice, err := getCandidatePrices(candidates)
	if err != nil {
		return Command{}, fmt.Errorf("getting offering price from candidate node, %w", err)
	}

	allExistingAreSpot := true
	for _, cn := range candidates {
		if cn.capacityType != v1.CapacityTypeSpot {
			allExistingAreSpot = false
		}
	}

	// sort the instanceTypes by price before we take any actions like truncation for spot-to-spot consolidation or finding the nodeclaim
	// that meets the minimum requirement after filteringByPrice
	results.NewNodeClaims[0].InstanceTypeOptions = results.NewNodeClaims[0].InstanceTypeOptions.OrderByPrice(results.NewNodeClaims[0].Requirements)

	if allExistingAreSpot &&
		results.NewNodeClaims[0].Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeSpot) {
		return c.computeSpotToSpotConsolidation(ctx, candidates, results, candidatePrice)
	}

	// Apply price filtering to ensure cost savings requirements are met
	if !c.filterByPrice(ctx, candidates, results, candidatePrice) {
		return Command{}, nil
	}

	// We are consolidating a node from OD -> [OD,Spot] but have filtered the instance types by cost based on the
	// assumption, that the spot variant will launch. We also need to add a requirement to the node to ensure that if
	// spot capacity is insufficient we don't replace the node with a more expensive on-demand node.  Instead the launch
	// should fail and we'll just leave the node alone. We don't need to do the same for reserved since the requirements
	// are injected on by the scheduler.
	ctReq := results.NewNodeClaims[0].Requirements.Get(v1.CapacityTypeLabelKey)
	if ctReq.Has(v1.CapacityTypeSpot) && ctReq.Has(v1.CapacityTypeOnDemand) {
		results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeSpot))
	}

	return Command{
		Candidates:   candidates,
		Replacements: replacementsFromNodeClaims(results.NewNodeClaims...),
		Results:      results,
	}, nil
}

// Compute command to execute spot-to-spot consolidation if:
//  1. The SpotToSpotConsolidation feature flag is set to true.
//  2. For single-node consolidation:
//     a. There are at least 15 cheapest instance type replacement options to consolidate.
//     b. The current candidate is NOT part of the first 15 cheapest instance types inorder to avoid repeated consolidation.
func (c *consolidation) computeSpotToSpotConsolidation(ctx context.Context, candidates []*Candidate, results pscheduling.Results, candidatePrice float64) (Command, error) {

	// Spot consolidation is turned off.
	if !options.FromContext(ctx).FeatureGates.SpotToSpotConsolidation {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "SpotToSpotConsolidation is disabled, can't replace a spot node with a spot node")...)
		}
		return Command{}, nil
	}

	// Since we are sure that the replacement nodeclaim considered for the spot candidates are spot, we will enforce it through the requirements.
	results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeSpot))
	// All possible replacements for the current candidate compatible with spot offerings
	results.NewNodeClaims[0].InstanceTypeOptions = results.NewNodeClaims[0].InstanceTypeOptions.Compatible(results.NewNodeClaims[0].Requirements)

	// Apply price filtering to ensure cost savings requirements are met
	if !c.filterByPrice(ctx, candidates, results, candidatePrice) {
		return Command{}, nil
	}

	// For multi-node consolidation:
	// We don't have any requirement to check the remaining instance type flexibility, so exit early in this case.
	if len(candidates) > 1 {
		return Command{
			Candidates:   candidates,
			Replacements: replacementsFromNodeClaims(results.NewNodeClaims...),
			Results:      results,
		}, nil
	}

	// For single-node consolidation:

	// We check whether we have 15 cheaper instances than the current candidate instance. If this is the case, we know the following things:
	//   1) The current candidate is not in the set of the 15 cheapest instance types and
	//   2) There were at least 15 options cheaper than the current candidate.
	if len(results.NewNodeClaims[0].InstanceTypeOptions) < MinInstanceTypesForSpotToSpotConsolidation {
		c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("SpotToSpotConsolidation requires %d cheaper instance type options than the current candidate to consolidate, got %d",
			MinInstanceTypesForSpotToSpotConsolidation, len(results.NewNodeClaims[0].InstanceTypeOptions)))...)
		return Command{}, nil
	}

	// If a user has minValues set in their NodePool requirements, then we cap the number of instancetypes at 100 which would be the actual number of instancetypes sent for launch to enable spot-to-spot consolidation.
	// If no minValues in the NodePool requirement, then we follow the default 15 to cap the instance types for launch to enable a spot-to-spot consolidation.
	// Restrict the InstanceTypeOptions for launch to 15(if default) so we don't get into a continual consolidation situation.
	// For example:
	// 1) Suppose we have 5 instance types, (A, B, C, D, E) in order of price with the minimum flexibility 3 and they’ll all work for our pod.  We send CreateInstanceFromTypes(A,B,C,D,E) and it gives us a E type based on price and availability of spot.
	// 2) We check if E is part of (A,B,C,D) and it isn't, so we will immediately have consolidation send a CreateInstanceFromTypes(A,B,C,D), since they’re cheaper than E.
	// 3) Assuming CreateInstanceFromTypes(A,B,C,D) returned D, we check if D is part of (A,B,C) and it isn't, so will have another consolidation send a CreateInstanceFromTypes(A,B,C), since they’re cheaper than D resulting in continual consolidation.
	// If we had restricted instance types to min flexibility at launch at step (1) i.e CreateInstanceFromTypes(A,B,C), we would have received the instance type part of the list preventing immediate consolidation.
	// Taking this to 15 types, we need to only send the 15 cheapest types in the CreateInstanceFromTypes call so that the resulting instance is always in that set of 15 and we won’t immediately consolidate.
	if results.NewNodeClaims[0].Requirements.HasMinValues() {
		// Here we are trying to get the max of the minimum instances required to satisfy the minimum requirement and the default 15 to cap the instances for spot-to-spot consolidation.
		minInstanceTypes, _, _ := results.NewNodeClaims[0].InstanceTypeOptions.SatisfiesMinValues(results.NewNodeClaims[0].Requirements)
		results.NewNodeClaims[0].InstanceTypeOptions = lo.Slice(results.NewNodeClaims[0].InstanceTypeOptions, 0, lo.Max([]int{MinInstanceTypesForSpotToSpotConsolidation, minInstanceTypes}))
	} else {
		results.NewNodeClaims[0].InstanceTypeOptions = lo.Slice(results.NewNodeClaims[0].InstanceTypeOptions, 0, MinInstanceTypesForSpotToSpotConsolidation)
	}

	return Command{
		Candidates:   candidates,
		Replacements: replacementsFromNodeClaims(results.NewNodeClaims...),
		Results:      results,
	}, nil
}

// getCandidatePrices returns the sum of the prices of the given candidates
func getCandidatePrices(candidates []*Candidate) (float64, error) {
	var price float64
	for _, c := range candidates {
		reqs := scheduling.NewLabelRequirements(c.Labels())
		compatibleOfferings := c.instanceType.Offerings.Compatible(reqs)
		if len(compatibleOfferings) == 0 {
			// It's expected that offerings may no longer exist for capacity reservations once a NodeClass stops selecting on
			// them (or they are no longer considered for some other reason on by the cloudprovider). By definition though,
			// reserved capacity is free. By modeling it as free, consolidation won't be able to succeed, but the node should be
			// disrupted via drift regardless.
			if reqs.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeReserved) {
				return 0.0, nil
			}
			return 0.0, serrors.Wrap(fmt.Errorf("unable to determine offering"), "instance-type", c.instanceType.Name, "capacity-type", c.capacityType, "zone", c.zone)
		}
		price += compatibleOfferings.Cheapest().Price
	}
	return price, nil
}

// parsePriceFactorFromNodePool parses and validates the consolidation price improvement percentage from a NodePool.
// Converts percentage (0-100) to factor (0.0-1.0) using: factor = 1.0 - (percentage / 100.0)
// Returns (factor, true) if valid, (0.0, false) if invalid or out of range [0, 100].
func parsePriceFactorFromNodePool(ctx context.Context, nodePool *v1.NodePool) (float64, bool) {
	if nodePool == nil || nodePool.Spec.Disruption.ConsolidationPriceImprovementPercentage == nil {
		return 0.0, false
	}

	pct := *nodePool.Spec.Disruption.ConsolidationPriceImprovementPercentage

	if pct < 0 || pct > 100 {
		log.FromContext(ctx).Info("NodePool consolidationPriceImprovementPercentage out of range, skipping",
			"nodePool", nodePool.Name, "value", pct)
		return 0.0, false
	}

	// Convert percentage to factor: factor = 1.0 - (percentage / 100.0)
	// Examples: 10% → 0.9, 20% → 0.8, 0% → 1.0, 100% → 0.0
	factor := 1.0 - (float64(pct) / 100.0)
	return factor, true
}

// getMinFactorFromCandidates finds the minimum (most conservative) valid price improvement factor
// across all candidates. Returns (factor, nodePoolName) if found, (nil, "") if none found.
func getMinFactorFromCandidates(ctx context.Context, candidates []*Candidate) (*float64, string) {
	var minFactor *float64
	var minFactorNodePool string

	for _, candidate := range candidates {
		factor, valid := parsePriceFactorFromNodePool(ctx, candidate.NodePool)
		if !valid {
			continue
		}

		if minFactor == nil || factor < *minFactor {
			minFactor = &factor
			minFactorNodePool = candidate.NodePool.Name
		}
	}

	return minFactor, minFactorNodePool
}

// filterByPrice returns the instanceTypes that are lower priced than the current candidate and any error that indicates the input couldn't be filtered.
// Apply the price improvement factor to require cost savings threshold before consolidation.
// Returns true if filtering succeeded, false if it failed (caller should not proceed with consolidation).
// getPriceImprovementFactor resolves the price improvement factor with the following precedence:
// 1. NodePool-level (if set and valid) - for multi-node consolidation, uses the most conservative (minimum) factor
// 2. Operator-level
// 3. Default (1.0)
// Returns: (factor, factorSource, nodePoolName) where factorSource is "nodepool" or "operator"
func (c *consolidation) getPriceImprovementFactor(ctx context.Context, candidates []*Candidate) (float64, string, string) {
	// Try to find the minimum (most conservative) factor from candidates
	minFactor, minFactorNodePool := getMinFactorFromCandidates(ctx, candidates)

	if minFactor != nil {
		log.FromContext(ctx).V(1).Info("Using most conservative NodePool-level consolidation price improvement factor",
			"nodePool", minFactorNodePool, "value", *minFactor, "candidateCount", len(candidates))
		return *minFactor, "nodepool", minFactorNodePool
	}

	// Fall back to operator-level setting
	// Convert percentage (0-100) to factor (0.0-1.0): factor = 1.0 - (percentage / 100.0)
	operatorPct := options.FromContext(ctx).ConsolidationPriceImprovementPercentage
	operatorFactor := 1.0 - (float64(operatorPct) / 100.0)
	log.FromContext(ctx).V(1).Info("Using operator-level consolidation price improvement factor",
		"percentage", operatorPct, "factor", operatorFactor, "candidateCount", len(candidates))
	return operatorFactor, "operator", ""
}

// calculateSavingsPercentage calculates the cost savings percentage for consolidation.
// Returns the savings as a decimal (e.g., 0.10 for 10% savings).
// Returns 0.0 if replacement is more expensive than current.
func calculateSavingsPercentage(currentPrice, replacementPrice float64) float64 {
	if currentPrice <= 0.0 {
		return 0.0
	}
	if replacementPrice >= currentPrice {
		return 0.0
	}
	return (currentPrice - replacementPrice) / currentPrice
}

// getCheapestReplacementPrice extracts the cheapest replacement price from a NodeClaim's instance type options.
// Returns 0.0 if no valid price is found.
func getCheapestReplacementPrice(newNodeClaim *pscheduling.NodeClaim) float64 {
	if len(newNodeClaim.InstanceTypeOptions) == 0 {
		return 0.0
	}

	orderedTypes := newNodeClaim.InstanceTypeOptions.OrderByPrice(newNodeClaim.Requirements)
	if len(orderedTypes) == 0 {
		return 0.0
	}

	cheapestOffering := orderedTypes[0].Offerings.Cheapest()
	if cheapestOffering != nil && cheapestOffering.Available {
		return cheapestOffering.Price
	}

	return 0.0
}

// recordPriceFilterMetrics records all metrics related to price filtering for consolidation.
func recordPriceFilterMetrics(blocked bool, actualSavingsPct, requiredSavingsPct, priceImprovementFactor float64, factorSource, nodePoolName string) {
	ConsolidationPriceSavingsActual.Observe(actualSavingsPct, map[string]string{
		"blocked":       fmt.Sprintf("%t", blocked),
		"factor_source": factorSource,
	})
	ConsolidationPriceSavingsRequired.Observe(requiredSavingsPct, map[string]string{
		"blocked":       fmt.Sprintf("%t", blocked),
		"factor_source": factorSource,
	})
	ConsolidationPriceFactorUsed.Set(priceImprovementFactor, map[string]string{
		"factor_source": factorSource,
		"nodepool":      nodePoolName,
	})
}

// handleBlockedConsolidation handles the case when consolidation is blocked by price filtering.
// It records metrics, logs details, and publishes events for single-candidate consolidations.
func (c *consolidation) handleBlockedConsolidation(ctx context.Context, candidates []*Candidate, priceImprovementFactor float64,
	factorSource, nodePoolName string, candidatePrice, cheapestReplacementPrice, actualSavingsPct, requiredSavingsPct float64,
	instanceTypesBefore, instanceTypesAfter int) {

	// Record blocking metric
	ConsolidationPriceFactorBlocksTotal.Inc(map[string]string{
		"reason":        "underutilized",
		"factor_source": factorSource,
		"nodepool":      nodePoolName,
	})

	// Log blocked consolidation details
	log.FromContext(ctx).Info("Consolidation blocked by price improvement factor",
		"actual_savings_pct", fmt.Sprintf("%.1f%%", actualSavingsPct*100),
		"required_savings_pct", fmt.Sprintf("%.1f%%", requiredSavingsPct*100),
		"decision", "blocked",
		"factor_source", factorSource,
		"nodepool", nodePoolName,
		"candidate_price", fmt.Sprintf("$%.2f", candidatePrice),
		"cheapest_replacement_price", fmt.Sprintf("$%.2f", cheapestReplacementPrice),
		"instance_types_before", instanceTypesBefore,
		"instance_types_after", instanceTypesAfter,
		"candidate_count", len(candidates),
	)

	// Publish event for single-candidate consolidations
	if len(candidates) == 1 {
		if priceImprovementFactor == 1.0 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "Can't replace with a cheaper node")...)
		} else {
			savingsPercent := int((1.0 - priceImprovementFactor) * 100)
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim,
				fmt.Sprintf("Can't replace with a node that offers at least %d%% cost savings", savingsPercent))...)
		}
	}
}

func (c *consolidation) filterByPrice(ctx context.Context, candidates []*Candidate, results pscheduling.Results, candidatePrice float64) bool {
	priceImprovementFactor, factorSource, nodePoolName := c.getPriceImprovementFactor(ctx, candidates)

	// Get cheapest replacement price and instance type counts before filtering
	instanceTypesBefore := len(results.NewNodeClaims[0].InstanceTypeOptions)
	cheapestReplacementPrice := getCheapestReplacementPrice(results.NewNodeClaims[0])

	// Apply price improvement factor to require cost savings
	var err error
	results.NewNodeClaims[0], err = results.NewNodeClaims[0].RemoveInstanceTypeOptionsByPriceAndMinValues(
		results.NewNodeClaims[0].Requirements, candidatePrice, priceImprovementFactor)
	if err != nil {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim,
				fmt.Sprintf("Filtering by price: %v", err))...)
		}
		return false
	}

	instanceTypesAfter := len(results.NewNodeClaims[0].InstanceTypeOptions)
	blocked := instanceTypesAfter == 0

	// Calculate savings percentages and record metrics
	actualSavingsPct := calculateSavingsPercentage(candidatePrice, cheapestReplacementPrice)
	requiredSavingsPct := 1.0 - priceImprovementFactor
	recordPriceFilterMetrics(blocked, actualSavingsPct, requiredSavingsPct, priceImprovementFactor, factorSource, nodePoolName)

	if blocked {
		c.handleBlockedConsolidation(ctx, candidates, priceImprovementFactor, factorSource, nodePoolName,
			candidatePrice, cheapestReplacementPrice, actualSavingsPct, requiredSavingsPct,
			instanceTypesBefore, instanceTypesAfter)
		return false
	}

	// Log allowed consolidation
	log.FromContext(ctx).V(1).Info("Consolidation allowed by price improvement factor",
		"actual_savings_pct", fmt.Sprintf("%.1f%%", actualSavingsPct*100),
		"required_savings_pct", fmt.Sprintf("%.1f%%", requiredSavingsPct*100),
		"decision", "allowed",
		"factor_source", factorSource,
		"nodepool", nodePoolName,
		"candidate_price", fmt.Sprintf("$%.2f", candidatePrice),
		"cheapest_replacement_price", fmt.Sprintf("$%.2f", cheapestReplacementPrice),
		"instance_types_before", instanceTypesBefore,
		"instance_types_after", instanceTypesAfter,
		"candidate_count", len(candidates),
	)

	return true
}
