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
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	pscheduling "github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

// Consolidation is the consolidation controller.
type Consolidation struct {
	kubeClient             client.Client
	cluster                *state.Cluster
	provisioner            *provisioning.Provisioner
	recorder               events.Recorder
	clock                  clock.Clock
	cloudProvider          cloudprovider.CloudProvider
	lastConsolidationState int64
}

const consolidationTTL = 15 * time.Second

func NewConsolidation(clk clock.Clock, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cp cloudprovider.CloudProvider, recorder events.Recorder, cluster *state.Cluster) *Consolidation {
	return &Consolidation{
		clock:         clk,
		kubeClient:    kubeClient,
		cluster:       cluster,
		provisioner:   provisioner,
		recorder:      recorder,
		cloudProvider: cp,
	}
}

// shouldDeprovision is a predicate used to filter deprovisionable nodes
func (c *Consolidation) ShouldDeprovision(_ context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, _ []*v1.Pod) bool {
	if val, ok := n.Node.Annotations[v1alpha5.DoNotConsolidateNodeAnnotationKey]; ok {
		return val != "true"
	}
	return provisioner != nil && provisioner.Spec.Consolidation != nil && ptr.BoolValue(provisioner.Spec.Consolidation.Enabled)
}

// computeCommand generates a deprovisioning command given deprovisionable nodes
func (c *Consolidation) ComputeCommand(ctx context.Context, candidates ...CandidateNode) (Command, error) {
	// the last cluster consolidation wasn't able to improve things and nothing has changed regarding
	// the cluster that makes us think we would be successful now
	if c.lastConsolidationState == c.cluster.ClusterConsolidationState() {
		return Command{action: actionDoNothing}, nil
	}
	// First sort the candidate nodes by disruption cost.
	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].disruptionCost < candidates[j].disruptionCost
	})

	// We use this timestamp for the Consolidation TTL so that our TTL logic waits one time per consolidation loop.
	validationTimestamp := time.Time{}
	var cmd Command
	var err error

	// First delete any empty nodes we see
	cmd, validationTimestamp, err = c.deleteEmpty(ctx, validationTimestamp, candidates...)
	if err != nil {
		return Command{action: actionFailed}, fmt.Errorf("deleting empty nodes for consolidation, %w", err)
	}
	// If there are empty nodes, delete the empty nodes for this provisioning loop.
	if cmd.action == actionDelete {
		return cmd, nil
	}

	pdbs, err := NewPDBLimits(ctx, c.kubeClient)
	if err != nil {
		return Command{action: actionFailed}, fmt.Errorf("tracking PodDisruptionBudgets, %w", err)
	}

	for _, candidate := range candidates {
		// is this a node that we can terminate?  This check is meant to be fast so we can save the expense of simulated
		// scheduling unless its really needed
		if !canBeTerminated(candidate, pdbs) {
			continue
		}
		// do a simulated scheduling loop to check if Consolidation is possible
		cmd, validationTimestamp, err = c.computeConsolidation(ctx, validationTimestamp, candidate)
		if err != nil {
			return Command{action: actionFailed}, err
		}

		if err := c.waitForTTL(ctx, validationTimestamp); err != nil {
			return Command{action: actionFailed}, fmt.Errorf("context canceled during consolidation TTL")
		}

		ok, err := c.validateConsolidation(ctx, cmd)
		if err != nil {
			return Command{action: actionFailed}, fmt.Errorf("determining candidate node, %w", err)
		}
		if !ok {
			continue
		}
		return cmd, nil
	}
	// If none of the candidates were able to be deprovisioned, return nothingToDo
	return Command{action: actionDoNothing}, nil
}

// deleteEmpty returns a deprovisioningCommmand if there are empty nodes.
func (c *Consolidation) deleteEmpty(ctx context.Context, validationTimestamp time.Time, candidates ...CandidateNode) (Command, time.Time, error) {
	emptyNodes := lo.Filter(candidates, func(n CandidateNode, _ int) bool { return len(n.pods) == 0 })
	if len(emptyNodes) == 0 {
		return Command{action: actionDoNothing}, validationTimestamp, nil
	}

	cmd := Command{
		nodesToRemove: lo.Map(emptyNodes, func(n CandidateNode, _ int) *v1.Node { return n.Node }),
		action:        actionDelete,
	}

	validationTimestamp = lo.Ternary(validationTimestamp.IsZero(), c.clock.Now(), validationTimestamp)
	if err := c.waitForTTL(ctx, validationTimestamp); err != nil {
		return Command{action: actionFailed}, validationTimestamp, fmt.Errorf("determining candidate node %w", err)
	}

	ok, err := c.validateDeleteEmpty(ctx, cmd)
	if err != nil {
		return Command{action: actionFailed}, validationTimestamp, err
	}
	if !ok {
		return Command{action: actionDoNothing}, validationTimestamp, nil
	}
	return cmd, validationTimestamp, nil
}

