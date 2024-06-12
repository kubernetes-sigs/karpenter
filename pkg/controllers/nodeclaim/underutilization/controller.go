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

package underutilization

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
	provutils "sigs.k8s.io/karpenter/pkg/utils/provisioning"
)

const (
	maxConcurrentNominations = 5
	maxBatchSize             = 100

	singleTimeout = 3 * time.Minute
	multiTimeout  = 1 * time.Minute
)

type Controller struct {
	kubeClient             client.Client
	cluster                *state.Cluster
	recorder               events.Recorder
	provisioner            *provisioning.Provisioner
	cloudProvider          cloudprovider.CloudProvider
	clk                    clock.Clock
	instanceTypes          map[string]map[string]*cloudprovider.InstanceType
	lastConsolidationState time.Time
}

func NewController(clk clock.Clock, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cp cloudprovider.CloudProvider, recorder events.Recorder, cluster *state.Cluster,
) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cluster:       cluster,
		recorder:      recorder,
		provisioner:   provisioner,
		clk:           clk,
		cloudProvider: cp,
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("underutilization").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

func (c *Controller) Name() string {
	return "nodeclaim.underutilization"
}

// IsConsolidated returns true if nothing has changed since markConsolidated was called.
func (c *Controller) IsConsolidated() bool {
	return c.lastConsolidationState.Equal(c.cluster.ConsolidationState())
}

// markConsolidated records the current state of the cluster.
func (c *Controller) markConsolidated() {
	c.lastConsolidationState = c.cluster.ConsolidationState()
}

// Reconcile will try to find consolidation decisions in the cluster:
//  1. First get the active consolidation decisions in the cluster and see if any decisions
//     can be invalidated, removing the Underutilization condition from the nodeclaims if invalid.
//  2. If there are no actively progressing consolidations, do not do anything until cluster
//     state is no longer in a consolidated state. This is important to reduce resource usage
//     when the cluster is idle.
//  3. If there are 5 or more actively progressing consolidations, exit early, as we should just
//     wait for these to finish.
//  4. Compute a consolidation decision with scheduling simulations
//  5. If we find a valid decision, mark all node claims as Underutilized.
func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	noms := c.getActiveConsolidations(ctx)
	// If there are no active nominations and it's been less than 5 minutes since the last time
	// the cluster was marked as consolidated, wait.
	if len(noms) == 0 && c.IsConsolidated() {
		return reconcile.Result{RequeueAfter: time.Until(c.cluster.ConsolidationState().Add(5 * time.Minute))}, nil
	}
	// If there are too many nominations, see if we can quickly invalidate any existing nominations
	if len(noms) >= maxConcurrentNominations {
		for _, cmd := range noms {
			// If any nomination is invalid, remove the nomination and requeue
			// Short circuit and only unmark one of the commands to optimistically save work done
			if err := c.validateCandidates(ctx, cmd.Candidates...); err != nil {
				err = multierr.Append(err, c.setNodeClaimsUnderutilized(ctx, false, "", cmd.Candidates...))
				return reconcile.Result{}, fmt.Errorf("validating candidates, %w", err)
			}
		}
		// If we can't invalidate any consolidations, we shouldn't enqueue another. Try again later.
		return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
	}

	newCandidateSet, err := c.computeConsolidation(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("computing consolidation decision, %w", err)
	}
	// we failed to find a consolidation decision
	if len(newCandidateSet) == 0 {
		c.markConsolidated()
		return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if err := c.setNodeClaimsUnderutilized(ctx, true, uuid.NewUUID(), newCandidateSet...); err != nil {
		return reconcile.Result{}, fmt.Errorf("propagating nomination, %w", err)
	}

	return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
}

// returns a map of command-id to list of candidate nodeclaim names
// returned sorted by creation timestamp
func (c *Controller) getActiveConsolidations(ctx context.Context) map[string]disruption.ConsolidationCommand {
	nodePools := c.cluster.NodePools()
	commands := map[string]disruption.ConsolidationCommand{}
	lo.ForEach(c.cluster.Nodes().Disruptable(ctx, c.kubeClient, c.recorder), func(s *state.StateNode, _ int) {
		// if the nodeclaim doesn't have the underutilized status condition, it's not part of
		// an active consolidation.
		cond := s.NodeClaim.StatusConditions().Get(v1beta1.ConditionTypeUnderutilized)
		if !cond.IsTrue() {
			return
		}
		// If the nodepool isn't found, we assume it's deleted or cluster state hasn't seen it yet.
		// If it was deleted, we rely on the termination controller to clean this up
		np, found := nodePools[s.Node.Labels[v1beta1.NodePoolLabelKey]]
		if !found {
			return
		}
		cmd := commands[cond.Message]
		// Add the command ID in idempotently
		cmd.CommandID = cond.Message
		cmd.Candidates = append(cmd.Candidates, s)
		// Use the larger consolidateAfter when batching
		cmd.ConsolidateAfter = lo.Max([]time.Duration{cmd.ConsolidateAfter, *np.Spec.Disruption.ConsolidateAfter.Duration})
		commands[cmd.CommandID] = cmd
	})
	return commands
}

