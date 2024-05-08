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

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	operatorlogging "sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
)

const MinInstanceTypesForSpotToSpotConsolidation = 15

// Compute command to execute spot-to-spot consolidation if:
//  1. The SpotToSpotConsolidation feature flag is set to true.
//  2. For single-node consolidation:
//     a. There are at least 15 cheapest instance type replacement options to consolidate.
//     b. The current candidate is NOT part of the first 15 cheapest instance types inorder to avoid repeated consolidation.
func (c *Controller) validateSpotToSpotConsolidation(ctx context.Context, candidates []*state.StateNode, results pscheduling.Results,
	candidatePrice float64) (bool, error) {

	// Spot consolidation is turned off.
	if !options.FromContext(ctx).FeatureGates.SpotToSpotConsolidation {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "SpotToSpotConsolidation is disabled, can't replace a spot node with a spot node")...)
		}
		return false, nil
	}

	// Since we are sure that the replacement nodeclaim considered for the spot candidates are spot, we will enforce it through the requirements.
	results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, v1beta1.CapacityTypeSpot))
	// All possible replacements for the current candidate compatible with spot offerings
	instanceTypeOptionsWithSpotOfferings :=
		results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions.Compatible(results.NewNodeClaims[0].Requirements)

	var incompatibleMinReqKey string
	// Possible replacements that are lower priced than the current candidate and the requirement that is not compatible with minValues
	results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions, incompatibleMinReqKey, _ =
		disruptionutils.FilterByPriceWithMinValues(instanceTypeOptionsWithSpotOfferings, results.NewNodeClaims[0].Requirements, candidatePrice)

	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) == 0 {
		if len(candidates) == 1 {
			if len(incompatibleMinReqKey) > 0 {
				c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("minValues requirement is not met for %s", incompatibleMinReqKey))...)
			} else {
				c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "Can't replace spot node with a cheaper spot node")...)
			}
		}
		// no instance types remain after filtering by price
		return false, nil
	}

	// For multi-node consolidation:
	// We don't have any requirement to check the remaining instance type flexibility, so exit early in this case.
	if len(candidates) > 1 {
		return true, nil
	}

	// For single-node consolidation:

	// We check whether we have 15 cheaper instances than the current candidate instance. If this is the case, we know the following things:
	//   1) The current candidate is not in the set of the 15 cheapest instance types and
	//   2) There were at least 15 options cheaper than the current candidate.
	if len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions) < MinInstanceTypesForSpotToSpotConsolidation {
		c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("SpotToSpotConsolidation requires %d cheaper instance type options than the current candidate to consolidate, got %d",
			MinInstanceTypesForSpotToSpotConsolidation, len(results.NewNodeClaims[0].NodeClaimTemplate.InstanceTypeOptions)))...)
		return false, nil
	}

	return true, nil
}

//nolint:gocyclo
func SimulateScheduling(ctx context.Context, kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner,
	recorder events.Recorder, candidates ...*state.StateNode,
) (pscheduling.Results, error) {
	nodes := cluster.Nodes()
	deletingNodes := nodes.Deleting()

	// We get the pods that are on nodes that are deleting
	// and track them separately for understanding simulation results
	deletingNodePods, err := deletingNodes.ReschedulablePods(ctx, kubeClient)
	if err != nil {
		return pscheduling.Results{}, fmt.Errorf("failed to get pods from deleting nodes, %w", err)
	}
	// Get all pending pods
	pods, err := provisioner.GetPendingPods(ctx)
	if err != nil {
		return pscheduling.Results{}, fmt.Errorf("determining pending pods, %w", err)
	}
	// Get the reschedulable pods on the candidates we're trying to consider deleting
	errs := make([]error, len(candidates))
	for i, candidate := range candidates {
		reschedulable, err := candidate.ReschedulablePods(ctx, kubeClient)
		if err != nil {
			errs[i] = err
			continue
		}
		pods = append(pods, reschedulable...)
	}
	pods = append(pods, deletingNodePods...)
	// Add a nop logger so we don't emit any logs from the scheduling simulation, as we're only simulating.
	ctx = logging.WithLogger(ctx, operatorlogging.NopLogger)

	scheduler, err := provisioner.NewScheduler(ctx, pods, nodes)
	if err != nil {
		return pscheduling.Results{}, fmt.Errorf("creating scheduler, %w", err)
	}
	deletingNodePodKeys := lo.SliceToMap(deletingNodePods, func(p *v1.Pod) (client.ObjectKey, interface{}) {
		return client.ObjectKeyFromObject(p), nil
	})

	results := scheduler.Solve(ctx, pods).TruncateInstanceTypes(pscheduling.MaxInstanceTypes)
	for _, n := range results.ExistingNodes {
		// We consider existing nodes for scheduling. When these nodes are unmanaged, their taint logic should
		// tell us if we can schedule to them or not; however, if these nodes are managed, we will still schedule to them
		// even if they are still in the middle of their initialization loop. In the case of disruption, we don't want
		// to proceed disrupting if our scheduling decision relies on nodes that haven't entered a terminal state.
		if !n.Initialized() {
			for _, p := range n.Pods {
				// Only add a pod scheduling error if it isn't on an already deleting node.
				// If the pod is on a deleting node, we assume one of two things has already happened:
				// 1. The node was manually terminated, at which the provisioning controller has scheduled or is scheduling a node
				//    for the pod.
				// 2. The node was chosen for a previous disruption command, we assume that the uninitialized node will come up
				//    for this command, and we assume it will be successful. If it is not successful, the node will become
				//    not terminating, and we will no longer need to consider these pods.
				if _, ok := deletingNodePodKeys[client.ObjectKeyFromObject(p)]; !ok {
					results.PodErrors[p] = fmt.Errorf("would schedule against a non-initialized node %s", n.Name())
				}
			}
		}
	}

	return results, nil
}
