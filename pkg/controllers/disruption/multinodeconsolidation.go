/*
Copyright 2023 The Kubernetes Authors.

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
	"fmt"
	"math"
	"time"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/logging"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

const MultiNodeConsolidationTimeoutDuration = 1 * time.Minute

type MultiNodeConsolidation struct {
	consolidation
}

func NewMultiNodeConsolidation(consolidation consolidation) *MultiNodeConsolidation {
	return &MultiNodeConsolidation{consolidation: consolidation}
}

func (m *MultiNodeConsolidation) ComputeCommand(ctx context.Context, candidates ...*Candidate) (Command, error) {
	if m.isConsolidated() {
		return Command{}, nil
	}
	candidates, err := m.sortAndFilterCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("sorting candidates, %w", err)
	}
	disruptionEligibleNodesGauge.With(map[string]string{
		methodLabel:            m.Type(),
		consolidationTypeLabel: m.ConsolidationType(),
	}).Set(float64(len(candidates)))

	// Only consider a maximum batch of 100 NodeClaims to save on computation.
	// This could be further configurable in the future.
	maxParallel := lo.Clamp(len(candidates), 0, 100)

	cmd, err := m.firstNConsolidationOption(ctx, candidates, maxParallel)
	if err != nil {
		return Command{}, err
	}

	if cmd.Action() == NoOpAction {
		// couldn't identify any candidates
		m.markConsolidated()
		return cmd, nil
	}

	v := NewValidation(consolidationTTL, m.clock, m.cluster, m.kubeClient, m.provisioner, m.cloudProvider, m.recorder, m.queue)
	isValid, err := v.IsValid(ctx, cmd)
	if err != nil {
		return Command{}, fmt.Errorf("validating, %w", err)
	}

	if !isValid {
		logging.FromContext(ctx).Debugf("abandoning multi-node consolidation attempt due to pod churn, command is no longer valid, %s", cmd)
		return Command{}, nil
	}
	return cmd, nil
}

// firstNConsolidationOption looks at the first N NodeClaims to determine if they can all be consolidated at once.  The
// NodeClaims are sorted by increasing disruption order which correlates to likelihood if being able to consolidate the node
func (m *MultiNodeConsolidation) firstNConsolidationOption(ctx context.Context, candidates []*Candidate, max int) (Command, error) {
	// we always operate on at least two NodeClaims at once, for single NodeClaims standard consolidation will find all solutions
	if len(candidates) < 2 {
		return Command{}, nil
	}
	min := 1
	if len(candidates) <= max {
		max = len(candidates) - 1
	}

	lastSavedCommand := Command{}
	// Set a timeout
	timeout := m.clock.Now().Add(MultiNodeConsolidationTimeoutDuration)
	// binary search to find the maximum number of NodeClaims we can terminate
	for min <= max {
		if m.clock.Now().After(timeout) {
			disruptionConsolidationTimeoutTotalCounter.WithLabelValues(m.ConsolidationType()).Inc()
			if lastSavedCommand.candidates == nil {
				logging.FromContext(ctx).Debugf("failed to find a multi-node consolidation after timeout, last considered batch had %d", (min+max)/2)
			} else {
				logging.FromContext(ctx).Debugf("stopping multi-node consolidation after timeout, returning last valid command %s", lastSavedCommand)
			}
			return lastSavedCommand, nil
		}
		mid := (min + max) / 2
		candidatesToConsolidate := candidates[0 : mid+1]

		cmd, err := m.computeConsolidation(ctx, candidatesToConsolidate...)
		if err != nil {
			return Command{}, err
		}

		// ensure that the action is sensical for replacements, see explanation on filterOutSameType for why this is
		// required
		replacementHasValidInstanceTypes := false
		if cmd.Action() == ReplaceAction {
			cmd.replacements[0].InstanceTypeOptions = filterOutSameType(cmd.replacements[0], candidatesToConsolidate)
			replacementHasValidInstanceTypes = len(cmd.replacements[0].InstanceTypeOptions) > 0
		}

		// replacementHasValidInstanceTypes will be false if the replacement action has valid instance types remaining after filtering.
		if replacementHasValidInstanceTypes || cmd.Action() == DeleteAction {
			// We can consolidate NodeClaims [0,mid]
			lastSavedCommand = cmd
			min = mid + 1
		} else {
			max = mid - 1
		}
	}
	return lastSavedCommand, nil
}

// filterOutSameType filters out instance types that are more expensive than the cheapest instance type that is being
// consolidated if the list of replacement instance types include one of the instance types that is being removed
//
// This handles the following potential consolidation result:
// NodeClaims=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.small, t3a.xlarge, t3a.2xlarge
//
// In this case, we shouldn't perform this consolidation at all.  This is equivalent to just
// deleting the 2x t3a.xlarge NodeClaims.  This code will identify that t3a.small is in both lists and filter
// out any instance type that is the same or more expensive than the t3a.small
//
// For another scenario:
// NodeClaims=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.nano, t3a.small, t3a.xlarge, t3a.2xlarge
//
// This code sees that t3a.small is the cheapest type in both lists and filters it and anything more expensive out
// leaving the valid consolidation:
// NodeClaims=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.nano
func filterOutSameType(newNodeClaim *scheduling.NodeClaim, consolidate []*Candidate) []*cloudprovider.InstanceType {
	existingInstanceTypes := sets.New[string]()
	pricesByInstanceType := map[string]float64{}

	// get the price of the cheapest node that we currently are considering deleting indexed by instance type
	for _, c := range consolidate {
		existingInstanceTypes.Insert(c.instanceType.Name)
		of, ok := c.instanceType.Offerings.Get(c.capacityType, c.zone)
		if !ok {
			continue
		}
		existingPrice, ok := pricesByInstanceType[c.instanceType.Name]
		if !ok {
			existingPrice = math.MaxFloat64
		}
		if of.Price < existingPrice {
			pricesByInstanceType[c.instanceType.Name] = of.Price
		}
	}

	maxPrice := math.MaxFloat64
	for _, it := range newNodeClaim.InstanceTypeOptions {
		// we are considering replacing multiple NodeClaims with a single NodeClaim of one of the same types, so the replacement
		// node must be cheaper than the price of the existing node, or we should just keep that one and do a
		// deletion only to reduce cluster disruption (fewer pods will re-schedule).
		if existingInstanceTypes.Has(it.Name) {
			if pricesByInstanceType[it.Name] < maxPrice {
				maxPrice = pricesByInstanceType[it.Name]
			}
		}
	}

	return filterByPrice(newNodeClaim.InstanceTypeOptions, newNodeClaim.Requirements, maxPrice)
}

func (m *MultiNodeConsolidation) Type() string {
	return metrics.ConsolidationReason
}

func (m *MultiNodeConsolidation) ConsolidationType() string {
	return "multi"
}
