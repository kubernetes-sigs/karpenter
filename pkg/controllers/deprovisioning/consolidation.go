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
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/utils/pod"
)

// Controller is the consolidation controller.  It is not a standard controller-runtime controller in that it doesn't
// have a reconcile method.
type Consolidation struct {
	kubeClient    client.Client
	cluster       *state.Cluster
	provisioner   *provisioning.Provisioner
	recorder      events.Recorder
	clock         clock.Clock
	cloudProvider cloudprovider.CloudProvider
}

const consolidationName = "consolidation"

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

// Skip nodes with consolidation disabled or are annotated as do-not-consolidate
func (c *Consolidation) shouldNotBeDeprovisioned(_ context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, _ []*v1.Pod) bool {
	return provisioner == nil || provisioner.Spec.Consolidation == nil || !ptr.BoolValue(provisioner.Spec.Consolidation.Enabled) ||
		n.Node.Annotations[v1alpha5.DoNotConsolidateNodeAnnotationKey] == "true"
}

// Sorts candidateNodes by disruption cost
func (c *Consolidation) sortCandidates(nodes []candidateNode) []candidateNode {
	sort.Slice(nodes, func(i int, j int) bool {
		return nodes[i].disruptionCost < nodes[j].disruptionCost
	})
	return nodes
}

//nolint:gocyclo
func (c *Consolidation) computeCommand(ctx context.Context, attempt int, candidates ...candidateNode) (deprovisioningCommand, error) {
	if attempt == 0 {
		emptyNodes := lo.Filter(candidates, func(n candidateNode, _ int) bool { return len(n.pods) == 0 })
		if len(emptyNodes) != 0 {
			return deprovisioningCommand{
				nodesToRemove: lo.Map(emptyNodes, func(n candidateNode, _ int) *v1.Node { return n.Node }),
				action:        actionDeleteEmpty,
				created:       c.clock.Now(),
			}, nil
		}
	}

	// If we get here, return nothingToDo as no candidateNodes are valid.
	if attempt >= len(candidates) {
		return deprovisioningCommand{action: actionDoNothing}, nil
	}
	node := candidates[attempt]

	pdbs, err := NewPDBLimits(ctx, c.kubeClient)
	if err != nil {
		return deprovisioningCommand{action: actionFailed}, fmt.Errorf("tracking PodDisruptionBudgets, %w", err)
	}
	// is this a node that we can terminate?  This check is meant to be fast so we can save the expense of simulated
	// scheduling unless its really needed
	if err = canBeTerminated(node, pdbs); err != nil {
		logging.FromContext(ctx).Debugf("cannot deprovision node %w", err)
		return deprovisioningCommand{action: actionNotPossible}, nil
	}

	defer metrics.Measure(deprovisioningDurationHistogram.WithLabelValues("Replace/Delete"))()
	// Run scheduling simulation to compute consolidation option
	newNodes, allPodsScheduled, err := simulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, node)
	if err != nil {
		// if a candidate node is now deleting, just retry
		if errors.Is(err, errCandidateNodeDeleting) {
			return deprovisioningCommand{action: actionDoNothing}, nil
		}
		return deprovisioningCommand{}, err
	}

	// if not all of the pods were scheduled, we can't do anything
	if !allPodsScheduled {
		return deprovisioningCommand{action: actionNotPossible}, nil
	}

	// were we able to schedule all the pods on the inflight nodes?
	if len(newNodes) == 0 {
		return deprovisioningCommand{
			nodesToRemove: []*v1.Node{node.Node},
			action:        actionDeleteConsolidation,
			created:       c.clock.Now(),
		}, nil
	}

	// we're not going to turn a single node into multiple nodes
	if len(newNodes) != 1 {
		return deprovisioningCommand{action: actionNotPossible}, nil
	}

	// get the current node price based on the offering
	// fallback if we can't find the specific zonal pricing data
	offering, ok := cloudprovider.GetOffering(node.instanceType, node.capacityType, node.zone)
	if !ok {
		return deprovisioningCommand{action: actionFailed}, fmt.Errorf("getting offering price from candidate node, %w", err)
	}
	newNodes[0].InstanceTypeOptions = filterByPrice(newNodes[0].InstanceTypeOptions, newNodes[0].Requirements, offering.Price)
	if len(newNodes[0].InstanceTypeOptions) == 0 {
		// no instance types remain after filtering by price
		return deprovisioningCommand{action: actionNotPossible}, nil
	}

	// If the existing node is spot and the replacement is spot, we don't consolidate.  We don't have a reliable
	// mechanism to determine if this replacement makes sense given instance type availability (e.g. we may replace
	// a spot node with one that is less available and more likely to be reclaimed).
	if node.capacityType == v1alpha5.CapacityTypeSpot &&
		newNodes[0].Requirements.Get(v1alpha5.LabelCapacityType).Has(v1alpha5.CapacityTypeSpot) {
		return deprovisioningCommand{action: actionNotPossible}, nil
	}

	// We are consolidating a node from OD -> [OD,Spot] but have filtered the instance types by cost based on the
	// assumption, that the spot variant will launch. We also need to add a requirement to the node to ensure that if
	// spot capacity is insufficient we don't replace the node with a more expensive on-demand node.  Instead the launch
	// should fail and we'll just leave the node alone.
	ctReq := newNodes[0].Requirements.Get(v1alpha5.LabelCapacityType)
	if ctReq.Has(v1alpha5.CapacityTypeSpot) && ctReq.Has(v1alpha5.CapacityTypeOnDemand) {
		newNodes[0].Requirements.Add(scheduling.NewRequirement(v1alpha5.LabelCapacityType, v1.NodeSelectorOpIn, v1alpha5.CapacityTypeSpot))
	}

	return deprovisioningCommand{
		nodesToRemove:   []*v1.Node{node.Node},
		action:          actionReplaceConsolidation,
		replacementNode: newNodes[0],
		created:         c.clock.Now(),
	}, nil
}

