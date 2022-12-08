/*
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

package deprovisioning

import (
	"context"
	"fmt"
	"math"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
)

type MultiNodeConsolidation struct {
	consolidation
}

func NewMultiNodeConsolidation(clk clock.Clock, cluster *state.Cluster, kubeClient client.Client,
	provisioner *provisioning.Provisioner, cp cloudprovider.CloudProvider, reporter *Reporter) *MultiNodeConsolidation {
	return &MultiNodeConsolidation{makeConsolidation(clk, cluster, kubeClient, provisioner, cp, reporter)}
}

func (m *MultiNodeConsolidation) ComputeCommand(ctx context.Context, candidates ...CandidateNode) (Command, error) {
	if !m.ShouldAttemptConsolidation() {
		return Command{action: actionDoNothing}, nil
	}
	candidates, err := m.sortAndFilterCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("sorting candidates, %w", err)
	}

	// For now, we will consider up to every node in the cluster, might be configurable in the future.
	maxParallel := len(candidates)
	cmd, err := m.firstNNodeConsolidationOption(ctx, candidates, maxParallel)
	if err != nil {
		return Command{}, err
	}
	if cmd.action == actionDoNothing {
		return cmd, nil
	}

	v := NewValidation(consolidationTTL, m.clock, m.cluster, m.kubeClient, m.provisioner, m.cloudProvider)
	isValid, err := v.IsValid(ctx, cmd)
	if err != nil {
		return Command{}, fmt.Errorf("validating, %w", err)
	}

	if !isValid {
		return Command{action: actionRetry}, nil
	}
	return cmd, nil
}

// firstNNodeConsolidationOption looks at the first N nodes to determine if they can all be consolidated at once.  The
// nodes are sorted by increasing disruption order which correlates to likelihood if being able to consolidate the node
func (m *MultiNodeConsolidation) firstNNodeConsolidationOption(ctx context.Context, candidates []CandidateNode, max int) (Command, error) {
	// we always operate on at least two nodes at once, for single nodes standard consolidation will find all solutions
	if len(candidates) < 2 {
		return Command{action: actionDoNothing}, nil
	}
	min := 1
	if len(candidates) <= max {
		max = len(candidates) - 1
	}

	lastSavedCommand := Command{action: actionDoNothing}
	// binary search to find the maximum number of nodes we can terminate
	for min <= max {
		mid := (min + max) / 2

		nodesToConsolidate := candidates[0 : mid+1]

		action, err := m.computeConsolidation(ctx, nodesToConsolidate...)
		if err != nil {
			return Command{}, err
		}

		// ensure that the action is sensical for replacements, see explanation on filterOutSameType for why this is
		// required
		if action.action == actionReplace {
			action.replacementNodes[0].InstanceTypeOptions = filterOutSameType(action.replacementNodes[0], nodesToConsolidate)
			if len(action.replacementNodes[0].InstanceTypeOptions) == 0 {
				action.action = actionDoNothing
			}
		}

		if action.action == actionReplace || action.action == actionDelete {
			// we can consolidate nodes [0,mid]
			lastSavedCommand = action
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
// nodes=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.small, t3a.xlarge, t3a.2xlarge
//
// In this case, we shouldn't perform this consolidation at all.  This is equivalent to just
// deleting the 2x t3a.xlarge nodes.  This code will identify that t3a.small is in both lists and filter
// out any instance type that is the same or more expensive than the t3a.small
//
// For another scenario:
// nodes=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.nano, t3a.small, t3a.xlarge, t3a.2xlarge
//
// This code sees that t3a.small is the cheapest type in both lists and filters it and anything more expensive out
// leaving the valid consolidation:
// nodes=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.nano
func filterOutSameType(newNode *scheduling.Node, consolidate []CandidateNode) []*cloudprovider.InstanceType {
	existingInstanceTypes := sets.NewString()
	nodePricesByInstanceType := map[string]float64{}

	// get the price of the cheapest node that we currently are considering deleting indexed by instance type
	for _, n := range consolidate {
		existingInstanceTypes.Insert(n.instanceType.Name)
		of, ok := n.instanceType.Offerings.Get(n.capacityType, n.zone)
		if !ok {
			continue
		}
		existingPrice, ok := nodePricesByInstanceType[n.instanceType.Name]
		if !ok {
			existingPrice = math.MaxFloat64
		}
		if of.Price < existingPrice {
			nodePricesByInstanceType[n.instanceType.Name] = of.Price
		}
	}

	maxPrice := math.MaxFloat64
	for _, it := range newNode.InstanceTypeOptions {
		// we are considering replacing multiple nodes with a single node of one of the same types, so the replacement
		// node must be cheaper than the price of the existing node, or we should just keep that one and do a
		// deletion only to reduce cluster disruption (fewer pods will re-schedule).
		if existingInstanceTypes.Has(it.Name) {
			if nodePricesByInstanceType[it.Name] < maxPrice {
				maxPrice = nodePricesByInstanceType[it.Name]
			}
		}
	}

	return filterByPrice(newNode.InstanceTypeOptions, newNode.Requirements, maxPrice)
}