// Returns true if the candidates should be unmarked if used to validate existing commands.
// Returns a nil error if the candidates are valid.
// This intentionally does a quick check rather than doing a scheduling simulation.
// Candidates are considered invalid if:
// 1. They're not disruptable from cluster state's perspective
// 2. ConsolidateAfter is disabled
// 3. Is NotEmpty, but it's NodePool only allows WhenEmpty consolidation
// 4. NodePool Disruption Budgets do not allow any disruption
func (c *Controller) validateCandidates(ctx context.Context, candidates ...*state.StateNode) error {
	newCandidates := lo.Filter(candidates, func(candidate *state.StateNode, _ int) bool {
		return candidate.IsDisruptable(ctx, c.kubeClient, c.recorder) != nil
	})
	if len(newCandidates) != len(candidates) {
		return fmt.Errorf("detected non-disruptable candidates")
	}

	nodePools := c.cluster.NodePools()
	// Group candidates by their NodePool so that we can calculate allowed disruptions
	nodePoolsToCandidates := lo.GroupBy(newCandidates, func(candidate *state.StateNode) string {
		return candidate.Labels()[v1beta1.NodePoolLabelKey]
	})

	for _, cn := range newCandidates {
		// If the NodePool isn't using WhenEmpty, checking if it's an invalid candidate can't be done quickly.
		if nodePools[cn.Labels()[v1beta1.NodePoolLabelKey]].Spec.Disruption.ConsolidationPolicy != v1beta1.ConsolidationPolicyWhenEmpty {
			continue
		}
		pods, err := cn.ReschedulablePods(ctx, c.kubeClient)
		if err != nil {
			return fmt.Errorf("getting candidate pods, %w", err)
		}
		if len(pods) > 0 {
			return fmt.Errorf("found non-empty node with NodePool consolidationPolicy set to %q", v1beta1.ConsolidationPolicyWhenEmpty)
		}
	}

	// Cross reference each node to its nodepool:
	// 1. check if consolidation is enabled
	// 2. check if node disruption budgets allowed disruptions is 0
	for npName := range nodePoolsToCandidates {
		nodePool := nodePools[npName]
		if nodePool.Spec.Disruption.ConsolidateAfter.Duration == nil {
			return fmt.Errorf("consolidation is not enabled for nodepool %s", npName)
		}
		// If the nodepool has no allowed disruptions, we know we can't disrupt this node
		if c.cluster.AllowedNodePoolDisruptions(ctx, npName, c.clk) == 0 {
			return fmt.Errorf("nodepool %q has no allowed disruptions", npName)
		}
	}

	return nil
}

// computeConsolidation optimistically tries to find a consolidation decision:
//  1. Iterate through the candidate nodes one at a time
//     a. If the node cannot be consolidated, continue to the next node in the list.
//  2. If the currently considered node can be consolidated, start increasing the batch size.
//  3. Increase the batch size by a factor of 2 for every valid scheduling simulation.
//     a. With a max batch size of 100, this means the max number of scheduling simulations for each starting point will be
//     1 -> 2 -> 4 -> 8 -> 16 -> 32 -> 64 -> 100 = 8
func (c *Controller) computeConsolidation(ctx context.Context) ([]*state.StateNode, error) {
	candidates := c.Candidates(ctx)
	lastValidMax := -1
	start := c.clk.Now()
	for i := range candidates {
		// add a timeout for finding a single valid command.
		if c.clk.Since(start) > singleTimeout {
			ConsolidationTimeoutTotalCounter.Inc()
			break
		}
		batchSize := 1
		multiBatchStart := c.clk.Now()
		// Get the subset of candidates at indices [i, i + batchSize), and do a scheduling simulation. If we succeed, double
		// the size of the subset by doubling the batch size, where the batch size will be <= len(candidates) and <= 100.
		for ; batchSize <= maxBatchSize; batchSize = lo.Clamp(batchSize*2, 1, maxBatchSize) {
			// add a timeout on increasing the batch size.
			if c.clk.Since(multiBatchStart) > multiTimeout {
				ConsolidationTimeoutTotalCounter.Inc()
				break
			}
			maxIndex := lo.Clamp(i+batchSize, 0, len(candidates))
			nodesToConsolidate := lo.Slice(candidates, i, maxIndex)
			results, err := provutils.SimulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, c.recorder, nodesToConsolidate...)
			if err != nil {
				return nil, fmt.Errorf("failed to simulate scheduling, %w", err)
			}
			valid, err := c.analyzeResults(ctx, results, nodesToConsolidate)
			if err != nil {
				return nil, fmt.Errorf("analyzing scheduling simulation results, %w", err)
			}
			// If not valid, don't double the batch size, and don't continue to simulate for this starting index
			if !valid {
				break
			}
			// If the command was valid, set the lastValidMax here
			lastValidMax = maxIndex
			// If we've reached the end of the list, we don't want to do another iteration of this loop
			if maxIndex == len(candidates) {
				break
			}
		}
		// If we had a valid command created as part of the previous scheduling simulations,
		// break the for loop and return that subset of candidates
		if lastValidMax != -1 {
			return candidates[i:lastValidMax], nil
		}
	}
	// If we reach here, this means we never found a consolidation decision, so we return no candidates.
	return nil, nil
}

