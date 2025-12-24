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
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// MinDisruptionCost is the minimum disruption cost used when calculating savings per disruption cost.
// This prevents divide-by-zero for empty nodes and ensures even empty nodes require some savings to consolidate.
// A value of 1.0 is equivalent to one pod with default eviction cost.
const MinDisruptionCost = 1.0

type ConsolidationDecisionConfig struct {
	SpotToSpotConsolidation     bool
	MinSavingsPerDisruptionCost float64
}

// EvaluateConsolidation evaluates scheduling results and returns a consolidation decision.
// Returns a Command (which may be a no-op) and an optional reason string for logging/events.
// Note: This function modifies results.NewNodeClaims in place (filtering, sorting).
func EvaluateConsolidation(
	candidates []*Candidate,
	results pscheduling.Results,
	config ConsolidationDecisionConfig,
) (Command, string, error) {
	if cmd, reason := validateConsolidationPreconditions(candidates, results); cmd != nil || reason != "" {
		return *cmd, reason, nil
	}

	// get the current node price based on the offering
	candidatePrice, err := getCandidatePrices(candidates)
	if err != nil {
		return Command{}, "", fmt.Errorf("getting offering price from candidate node, %w", err)
	}

	// sort the instanceTypes by price before we take any actions like truncation for spot-to-spot consolidation
	results.NewNodeClaims[0].InstanceTypeOptions = results.NewNodeClaims[0].InstanceTypeOptions.OrderByPrice(results.NewNodeClaims[0].Requirements)

	if allCandidatesAreSpot(candidates) && results.NewNodeClaims[0].Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeSpot) {
		return evaluateSpotToSpotConsolidation(candidates, results, candidatePrice, config)
	}

	return evaluateRegularConsolidation(candidates, results, candidatePrice, config)
}

func validateConsolidationPreconditions(candidates []*Candidate, results pscheduling.Results) (*Command, string) {
	// if not all of the pods were scheduled, we can't do anything
	if !results.AllNonPendingPodsScheduled() {
		return &Command{}, pretty.Sentence(results.NonPendingPodSchedulingErrors())
	}

	// were we able to schedule all the pods on the inflight candidates?
	if len(results.NewNodeClaims) == 0 {
		cmd := Command{Candidates: candidates, Results: results}
		return &cmd, ""
	}

	// we're not going to turn a single node into multiple candidates
	if len(results.NewNodeClaims) != 1 {
		return &Command{}, fmt.Sprintf("Can't remove without creating %d candidates", len(results.NewNodeClaims))
	}

	return nil, ""
}

func allCandidatesAreSpot(candidates []*Candidate) bool {
	for _, cn := range candidates {
		if cn.capacityType != v1.CapacityTypeSpot {
			return false
		}
	}
	return true
}

func evaluateRegularConsolidation(candidates []*Candidate, results pscheduling.Results, candidatePrice float64, config ConsolidationDecisionConfig) (Command, string, error) {
	// filterByPrice returns the instanceTypes that are lower priced than the current candidate
	nodeClaim, err := results.NewNodeClaims[0].RemoveInstanceTypeOptionsByPriceAndMinValues(results.NewNodeClaims[0].Requirements, candidatePrice)
	if err != nil {
		return Command{}, fmt.Sprintf("Filtering by price: %v", err), nil
	}
	results.NewNodeClaims[0] = nodeClaim

	if len(results.NewNodeClaims[0].InstanceTypeOptions) == 0 {
		return Command{}, "Can't replace with a cheaper node", nil
	}

	// Check if savings justify the disruption cost
	replacementPrice := GetCheapestReplacementPrice(results.NewNodeClaims[0].InstanceTypeOptions, results.NewNodeClaims[0].Requirements)
	if reason := CheckSavingsThreshold(candidates, candidatePrice, replacementPrice, config); reason != "" {
		return Command{}, reason, nil
	}

	// We are consolidating a node from OD -> [OD,Spot] but have filtered the instance types by cost based on the
	// assumption that the spot variant will launch. We also need to add a requirement to the node to ensure that if
	// spot capacity is insufficient we don't replace the node with a more expensive on-demand node.
	ctReq := results.NewNodeClaims[0].Requirements.Get(v1.CapacityTypeLabelKey)
	if ctReq.Has(v1.CapacityTypeSpot) && ctReq.Has(v1.CapacityTypeOnDemand) {
		results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeSpot))
	}

	return Command{
		Candidates:   candidates,
		Replacements: replacementsFromNodeClaims(results.NewNodeClaims...),
		Results:      results,
	}, "", nil
}

