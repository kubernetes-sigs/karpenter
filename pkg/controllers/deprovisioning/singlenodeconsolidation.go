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
	"errors"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	scheduling2 "github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

// SingleNodeConsolidation is the consolidation controller that performs single node consolidation.
type SingleNodeConsolidation struct {
	consolidation
}

func NewSingleNodeConsolidation(clk clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner, cp cloudprovider.CloudProvider) *SingleNodeConsolidation {
	return &SingleNodeConsolidation{consolidation: consolidation{
		clock:         clk,
		cluster:       cluster,
		kubeClient:    kubeClient,
		provisioner:   provisioner,
		cloudProvider: cp,
	},
	}
}

// ComputeCommand generates a deprovisioning command given deprovisionable nodes
//
//nolint:gocyclo
func (c *SingleNodeConsolidation) ComputeCommand(ctx context.Context, candidates ...CandidateNode) (Command, error) {
	if !c.ShouldAttemptConsolidation() {
		return Command{action: actionDoNothing}, nil
	}
	candidates, err := c.sortAndFilterCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("sorting candidates, %w", err)
	}

	v := NewValidation(consolidationTTL, c.clock, c.cluster, c.kubeClient, c.provisioner, c.cloudProvider)
	var failedValidation bool
	for _, node := range candidates {
		// compute a possible consolidation option
		cmd, err := c.computeConsolidation(ctx, node)
		if err != nil {
			logging.FromContext(ctx).Errorf("computing consolidation %s", err)
			continue
		}
		if cmd.action == actionDoNothing || cmd.action == actionRetry || cmd.action == actionFailed {
			continue
		}

		isValid, err := v.IsValid(ctx, cmd)
		if err != nil {
			logging.FromContext(ctx).Errorf("validating consolidation %s", err)
			continue
		}
		if !isValid {
			failedValidation = true
			continue
		}

		if cmd.action == actionReplace || cmd.action == actionDelete {
			return cmd, nil
		}
	}

	// we failed validation, so we need to retry
	if failedValidation {
		return Command{action: actionRetry}, nil
	}
	return Command{action: actionDoNothing}, nil
}

// computeConsolidation computes a consolidation action to take
//
// nolint:gocyclo
func (c *SingleNodeConsolidation) computeConsolidation(ctx context.Context, node CandidateNode) (Command, error) {
	defer metrics.Measure(deprovisioningDurationHistogram.WithLabelValues("Replace/Delete"))()
	// Run scheduling simulation to compute consolidation option
	newNodes, allPodsScheduled, err := simulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, node)
	if err != nil {
		// if a candidate node is now deleting, just retry
		if errors.Is(err, errCandidateNodeDeleting) {
			return Command{action: actionDoNothing}, nil
		}
		return Command{}, err
	}

	// if not all of the pods were scheduled, we can't do anything
	if !allPodsScheduled {
		return Command{action: actionDoNothing}, nil
	}

	// were we able to schedule all the pods on the inflight nodes?
	if len(newNodes) == 0 {
		return Command{
			nodesToRemove: []*v1.Node{node.Node},
			action:        actionDelete,
		}, nil
	}

	// we're not going to turn a single node into multiple nodes
	if len(newNodes) != 1 {
		return Command{action: actionDoNothing}, nil
	}

	// get the current node price based on the offering
	// fallback if we can't find the specific zonal pricing data
	offering, ok := node.instanceType.Offerings.Get(node.capacityType, node.zone)
	if !ok {
		return Command{}, fmt.Errorf("getting offering price from candidate node, %w", err)
	}
	newNodes[0].InstanceTypeOptions = filterByPrice(newNodes[0].InstanceTypeOptions, newNodes[0].Requirements, offering.Price)
	if len(newNodes[0].InstanceTypeOptions) == 0 {
		// no instance types remain after filtering by price
		return Command{action: actionDoNothing}, nil
	}

	// If the existing node is spot and the replacement is spot, we don't consolidate.  We don't have a reliable
	// mechanism to determine if this replacement makes sense given instance type availability (e.g. we may replace
	// a spot node with one that is less available and more likely to be reclaimed).
	if node.capacityType == v1alpha5.CapacityTypeSpot &&
		newNodes[0].Requirements.Get(v1alpha5.LabelCapacityType).Has(v1alpha5.CapacityTypeSpot) {
		return Command{action: actionDoNothing}, nil
	}

	// We are consolidating a node from OD -> [OD,Spot] but have filtered the instance types by cost based on the
	// assumption, that the spot variant will launch. We also need to add a requirement to the node to ensure that if
	// spot capacity is insufficient we don't replace the node with a more expensive on-demand node.  Instead the launch
	// should fail and we'll just leave the node alone.
	ctReq := newNodes[0].Requirements.Get(v1alpha5.LabelCapacityType)
	if ctReq.Has(v1alpha5.CapacityTypeSpot) && ctReq.Has(v1alpha5.CapacityTypeOnDemand) {
		newNodes[0].Requirements.Add(scheduling.NewRequirement(v1alpha5.LabelCapacityType, v1.NodeSelectorOpIn, v1alpha5.CapacityTypeSpot))
	}

	return Command{
		nodesToRemove:    []*v1.Node{node.Node},
		action:           actionReplace,
		replacementNodes: []*scheduling2.Machine{newNodes[0]},
	}, nil
}
