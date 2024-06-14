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
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha5"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption/orchestration"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// MinInstanceTypesForSpotToSpotConsolidation is the minimum number of instanceTypes in a NodeClaim needed to trigger spot-to-spot single-node consolidation
const MinInstanceTypesForSpotToSpotConsolidation = 15

// Underutilization is the base Underutilization controller that provides common functionality used across the different
// Underutilization methods.
type Underutilization struct {
	// Consolidation needs to be aware of the queue for validation
	queue         *orchestration.Queue
	clock         clock.Clock
	cluster       *state.Cluster
	kubeClient    client.Client
	provisioner   *provisioning.Provisioner
	cloudProvider cloudprovider.CloudProvider
	recorder      events.Recorder
}

type Nomination struct {
	Candidates       []*Candidate
	ConsolidateAfter time.Duration
	CommandID        string
}

func NewUnderutilization(clock clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cloudProvider cloudprovider.CloudProvider, recorder events.Recorder, queue *orchestration.Queue) *Underutilization {
	return &Underutilization{
		queue:         queue,
		clock:         clock,
		cluster:       cluster,
		kubeClient:    kubeClient,
		provisioner:   provisioner,
		cloudProvider: cloudProvider,
		recorder:      recorder,
	}
}

func (u *Underutilization) Type() string {
	return "underutilization"
}

// ShouldDisrupt is a predicate used to filter candidates
func (u *Underutilization) ShouldDisrupt(_ context.Context, cn *Candidate) bool {
	// TODO: Remove the check for do-not-consolidate at v1
	if cn.Annotations()[v1alpha5.DoNotConsolidateNodeAnnotationKey] == "true" {
		u.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("%s annotation exists", v1alpha5.DoNotConsolidateNodeAnnotationKey))...)
		return false
	}
	if cn.nodePool.Spec.Disruption.ConsolidateAfter.Duration == nil {
		u.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("NodePool %q has consolidation disabled", cn.nodePool.Name))...)
		return false
	}
	// If "WhenUnderutilized" is not the consolidation policy, only consider emptiness for
	// cost consolidation
	if cn.nodePool.Spec.Disruption.ConsolidationPolicy != v1beta1.ConsolidationPolicyWhenUnderutilized {
		u.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("NodePool %q has underutilization consolidation disabled", cn.nodePool.Name))...)
		return false
	}
	return cn.NodeClaim.StatusConditions().Get(v1beta1.ConditionTypeUnderutilized).IsTrue()
}

// ComputeCommand computes a consolidation action to take
//
// nolint:gocyclo
func (u *Underutilization) ComputeCommand(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) (Command, pscheduling.Results, error) {
	// All the candidates here all have the v1beta1.ConditionTypeUnderutilized condition
	nominations, err := u.nominations(ctx, candidates...)
	if err != nil {
		return Command{}, pscheduling.Results{}, fmt.Errorf("grabbing nominations, %w", err)
	}
	for _, nodes := range nominations {
		// Run scheduling simulation to compute consolidation option
		results, err := SimulateScheduling(ctx, u.kubeClient, u.cluster, u.provisioner, nodes...)
		if err != nil {
			// if a candidate node is now deleting, just retry
			if errors.Is(err, errCandidateDeleting) {
				return Command{}, pscheduling.Results{}, nil
			}
			return Command{}, pscheduling.Results{}, err
		}
		results, valid, err := validateResults(ctx, results, u.recorder, nodes...)
		if err != nil {
			return Command{}, pscheduling.Results{}, fmt.Errorf("validating scheduling simulation results, %w", err)
		}
		// If valid, we exit early and return the command
		if valid {
			return Command{
				candidates:   nodes,
				replacements: results.NewNodeClaims,
			}, results, nil
		}
	}

	return Command{}, pscheduling.Results{}, nil
}

