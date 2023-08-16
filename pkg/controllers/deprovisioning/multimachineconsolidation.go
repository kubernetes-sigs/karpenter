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
	"time"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	deprovisioningevents "github.com/aws/karpenter-core/pkg/controllers/deprovisioning/events"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
)

const MultiMachineConsolidationTimeoutDuration = 1 * time.Minute

type MultiMachineConsolidation struct {
	consolidation
}

func NewMultiMachineConsolidation(clk clock.Clock, cluster *state.Cluster, kubeClient client.Client,
	provisioner *provisioning.Provisioner, cp cloudprovider.CloudProvider, recorder events.Recorder) *MultiMachineConsolidation {
	return &MultiMachineConsolidation{makeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder)}
}

func (m *MultiMachineConsolidation) ShouldDeprovision(_ context.Context, cn *Candidate) bool {
	if cn.Annotations()[v1alpha5.DoNotConsolidateNodeAnnotationKey] == "true" {
		m.recorder.Publish(deprovisioningevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("%s annotation exists", v1alpha5.DoNotConsolidateNodeAnnotationKey))...)
		return false
	}
	if cn.nodePool.Spec.Deprovisioning.ConsolidationPolicy == v1beta1.ConsolidationPolicyNever ||
		cn.nodePool.Spec.Deprovisioning.ConsolidationPolicy == v1beta1.ConsolidationPolicyWhenEmpty {
		m.recorder.Publish(deprovisioningevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("%s %q has underutilized consolidation disabled by consolidation policy", lo.Ternary(cn.nodePool.IsProvisioner, "Provisioner", "NodePool"), cn.nodePool.Name))...)
		return false
	}
	return true
}

func (m *MultiMachineConsolidation) ComputeCommand(ctx context.Context, candidates ...*Candidate) (Command, error) {
	if m.isConsolidated() {
		return Command{}, nil
	}
	candidates, err := m.sortAndFilterCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("sorting candidates, %w", err)
	}
	deprovisioningEligibleMachinesGauge.WithLabelValues(m.String()).Set(float64(len(candidates)))

	// Only consider a maximum batch of 100 machines to save on computation.
	// This could be further configurable in the future.
	maxParallel := lo.Clamp(len(candidates), 0, 100)

	cmd, err := m.firstNMachineConsolidationOption(ctx, candidates, maxParallel)
	if err != nil {
		return Command{}, err
	}

	if cmd.Action() == NoOpAction {
		// couldn't identify any candidates
		m.markConsolidated()
		return cmd, nil
	}

	v := NewValidation(consolidationTTL(cmd.candidates), m.clock, m.cluster, m.kubeClient, m.provisioner, m.cloudProvider, m.recorder)
	isValid, err := v.IsValid(ctx, cmd)
	if err != nil {
		return Command{}, fmt.Errorf("validating, %w", err)
	}

	if !isValid {
		logging.FromContext(ctx).Debugf("abandoning multi machine consolidation attempt due to pod churn, command is no longer valid, %s", cmd)
		return Command{}, nil
	}
	return cmd, nil
}

// firstNMachineConsolidationOption looks at the first N machines to determine if they can all be consolidated at once.  The
// machines are sorted by increasing disruption order which correlates to likelihood if being able to consolidate the machine
func (m *MultiMachineConsolidation) firstNMachineConsolidationOption(ctx context.Context, candidates []*Candidate, max int) (Command, error) {
	// we always operate on at least two machines at once, for single machines standard consolidation will find all solutions
	if len(candidates) < 2 {
		return Command{}, nil
	}
	min := 1
	if len(candidates) <= max {
		max = len(candidates) - 1
	}

	lastSavedCommand := Command{}
	// Set a timeout
	timeout := m.clock.Now().Add(MultiMachineConsolidationTimeoutDuration)
	// binary search to find the maximum number of machines we can terminate
	for min <= max {
		if m.clock.Now().After(timeout) {
			deprovisioningConsolidationTimeoutsCounter.WithLabelValues(multiMachineConsolidationLabelValue).Inc()
			if lastSavedCommand.candidates == nil {
				logging.FromContext(ctx).Debugf("failed to find a multi-machine consolidation after timeout, last considered batch had %d machines", (min+max)/2)
			} else {
				logging.FromContext(ctx).Debugf("stopping multi-machine consolidation after timeout, returning last valid command %s", lastSavedCommand)
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
			// we can consolidate machines [0,mid]
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
// machines=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.small, t3a.xlarge, t3a.2xlarge
//
// In this case, we shouldn't perform this consolidation at all.  This is equivalent to just
// deleting the 2x t3a.xlarge machines.  This code will identify that t3a.small is in both lists and filter
// out any instance type that is the same or more expensive than the t3a.small
//
// For another scenario:
// machines=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.nano, t3a.small, t3a.xlarge, t3a.2xlarge
//
// This code sees that t3a.small is the cheapest type in both lists and filters it and anything more expensive out
// leaving the valid consolidation:
// machines=[t3a.2xlarge, t3a.2xlarge, t3a.small] -> 1 of t3a.nano
func filterOutSameType(newMachine *scheduling.NodeClaim, consolidate []*Candidate) []*cloudprovider.InstanceType {
	existingInstanceTypes := sets.NewString()
	pricesByInstanceType := map[string]float64{}

	// get the price of the cheapest machine that we currently are considering deleting indexed by instance type
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
	for _, it := range newMachine.InstanceTypeOptions {
		// we are considering replacing multiple machines with a single machine of one of the same types, so the replacement
		// machine must be cheaper than the price of the existing machine, or we should just keep that one and do a
		// deletion only to reduce cluster disruption (fewer pods will re-schedule).
		if existingInstanceTypes.Has(it.Name) {
			if pricesByInstanceType[it.Name] < maxPrice {
				maxPrice = pricesByInstanceType[it.Name]
			}
		}
	}

	return filterByPrice(newMachine.InstanceTypeOptions, newMachine.Requirements, maxPrice)
}