// evaluateSpotToSpotConsolidation handles spot-to-spot consolidation decisions.
func evaluateSpotToSpotConsolidation(
	candidates []*Candidate,
	results pscheduling.Results,
	candidatePrice float64,
	config ConsolidationDecisionConfig,
) (Command, string, error) {
	// Spot consolidation is turned off.
	if !config.SpotToSpotConsolidation {
		return Command{}, "SpotToSpotConsolidation is disabled, can't replace a spot node with a spot node", nil
	}

	// Enforce spot requirement
	results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeSpot))
	results.NewNodeClaims[0].InstanceTypeOptions = results.NewNodeClaims[0].InstanceTypeOptions.Compatible(results.NewNodeClaims[0].Requirements)

	// Filter by price
	var err error
	results.NewNodeClaims[0], err = results.NewNodeClaims[0].RemoveInstanceTypeOptionsByPriceAndMinValues(results.NewNodeClaims[0].Requirements, candidatePrice)
	if err != nil {
		return Command{}, fmt.Sprintf("Filtering by price: %v", err), nil
	}
	if len(results.NewNodeClaims[0].InstanceTypeOptions) == 0 {
		return Command{}, "Can't replace with a cheaper node", nil
	}

	// Check if savings justify the disruption cost
	replacementPrice := GetCheapestReplacementPrice(results.NewNodeClaims[0].InstanceTypeOptions, results.NewNodeClaims[0].Requirements)
	if reason := CheckSavingsThreshold(candidates, candidatePrice, replacementPrice, config); reason != "" {
		return Command{}, reason, nil
	}

	// For multi-node consolidation, we don't need to check instance type flexibility
	if len(candidates) > 1 {
		return Command{
			Candidates:   candidates,
			Replacements: replacementsFromNodeClaims(results.NewNodeClaims...),
			Results:      results,
		}, "", nil
	}

	// For single-node consolidation: check we have at least 15 cheaper instance types
	if len(results.NewNodeClaims[0].InstanceTypeOptions) < MinInstanceTypesForSpotToSpotConsolidation {
		return Command{}, fmt.Sprintf("SpotToSpotConsolidation requires %d cheaper instance type options than the current candidate to consolidate, got %d",
			MinInstanceTypesForSpotToSpotConsolidation, len(results.NewNodeClaims[0].InstanceTypeOptions)), nil
	}

	// If a user has minValues set in their NodePool requirements, then we cap the number of instancetypes at 100 which would be the actual number of instancetypes sent for launch to enable spot-to-spot consolidation.
	// If no minValues in the NodePool requirement, then we follow the default 15 to cap the instance types for launch to enable a spot-to-spot consolidation.
	// Restrict the InstanceTypeOptions for launch to 15(if default) so we don't get into a continual consolidation situation.
	// For example:
	// 1) Suppose we have 5 instance types, (A, B, C, D, E) in order of price with the minimum flexibility 3 and they'll all work for our pod.  We send CreateInstanceFromTypes(A,B,C,D,E) and it gives us a E type based on price and availability of spot.
	// 2) We check if E is part of (A,B,C,D) and it isn't, so we will immediately have consolidation send a CreateInstanceFromTypes(A,B,C,D), since they're cheaper than E.
	// 3) Assuming CreateInstanceFromTypes(A,B,C,D) returned D, we check if D is part of (A,B,C) and it isn't, so will have another consolidation send a CreateInstanceFromTypes(A,B,C), since they're cheaper than D resulting in continual consolidation.
	// If we had restricted instance types to min flexibility at launch at step (1) i.e CreateInstanceFromTypes(A,B,C), we would have received the instance type part of the list preventing immediate consolidation.
	// Taking this to 15 types, we need to only send the 15 cheapest types in the CreateInstanceFromTypes call so that the resulting instance is always in that set of 15 and we won't immediately consolidate.
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
	}, "", nil
}

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
	volumeTopology         *pscheduling.VolumeTopology
	scheduler              SchedulerFunc
	lastConsolidationState time.Time
}

func MakeConsolidation(clock clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cloudProvider cloudprovider.CloudProvider, recorder events.Recorder, queue *Queue, volumeTopology *pscheduling.VolumeTopology) consolidation {
	c := consolidation{
		queue:          queue,
		clock:          clock,
		cluster:        cluster,
		kubeClient:     kubeClient,
		provisioner:    provisioner,
		cloudProvider:  cloudProvider,
		recorder:       recorder,
		volumeTopology: volumeTopology,
	}
	// Default scheduler uses SimulateScheduling with the consolidation's dependencies
	c.scheduler = func(ctx context.Context, candidates ...*Candidate) (pscheduling.Results, error) {
		return SimulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, candidates...)
	}
	return c
}

