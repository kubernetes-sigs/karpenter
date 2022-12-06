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

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

// consolidation is the base consolidation controller that provides common functionality used across the different
// consolidation methods.
type consolidation struct {
	clock                  clock.Clock
	cluster                *state.Cluster
	kubeClient             client.Client
	provisioner            *provisioning.Provisioner
	cloudProvider          cloudprovider.CloudProvider
	reporter               *Reporter
	lastConsolidationState int64
}

func makeConsolidation(clock clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cloudProvider cloudprovider.CloudProvider, reporter *Reporter) consolidation {
	return consolidation{
		clock:                  clock,
		cluster:                cluster,
		kubeClient:             kubeClient,
		provisioner:            provisioner,
		cloudProvider:          cloudProvider,
		reporter:               reporter,
		lastConsolidationState: 0,
	}
}

// consolidationTTL is the TTL between creating a consolidation command and validating that it still works.
const consolidationTTL = 15 * time.Second

// string is the string representation of the deprovisioner
func (c *consolidation) String() string {
	return metrics.ConsolidationReason
}

// RecordLastState is used to record the last state that the consolidation implementation failed to work in to allow
// skipping future consolidation attempts until the state changes.
func (c *consolidation) RecordLastState(currentState int64) {
	c.lastConsolidationState = currentState
}

func (c *consolidation) ShouldAttemptConsolidation() bool {
	// the last cluster consolidation wasn't able to improve things and nothing has changed regarding
	// the cluster that makes us think we would be successful now
	return c.lastConsolidationState != c.cluster.ClusterConsolidationState()
}

// sortAndFilterCandidates orders deprovisionable nodes by the disruptionCost, removing any that we already know won't
// be viable consolidation options.
func (c *consolidation) sortAndFilterCandidates(ctx context.Context, nodes []CandidateNode) ([]CandidateNode, error) {
	pdbs, err := NewPDBLimits(ctx, c.kubeClient)
	if err != nil {
		return nil, fmt.Errorf("tracking PodDisruptionBudgets, %w", err)
	}

	// filter out nodes that can't be terminated
	nodes = lo.Filter(nodes, func(cn CandidateNode, _ int) bool {
		if reason, canTerminate := canBeTerminated(cn, pdbs); !canTerminate {
			c.reporter.RecordUnconsolidatableReason(ctx, cn.Node, reason)
			return false
		}
		return true
	})

	sort.Slice(nodes, func(i int, j int) bool {
		return nodes[i].disruptionCost < nodes[j].disruptionCost
	})
	return nodes, nil
}

// ShouldDeprovision is a predicate used to filter deprovisionable nodes
func (c *consolidation) ShouldDeprovision(ctx context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, _ []*v1.Pod) bool {
	if val, ok := n.Node.Annotations[v1alpha5.DoNotConsolidateNodeAnnotationKey]; ok {
		c.reporter.RecordUnconsolidatableReason(ctx, n.Node, fmt.Sprintf("%s annotation exists", v1alpha5.DoNotConsolidateNodeAnnotationKey))
		return val != "true"
	}
	if provisioner == nil {
		c.reporter.RecordUnconsolidatableReason(ctx, n.Node, "provisioner is unknown")
		return false
	}
	if provisioner.Spec.Consolidation == nil || !ptr.BoolValue(provisioner.Spec.Consolidation.Enabled) {
		c.reporter.RecordUnconsolidatableReason(ctx, n.Node, fmt.Sprintf("provisioner %s has consolidation disabled", provisioner.Name))
		return false
	}
	return true
}