// computeConsolidation computes a consolidation action to take
//
// nolint:gocyclo
func (c *Consolidation) computeConsolidation(ctx context.Context, validationTimestamp time.Time, node CandidateNode) (Command, time.Time, error) {
	defer metrics.Measure(deprovisioningDurationHistogram.WithLabelValues("Replace/Delete"))()
	// Run scheduling simulation to compute consolidation option
	newNodes, allPodsScheduled, err := simulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, node)
	if err != nil {
		// if a candidate node is now deleting, just retry
		if errors.Is(err, errCandidateNodeDeleting) {
			return Command{action: actionDoNothing}, validationTimestamp, nil
		}
		return Command{}, validationTimestamp, err
	}

	// if not all of the pods were scheduled, we can't do anything
	if !allPodsScheduled {
		return Command{action: actionDoNothing}, validationTimestamp, nil
	}

	// were we able to schedule all the pods on the inflight nodes?
	if len(newNodes) == 0 {
		validationTimestamp = lo.Ternary(validationTimestamp.IsZero(), c.clock.Now(), validationTimestamp)
		return Command{
			nodesToRemove: []*v1.Node{node.Node},
			action:        actionDelete,
		}, validationTimestamp, nil
	}

	// we're not going to turn a single node into multiple nodes
	if len(newNodes) != 1 {
		return Command{action: actionDoNothing}, validationTimestamp, nil
	}

	// get the current node price based on the offering
	// fallback if we can't find the specific zonal pricing data
	offering, ok := cloudprovider.GetOffering(node.instanceType, node.capacityType, node.zone)
	if !ok {
		return Command{action: actionFailed}, validationTimestamp, fmt.Errorf("getting offering price from candidate node, %w", err)
	}
	newNodes[0].InstanceTypeOptions = filterByPrice(newNodes[0].InstanceTypeOptions, newNodes[0].Requirements, offering.Price)
	if len(newNodes[0].InstanceTypeOptions) == 0 {
		// no instance types remain after filtering by price
		return Command{action: actionDoNothing}, validationTimestamp, nil
	}

	// If the existing node is spot and the replacement is spot, we don't consolidate.  We don't have a reliable
	// mechanism to determine if this replacement makes sense given instance type availability (e.g. we may replace
	// a spot node with one that is less available and more likely to be reclaimed).
	if node.capacityType == v1alpha5.CapacityTypeSpot &&
		newNodes[0].Requirements.Get(v1alpha5.LabelCapacityType).Has(v1alpha5.CapacityTypeSpot) {
		return Command{action: actionDoNothing}, validationTimestamp, nil
	}

	// We are consolidating a node from OD -> [OD,Spot] but have filtered the instance types by cost based on the
	// assumption, that the spot variant will launch. We also need to add a requirement to the node to ensure that if
	// spot capacity is insufficient we don't replace the node with a more expensive on-demand node.  Instead the launch
	// should fail and we'll just leave the node alone.
	ctReq := newNodes[0].Requirements.Get(v1alpha5.LabelCapacityType)
	if ctReq.Has(v1alpha5.CapacityTypeSpot) && ctReq.Has(v1alpha5.CapacityTypeOnDemand) {
		newNodes[0].Requirements.Add(scheduling.NewRequirement(v1alpha5.LabelCapacityType, v1.NodeSelectorOpIn, v1alpha5.CapacityTypeSpot))
	}

	validationTimestamp = lo.Ternary(validationTimestamp.IsZero(), c.clock.Now(), validationTimestamp)
	return Command{
		nodesToRemove:    []*v1.Node{node.Node},
		action:           actionReplace,
		replacementNodes: []*pscheduling.Node{newNodes[0]},
	}, validationTimestamp, nil
}