// SetScheduler allows injecting a custom scheduler, primarily for testing.
func (c *consolidation) SetScheduler(scheduler SchedulerFunc) {
	c.scheduler = scheduler
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

// computeConsolidation computes a consolidation action to take.
func (c *consolidation) computeConsolidation(ctx context.Context, candidates ...*Candidate) (Command, error) {
	// Run scheduling simulation
	results, err := c.scheduler(ctx, candidates...)
	if err != nil {
		// if a candidate node is now deleting, just retry
		if errors.Is(err, errCandidateDeleting) {
			return Command{}, nil
		}
		return Command{}, err
	}

	// Evaluate the consolidation decision
	config := ConsolidationDecisionConfig{
		SpotToSpotConsolidation:     options.FromContext(ctx).FeatureGates.SpotToSpotConsolidation,
		MinSavingsPerDisruptionCost: GetMinSavingsPerDisruptionCost(ctx, candidates),
	}
	cmd, reason, err := EvaluateConsolidation(candidates, results, config)
	if err != nil {
		return Command{}, err
	}

	// Publish events if consolidation was not possible
	// Only report events in the single node case since multi-node consolidation
	// will try different candidate combinations
	if reason != "" && len(candidates) == 1 {
		c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, reason)...)
	}

	return cmd, nil
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

// GetTotalDisruptionCost returns the sum of disruption costs for all candidates.
func GetTotalDisruptionCost(candidates []*Candidate) float64 {
	return lo.SumBy(candidates, func(c *Candidate) float64 { return c.DisruptionCost })
}

// GetCheapestReplacementPrice returns the price of the cheapest instance type option
// that is compatible with the given requirements. Returns math.MaxFloat64 if no
// compatible offerings are available.
// Precondition: instanceTypes must be sorted by price (via OrderByPrice).
func GetCheapestReplacementPrice(instanceTypes cloudprovider.InstanceTypes, reqs scheduling.Requirements) float64 {
	if len(instanceTypes) == 0 {
		return math.MaxFloat64
	}
	cheapestPrice := math.MaxFloat64
	for _, of := range instanceTypes[0].Offerings {
		if of.Available && reqs.IsCompatible(of.Requirements, scheduling.AllowUndefinedWellKnownLabels) && of.Price < cheapestPrice {
			cheapestPrice = of.Price
		}
	}
	return cheapestPrice
}

// CheckSavingsThreshold verifies that the savings per disruption cost meets the configured threshold.
// Returns a reason string if the threshold is not met, or empty string if consolidation should proceed.
//
// The metric "savings per disruption cost" measures hourly savings divided by the total disruption
// cost of the candidate nodes. DisruptionCost is calculated as the sum of pod eviction costs
// scaled by node lifetime remaining.
func CheckSavingsThreshold(candidates []*Candidate, candidatePrice, replacementPrice float64, config ConsolidationDecisionConfig) string {
	if config.MinSavingsPerDisruptionCost <= 0 {
		return ""
	}

	savings := candidatePrice - replacementPrice
	if savings <= 0 {
		return ""
	}

	totalDisruptionCost := GetTotalDisruptionCost(candidates)
	effectiveDisruptionCost := math.Max(totalDisruptionCost, MinDisruptionCost)
	savingsPerDisruptionCost := savings / effectiveDisruptionCost

	if savingsPerDisruptionCost < config.MinSavingsPerDisruptionCost {
		return fmt.Sprintf("savings per disruption cost %.4f below threshold %.4f (savings=$%.4f/hr, disruption cost=%.2f)",
			savingsPerDisruptionCost, config.MinSavingsPerDisruptionCost, savings, effectiveDisruptionCost)
	}
	return ""
}

// GetMinSavingsPerDisruptionCost returns the threshold to use for this consolidation.
// For multi-node consolidation involving multiple NodePools, uses the maximum threshold
// across all candidates so that any NodePool can enforce its own disruption bar.
func GetMinSavingsPerDisruptionCost(ctx context.Context, candidates []*Candidate) float64 {
	controllerDefault := options.FromContext(ctx).ConsolidationMinSavingsPerDisruptionCost
	maxThreshold := controllerDefault

	for _, candidate := range candidates {
		if candidate.NodePool == nil {
			continue
		}
		if thresholdStr := candidate.NodePool.Spec.Disruption.MinSavingsPerDisruptionCost; thresholdStr != nil {
			if val, err := strconv.ParseFloat(*thresholdStr, 64); err == nil && val > maxThreshold {
				maxThreshold = val
			}
		}
	}
	return maxThreshold
}