// returns a map of command-id to list of candidate nodeclaim names
// returned sorted by creation timestamp
func (u *Underutilization) nominations(ctx context.Context, candidates ...*Candidate) ([][]*Candidate, error) {
	nodePools := u.cluster.NodePools()
	nominations := map[string]Nomination{}
	// query the cluster state and group all the nodes by their status condition's message, and aggregate
	// the consolidateAfters
	lo.ForEach(candidates, func(s *Candidate, _ int) {
		// We've already filtered candidates that don't have the underutilization condition set to true
		// so we only need to grab it to get its batch ID
		cond := s.NodeClaim.StatusConditions().Get(v1beta1.ConditionTypeUnderutilized)
		// If the nodepool isn't found, we assume it's deleted or cluster state hasn't seen it yet.
		// If it was deleted, we rely on the termination controller to clean this up
		np, found := nodePools[s.Node.Labels[v1beta1.NodePoolLabelKey]]
		if !found {
			return
		}
		// If the candidate we're coming across is part of an existing nomination, grab it again to edit
		nomination := nominations[cond.Message]
		// Add the command ID in idempotently
		nomination.CommandID = cond.Message
		nomination.Candidates = append(nomination.Candidates, s)
		// Use the larger consolidateAfter when batching
		nomination.ConsolidateAfter = lo.Max([]time.Duration{nomination.ConsolidateAfter, *np.Spec.Disruption.ConsolidateAfter.Duration})
		nominations[nomination.CommandID] = nomination
	})

	// Filter each of the commands that aren't valid, and map the underlying candidates to the existing valid disruption
	// candidates.
	valid := lo.FilterMap(lo.Values(nominations), func(nom Nomination, _ int) ([]*Candidate, bool) {
		// notValid is true if there is a candidate that hasn't been marked as Underutilized
		// for long enough
		_, invalid := lo.Find(nom.Candidates, func(s *Candidate) bool {
			cond := s.NodeClaim.StatusConditions().Get(v1beta1.ConditionTypeUnderutilized)
			// We want all of the candidates to have the underutilized condition and have the last transition time be at least
			// as long ago as the max consolidate after of all the candidates included in the command.
			return time.Since(cond.LastTransitionTime.Time) < nom.ConsolidateAfter
		})
		// If we find a candidate that can't be consolidated because of its consolidateAfter, we omit the command
		return nom.Candidates, !invalid
		// We make sure that we don't have any candidates that aren't valid (all candidates are valid)
		// and that the number of valid candidates is the same as the number of original candidates
	})
	return valid, nil
}

//nolint:gocyclo
func validateResults(ctx context.Context, results pscheduling.Results, recorder events.Recorder, candidates ...*Candidate) (pscheduling.Results, bool, error) {
	// if not all of the pods were scheduled, we can't do anything
	if !results.AllNonPendingPodsScheduled() {
		// This method is used by multi-node consolidation as well, so we'll only report in the single node case
		if len(candidates) == 1 {
			recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, results.NonPendingPodSchedulingErrors())...)
		}
		return pscheduling.Results{}, false, nil
	}

	// were we able to schedule all the pods on the inflight candidates?
	if len(results.NewNodeClaims) == 0 {
		return results, true, nil
	}

	// we're not going to turn a single node into multiple candidates
	if len(results.NewNodeClaims) != 1 {
		if len(candidates) == 1 {
			recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("Can't remove without creating %d candidates", len(results.NewNodeClaims)))...)
		}
		return pscheduling.Results{}, false, nil
	}

	// get the current node price based on the offering
	// fallback if we can't find the specific zonal pricing data
	candidatePrice, cheapestOfferingsPrices, err := getCandidatePrices(candidates)
	if err != nil {
		return pscheduling.Results{}, false, fmt.Errorf("getting offering price from candidate node, %w", err)
	}

	allExistingAreSpot := true
	for _, cn := range candidates {
		if cn.capacityType != v1beta1.CapacityTypeSpot {
			allExistingAreSpot = false
		}
	}

	// sort the instanceTypes by price before we take any actions like truncation for spot-to-spot consolidation or finding the nodeclaim
	// that meets the minimum requirement after filteringByPrice
	results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions = results.NewNodeClaims[0].InstanceTypeOptions.OrderByPrice(results.NewNodeClaims[0].Requirements)

	if allExistingAreSpot &&
		results.NewNodeClaims[0].Requirements.Get(v1beta1.CapacityTypeLabelKey).Has(v1beta1.CapacityTypeSpot) {
		_, results = validateSpotToSpotConsolidation(ctx, candidates, recorder, results, candidatePrice)
		return results, true, nil
	}

	// filterByPrice returns the instanceTypes that are lower priced than the current candidate and any error that indicates the input couldn't be filtered.
	// If we use this directly for spot-to-spot consolidation, we are bound to get repeated consolidations because the strategy that chooses to launch the spot instance from the list does
	// it based on availability and price which could result in selection/launch of non-lowest priced instance in the list. So, we would keep repeating this loop till we get to lowest priced instance
	// causing churns and landing onto lower available spot instance ultimately resulting in higher interruptions.
	results.NewNodeClaims[0], err = results.NewNodeClaims[0].RemoveInstanceTypeOptionsByPriceAndMinValues(results.NewNodeClaims[0].Requirements, candidatePrice)

	if err != nil {
		if len(candidates) == 1 {
			recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("Filtering by price: %v", err))...)
		}
		return pscheduling.Results{}, false, nil
	}
	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) == 0 {
		if len(candidates) == 1 {
			recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "Can't replace with a cheaper node")...)
		}
		return pscheduling.Results{}, false, nil
	}

	// ensure that the action is sensical for replacements, see explanation on filterOutSameType for why this is
	// required
	results.NewNodeClaims[0].InstanceTypeOptions, err = results.NewNodeClaims[0].RemoveSameInstanceTypeOptions(cheapestOfferingsPrices, sets.New(lo.Map(candidates, func(s *Candidate, _ int) string {
		return s.Labels()[v1.LabelInstanceTypeStable]
	})...))
	// min values should only impact single node consolidation decisions
	if len(candidates) == 1 {
		if err != nil {
			return pscheduling.Results{}, false, fmt.Errorf("failed to satisfy minValues, %w", err)
		}
	}
	if len(results.NewNodeClaims[0].InstanceTypeOptions) == 0 {
		return pscheduling.Results{}, false, fmt.Errorf("can't replace nodes with the same instance type")
	}

	// We are consolidating a node from OD -> [OD,Spot] but have filtered the instance types by cost based on the
	// assumption, that the spot variant will launch. We also need to add a requirement to the node to ensure that if
	// spot capacity is insufficient we don't replace the node with a more expensive on-demand node.  Instead the launch
	// should fail and we'll just leave the node alone.
	ctReq := results.NewNodeClaims[0].Requirements.Get(v1beta1.CapacityTypeLabelKey)
	if ctReq.Has(v1beta1.CapacityTypeSpot) && ctReq.Has(v1beta1.CapacityTypeOnDemand) {
		results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, v1beta1.CapacityTypeSpot))
	}

	return results, true, nil
}