// Candidates returns a list of the stateNodes ordered by their disruption cost
func (c *Controller) Candidates(ctx context.Context) []*state.StateNode {
	candidates := c.cluster.Nodes().Disruptable(ctx, c.kubeClient, c.recorder)
	candidatesToPods := candidates.ReschedulablePodsForStateNodes(ctx, c.kubeClient)
	nodePools := c.cluster.NodePools()
	sort.Slice(candidates, func(i, j int) bool {
		iDisruptionCost := disruptionutils.ReschedulingCost(ctx, candidatesToPods[candidates[i]]) *
			disruptionutils.LifetimeRemaining(c.clk, nodePools[candidates[i].Labels()[v1beta1.NodePoolLabelKey]], candidates[i].NodeClaim)
		jDisruptionCost := disruptionutils.ReschedulingCost(ctx, candidatesToPods[candidates[j]]) *
			disruptionutils.LifetimeRemaining(c.clk, nodePools[candidates[j].Labels()[v1beta1.NodePoolLabelKey]], candidates[j].NodeClaim)
		return iDisruptionCost < jDisruptionCost
	})
	return candidates
}

// Analyze results will look at the consolidation command, and return true if it should be executed
//
//nolint:gocyclo
func (c *Controller) analyzeResults(ctx context.Context, results pscheduling.Results, candidates []*state.StateNode) (bool, error) {
	// if not all of the pods were scheduled, don't continue
	if !results.AllNonPendingPodsScheduled() {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, results.NonPendingPodSchedulingErrors())...)
		}
		return false, nil
	}

	// return true if there are no replacements. This means that we were able to reschedule all pods, and don't need to check if the consolidation
	// will save on cost.
	if len(results.NewNodeClaims) == 0 {
		return true, nil
	}

	// do not turn a single node into multiple candidates
	if len(results.NewNodeClaims) != 1 {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("Can't remove without creating %d candidates", len(results.NewNodeClaims)))...)
		}
		return false, nil
	}

	// get the current node price based on the offering
	// fallback if we can't find the specific zonal pricing data
	candidatePrice, err := c.getCandidatePrices(ctx, candidates...)
	if err != nil {
		return false, fmt.Errorf("getting offering price from candidate node, %w", err)
	}

	// Check if there are any on-demand nodes, so we can know if we're doing spot to spot consolidation
	_, allExistingAreSpot := lo.Find(candidates, func(candidate *state.StateNode) bool {
		return candidate.Labels()[v1beta1.CapacityTypeLabelKey] == v1beta1.CapacityTypeOnDemand
	})

	// sort the instanceTypes by price before we take any actions like truncation for spot-to-spot consolidation or finding the nodeclaim
	// that meets the minimum requirement after filteringByPrice
	results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions = results.NewNodeClaims[0].InstanceTypeOptions.OrderByPrice(results.NewNodeClaims[0].Requirements)

	if allExistingAreSpot &&
		results.NewNodeClaims[0].Requirements.Get(v1beta1.CapacityTypeLabelKey).Has(v1beta1.CapacityTypeSpot) {
		return c.validateSpotToSpotConsolidation(ctx, candidates, results, candidatePrice)
	}

	// filterByPriceWithMinValues returns the instanceTypes that are lower priced than the current candidate and the requirement for the NodeClaim that does not meet minValues.
	// If we use this directly for spot-to-spot consolidation, we are bound to get repeated consolidations because the strategy that chooses to launch the spot instance from the list does
	// it based on availability and price which could result in selection/launch of non-lowest priced instance in the list. So, we would keep repeating this loop till we get to lowest priced instance
	// causing churns and landing onto lower available spot instance ultimately resulting in higher interruptions.
	results.NewNodeClaims[0], err = results.NewNodeClaims[0].RemoveInstanceTypeOptionsByPriceAndMinValues(results.NewNodeClaims[0].Requirements, candidatePrice)

	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) == 0 {
		if len(candidates) == 1 {
			if err != nil {
				c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("minValues requirement not met, %s", err.Error()))...)
			} else {
				c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "Can't replace with a cheaper node")...)
			}
		}
		return false, nil
	}
	return true, nil
}