// ValidateCommand validates a command for a deprovisioner
func (c *consolidation) ValidateCommand(ctx context.Context, cmd Command, candidateNodes []CandidateNode) (bool, error) {
	// map from nodes we are about to remove back into candidate nodes with cluster state
	nodesToDelete := mapNodes(cmd.nodesToRemove, candidateNodes)
	// None of the chosen candidate nodes are valid for execution, so retry
	if len(nodesToDelete) == 0 {
		return false, nil
	}

	newNodes, allPodsScheduled, err := simulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, nodesToDelete...)
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
		if len(cmd.replacementMachines) == 0 {
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
	if len(cmd.replacementMachines) == 0 {
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
	if !instanceTypesAreSubset(cmd.replacementMachines[0].InstanceTypeOptions, newNodes[0].InstanceTypeOptions) {
		return false, nil
	}

	// Now we know:
	// - current scheduling simulation says to create a new node with types T = {T_0, T_1, ..., T_n}
	// - our lifecycle command says to create a node with types {U_0, U_1, ..., U_n} where U is a subset of T
	return true, nil
}

// computeConsolidation computes a consolidation action to take
//
// nolint:gocyclo
func (c *consolidation) computeConsolidation(ctx context.Context, nodes ...CandidateNode) (Command, error) {
	defer metrics.Measure(deprovisioningDurationHistogram.WithLabelValues("Replace/Delete"))()
	// Run scheduling simulation to compute consolidation option
	newNodes, allPodsScheduled, err := simulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, nodes...)
	if err != nil {
		// if a candidate node is now deleting, just retry
		if errors.Is(err, errCandidateNodeDeleting) {
			return Command{action: actionDoNothing}, nil
		}
		return Command{}, err
	}

	// if not all of the pods were scheduled, we can't do anything
	if !allPodsScheduled {
		// This method is used by multi-node consolidation as well, so we'll only report in the single node case
		if len(nodes) == 1 {
			c.reporter.RecordUnconsolidatableReason(ctx, nodes[0].Node, "not all pods would schedule")
		}
		return Command{action: actionDoNothing}, nil
	}

	// were we able to schedule all the pods on the inflight nodes?
	if len(newNodes) == 0 {
		return Command{
			nodesToRemove: lo.Map(nodes, func(n CandidateNode, _ int) *v1.Node { return n.Node }),
			action:        actionDelete,
		}, nil
	}

	// we're not going to turn a single node into multiple nodes
	if len(newNodes) != 1 {
		if len(nodes) == 1 {
			c.reporter.RecordUnconsolidatableReason(ctx, nodes[0].Node, fmt.Sprintf("can't remove without creating %d nodes", len(newNodes)))
		}
		return Command{action: actionDoNothing}, nil
	}

	// get the current node price based on the offering
	// fallback if we can't find the specific zonal pricing data
	nodesPrice, err := getNodePrices(nodes)
	if err != nil {
		return Command{}, fmt.Errorf("getting offering price from candidate node, %w", err)
	}
	newNodes[0].InstanceTypeOptions = filterByPrice(newNodes[0].InstanceTypeOptions, newNodes[0].Requirements, nodesPrice)
	if len(newNodes[0].InstanceTypeOptions) == 0 {
		if len(nodes) == 1 {
			c.reporter.RecordUnconsolidatableReason(ctx, nodes[0].Node, "can't replace with a cheaper node")
		}
		// no instance types remain after filtering by price
		return Command{action: actionDoNothing}, nil
	}

	// If the existing nodes are all spot and the replacement is spot, we don't consolidate.  We don't have a reliable
	// mechanism to determine if this replacement makes sense given instance type availability (e.g. we may replace
	// a spot node with one that is less available and more likely to be reclaimed).
	allExistingAreSpot := true
	for _, n := range nodes {
		if n.capacityType != v1alpha5.CapacityTypeSpot {
			allExistingAreSpot = false
		}
	}

	if allExistingAreSpot &&
		newNodes[0].Requirements.Get(v1alpha5.LabelCapacityType).Has(v1alpha5.CapacityTypeSpot) {
		if len(nodes) == 1 {
			c.reporter.RecordUnconsolidatableReason(ctx, nodes[0].Node, "can't replace a spot node with a spot node")
		}
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
		nodesToRemove:       lo.Map(nodes, func(n CandidateNode, _ int) *v1.Node { return n.Node }),
		action:              actionReplace,
		replacementMachines: newNodes,
	}, nil
}

// getNodePrices returns the sum of the prices of the given candidate nodes
func getNodePrices(nodes []CandidateNode) (float64, error) {
	var price float64
	for _, n := range nodes {
		offering, ok := n.instanceType.Offerings.Get(n.capacityType, n.zone)
		if !ok {
			return 0.0, fmt.Errorf("unable to determine offering for %s/%s/%s", n.instanceType.Name, n.capacityType, n.zone)
		}
		price += offering.Price
	}
	return price, nil
}