// Compute command to execute spot-to-spot consolidation if:
//  1. The SpotToSpotConsolidation feature flag is set to true.
//  2. For single-node consolidation:
//     a. There are at least 15 cheapest instance type replacement options to consolidate.
//     b. The current candidate is NOT part of the first 15 cheapest instance types inorder to avoid repeated consolidation.
func validateSpotToSpotConsolidation(ctx context.Context, candidates []*Candidate, recorder events.Recorder, results pscheduling.Results,
	candidatePrice float64) (Command, pscheduling.Results) {

	// Spot consolidation is turned off.
	if !options.FromContext(ctx).FeatureGates.SpotToSpotConsolidation {
		if len(candidates) == 1 {
			recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "SpotToSpotConsolidation is disabled, can't replace a spot node with a spot node")...)
		}
		return Command{}, pscheduling.Results{}
	}

	// Since we are sure that the replacement nodeclaim considered for the spot candidates are spot, we will enforce it through the requirements.
	results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, v1beta1.CapacityTypeSpot))
	// All possible replacements for the current candidate compatible with spot offerings
	results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions = results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions.Compatible(results.NewNodeClaims[0].Requirements)

	// filterByPrice returns the instanceTypes that are lower priced than the current candidate and any error that indicates the input couldn't be filtered.
	var err error
	results.NewNodeClaims[0], err = results.NewNodeClaims[0].RemoveInstanceTypeOptionsByPriceAndMinValues(results.NewNodeClaims[0].Requirements, candidatePrice)
	if err != nil {
		if len(candidates) == 1 {
			recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("Filtering by price: %v", err))...)
		}
		return Command{}, pscheduling.Results{}
	}
	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) == 0 {
		if len(candidates) == 1 {
			recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "Can't replace with a cheaper node")...)
		}
		return Command{}, pscheduling.Results{}
	}

	// For multi-node consolidation:
	// We don't have any requirement to check the remaining instance type flexibility, so exit early in this case.
	if len(candidates) > 1 {
		return Command{
			candidates:   candidates,
			replacements: results.NewNodeClaims,
		}, results
	}

	// For single-node consolidation:

	// We check whether we have 15 cheaper instances than the current candidate instance. If this is the case, we know the following things:
	//   1) The current candidate is not in the set of the 15 cheapest instance types and
	//   2) There were at least 15 options cheaper than the current candidate.
	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) < MinInstanceTypesForSpotToSpotConsolidation {
		recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("SpotToSpotConsolidation requires %d cheaper instance type options than the current candidate to consolidate, got %d",
			MinInstanceTypesForSpotToSpotConsolidation, len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions)))...)
		return Command{}, pscheduling.Results{}
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
		minInstanceTypes, _ := results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions.SatisfiesMinValues(results.NewNodeClaims[0].Requirements)
		results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions = lo.Slice(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions, 0, lo.Max([]int{MinInstanceTypesForSpotToSpotConsolidation, minInstanceTypes}))
	} else {
		results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions = lo.Slice(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions, 0, MinInstanceTypesForSpotToSpotConsolidation)
	}

	return Command{
		candidates:   candidates,
		replacements: results.NewNodeClaims,
	}, results
}

// getCandidatePrices returns the sum of the prices of the given candidates
func getCandidatePrices(candidates []*Candidate) (float64, map[string]float64, error) {
	pricesByInstanceType := map[string]float64{}

	var price float64
	for _, c := range candidates {
		compatibleOfferings := c.instanceType.Offerings.Compatible(scheduling.NewLabelRequirements(c.StateNode.Labels()))
		if len(compatibleOfferings) == 0 {
			return 0.0, nil, fmt.Errorf("unable to determine offering for %s/%s/%s", c.instanceType.Name, c.capacityType, c.zone)
		}
		existingPrice, ok := pricesByInstanceType[c.instanceType.Name]
		if !ok {
			existingPrice = math.MaxFloat64
		}
		if p := compatibleOfferings.Cheapest().Price; p < existingPrice {
			pricesByInstanceType[c.instanceType.Name] = p
		}
		price += compatibleOfferings.Cheapest().Price
	}
	return price, pricesByInstanceType, nil
}