// propagates the consolidation decision to cluster state and also marks all
// associated nodeclaims as underutilized
func (c *Controller) setNodeClaimsUnderutilized(ctx context.Context, setTrue bool, commandID types.UID, candidates ...*state.StateNode) error {
	for i := range candidates {
		nc := candidates[i].NodeClaim
		cond := nc.StatusConditions().Get(v1beta1.ConditionTypeUnderutilized)
		// If we want to set the condition and it's already true
		// or if we don't want to set the condition and it isn't true
		// e.g. (T && T) || (F && F), then do nothing
		if (setTrue && cond.IsTrue()) || (!setTrue && !cond.IsTrue()) {
			continue
		}
		deepCopy := nc.DeepCopy()
		if setTrue {
			nc.StatusConditions().SetTrueWithReason(v1beta1.ConditionTypeUnderutilized, v1beta1.ConditionTypeUnderutilized, (string)(commandID))
		} else {
			err := nc.StatusConditions().Clear(v1beta1.ConditionTypeUnderutilized)
			if err != nil {
				return fmt.Errorf("clearing underutilized condition from nodeclaim %q", nc.Name)
			}
		}
		if err := c.kubeClient.Status().Patch(ctx, nc, client.MergeFrom(deepCopy)); err != nil {
			return fmt.Errorf("patching nodeclaim, %w", err)
		}
		logging.FromContext(ctx).With("nodeclaim", nc.Name).Debugf("%s nodeclaim underutilization status condition", lo.Ternary(setTrue, "set", "removed"))
	}
	return nil
}

// getCandidatePrices returns the sum of the prices of the given candidates
func (c *Controller) getCandidatePrices(ctx context.Context, candidates ...*state.StateNode) (float64, error) {
	c.refreshInstanceTypeMapping(ctx)
	var price float64
	for _, candidate := range candidates {
		npName := candidate.Labels()[v1beta1.NodePoolLabelKey]
		itName := candidate.Labels()[v1.LabelInstanceTypeStable]
		ct := candidate.Labels()[v1beta1.CapacityTypeLabelKey]
		zone := candidate.Labels()[v1.LabelTopologyZone]
		reqs := scheduling.NewRequirements(
			scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, ct),
			scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, zone),
		)
		offs := c.instanceTypes[npName][itName].Offerings.Available().Compatible(reqs)
		if len(offs) == 0 {
			return 0.0, fmt.Errorf("unable to determine offering for %s/%s/%s", itName, ct, zone)
		}
		// We choose most expensive here rather than cheapest, since we assume that if there are multiple matching offerings
		// we get the most expensive, to ensure we only make cost saving decisions.
		price += offs.MostExpensive().Price
	}
	return price, nil
}

// refreshInstanceTypeMapping tries to update the Controller's NodePoolToInstanceTypesMap
// If the NodePool's instance types can't be discovered, the data is simply omitted.
func (c *Controller) refreshInstanceTypeMapping(ctx context.Context) {
	nodePoolToInstanceTypesMap := map[string]map[string]*cloudprovider.InstanceType{}
	var errs error
	failedNodePools := []string{}
	for _, np := range c.cluster.NodePools() {
		nodePoolInstanceTypes, err := c.cloudProvider.GetInstanceTypes(ctx, np)
		if err != nil {
			// don't error out on building the node pool, we just won't be able to handle any nodes that
			// were created by it
			errs = multierr.Append(errs, err)
			failedNodePools = append(failedNodePools, np.Name)
			continue
		}
		if len(nodePoolInstanceTypes) == 0 {
			continue
		}
		nodePoolToInstanceTypesMap[np.Name] = map[string]*cloudprovider.InstanceType{}
		for _, it := range nodePoolInstanceTypes {
			nodePoolToInstanceTypesMap[np.Name][it.Name] = it
		}
	}
	if errs != nil {
		// only log, so that we can continue with the rest of the nodePools that succeeded
		logging.FromContext(ctx).With("nodePoolNames", strings.Join(failedNodePools, ",")).Errorf("listing instance types, %s", errs)
	}
	c.instanceTypes = nodePoolToInstanceTypesMap
}