// waitForTTL is used to wait a consolidationTTL given the initial timestamp of the deprovisioning loop
func (c *Consolidation) waitForTTL(ctx context.Context, validationTimestamp time.Time) error {
	remainingDelay := consolidationTTL - c.clock.Since(validationTimestamp)
	if remainingDelay > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled during consolidation TTL")
		case <-c.clock.After(remainingDelay):
		}
	}
	return nil
}

// validateCommand validates a command for a deprovisioner
func (c *Consolidation) validateConsolidation(ctx context.Context, cmd Command) (bool, error) {
	candidateNodes, err := candidateNodes(ctx, c.cluster, c.kubeClient, c.clock, c.cloudProvider, c.ShouldDeprovision)
	if err != nil {
		return false, fmt.Errorf("determining candidate node, %w", err)
	}
	nodes := mapNodes(cmd.nodesToRemove, candidateNodes)
	// None of the chosen candidate nodes are valid for execution, so retry
	if len(nodes) == 0 {
		return false, nil
	}

	newNodes, allPodsScheduled, err := simulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, nodes...)
	if err != nil {
		return false, fmt.Errorf("simluating scheduling, %w", err)
	}
	if !allPodsScheduled {
		return false, nil
	}

	// We want to ensure that the re-simulated scheduling using the current cluster state produces the same result.
	// There are three possible options for the number of new nodesToDelete that we need to handle:
	// len(newNodes) == 0, as long as we weren't expecting a new node, this is valid
	// len(newNodes) > 1, something in the cluster changed so that the nodesToDelete we were going to delete can no longer
	//                    be deleted without producing more than one node
	// len(newNodes) == 1, as long as the node looks like what we were expecting, this is valid
	if len(newNodes) == 0 {
		if len(cmd.replacementNodes) == 0 {
			// scheduling produced zero new nodes and we weren't expecting any, so this is valid.
			return true, nil
		}
		// if it produced no new nodes, but we were expecting one we should re-simulate as there is likely a better
		// consolidation option now
		return false, nil
	}

	// we need more than one replacement node which is never valid currently (all of our node replacement is m->1, never m->n)
	if len(newNodes) > 1 {
		return false, nil
	}

	// we now know that scheduling simulation wants to create one new node
	if len(cmd.replacementNodes) == 0 {
		// but we weren't expecting any new nodes, so this is invalid
		return false, nil
	}

	// We know that the scheduling simulation wants to create a new node and that the command we are verifying wants
	// to create a new node. The scheduling simulation doesn't apply any filtering to instance types, so it may include
	// instance types that we don't want to launch which were filtered out when the lifecycleCommand was created.  To
	// check if our lifecycleCommand is valid, we just want to ensure that the list of instance types we are considering
	// creating are a subset of what scheduling says we should create.
	//
	// This is necessary since consolidation only wants cheaper nodes.  Suppose consolidation determined we should delete
	// a 4xlarge and replace it with a 2xlarge. If things have changed and the scheduling simulation we just performed
	// now says that we need to launch a 4xlarge. It's still launching the correct number of nodes, but it's just
	// as expensive or possibly more so we shouldn't validate.
	if !instanceTypesAreSubset(cmd.replacementNodes[0].InstanceTypeOptions, newNodes[0].InstanceTypeOptions) {
		return false, nil
	}

	// Now we know:
	// - current scheduling simulation says to create a new node with types T = {T_0, T_1, ..., T_n}
	// - our lifecycle command says to create a node with types {U_0, U_1, ..., U_n} where U is a subset of T
	return true, nil
}

// validateDeleteEmpty validates that the given nodes are still empty
func (c *Consolidation) validateDeleteEmpty(ctx context.Context, cmd Command) (bool, error) {
	candidateNodes, err := candidateNodes(ctx, c.cluster, c.kubeClient, c.clock, c.cloudProvider, c.ShouldDeprovision)
	if err != nil {
		return false, fmt.Errorf("determining candidate node, %w", err)
	}
	nodes := mapNodes(cmd.nodesToRemove, candidateNodes)
	// the deletion of empty nodes is easy to validate, we just ensure that all the nodesToDelete are still empty and that
	// the node isn't a target of a recent scheduling simulation
	for _, n := range nodes {
		if len(n.pods) != 0 && !c.cluster.IsNodeNominated(n.Name) {
			return false, nil
		}
	}
	return true, nil
}

// string is the string representation of the deprovisioner
func (c *Consolidation) String() string {
	return metrics.ConsolidationReason
}