func (c *Consolidation) validateCommand(ctx context.Context, nodesToDelete []candidateNode, cmd deprovisioningCommand) (bool, error) {
	if cmd.action == actionDeleteEmpty {
		return c.validateDeleteEmpty(nodesToDelete)
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
		if cmd.replacementNode == nil {
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
	if cmd.replacementNode == nil {
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
	if !instanceTypesAreSubset(cmd.replacementNode.InstanceTypeOptions, newNodes[0].InstanceTypeOptions) {
		return false, nil
	}

	// Now we know:
	// - current scheduling simulation says to create a new node with types T = {T_0, T_1, ..., T_n}
	// - our lifecycle command says to create a node with types {U_0, U_1, ..., U_n} where U is a subset of T
	return true, nil
}

func (c *Consolidation) isExecutableCommand(cmd deprovisioningCommand) bool {
	return len(cmd.nodesToRemove) > 0 && (cmd.action == actionDeleteConsolidation ||
		cmd.action == actionReplaceConsolidation || cmd.action == actionDeleteEmpty)
}

func (c *Consolidation) string() string {
	return consolidationName
}

func (c *Consolidation) getTTL() time.Duration {
	return 15 * time.Second
}

func (c *Consolidation) deprovisionIncrementally() bool {
	return true
}

func canBeTerminated(node candidateNode, pdbs *PDBLimits) error {
	if !node.DeletionTimestamp.IsZero() {
		return fmt.Errorf("already being deleted")
	}
	if !pdbs.CanEvictPods(node.pods) {
		return fmt.Errorf("not eligible for termination due to PDBs")
	}
	return podsPreventEviction(node)
}

func podsPreventEviction(node candidateNode) error {
	for _, p := range node.pods {
		// don't care about pods that are finishing, finished or owned by the node
		if pod.IsTerminating(p) || pod.IsTerminal(p) || pod.IsOwnedByNode(p) {
			continue
		}

		if pod.HasDoNotEvict(p) {
			return fmt.Errorf("found do-not-evict pod")
		}

		if pod.IsNotOwned(p) {
			return fmt.Errorf("found pod with no controller")
		}
	}
	return nil
}

// validateDeleteEmpty validates that the given nodes are still empty
func (c *Consolidation) validateDeleteEmpty(nodesToDelete []candidateNode) (bool, error) {
	// the deletion of empty nodes is easy to validate, we just ensure that all the nodesToDelete are still empty and that
	// the node isn't a target of a recent scheduling simulation
	for _, n := range nodesToDelete {
		if len(n.pods) != 0 && !c.cluster.IsNodeNominated(n.Name) {
			return false, nil
		}
	}
	return true, nil
}
