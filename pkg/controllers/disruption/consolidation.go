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
	"sort"
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
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// consolidationTTL is the TTL between creating a consolidation command and validating that it still works.
const consolidationTTL = 15 * time.Second

// consolidation is the base consolidation controller that provides common functionality used across the different
// consolidation methods.
type consolidation struct {
	// Consolidation needs to be aware of the queue for validation
	queue                  *orchestration.Queue
	clock                  clock.Clock
	cluster                *state.Cluster
	kubeClient             client.Client
	provisioner            *provisioning.Provisioner
	cloudProvider          cloudprovider.CloudProvider
	recorder               events.Recorder
	lastConsolidationState time.Time
}

func makeConsolidation(clock clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cloudProvider cloudprovider.CloudProvider, recorder events.Recorder, queue *orchestration.Queue) consolidation {
	return consolidation{
		queue:         queue,
		clock:         clock,
		cluster:       cluster,
		kubeClient:    kubeClient,
		provisioner:   provisioner,
		cloudProvider: cloudProvider,
		recorder:      recorder,
	}
}

// sortAndFilterCandidates orders candidates by the disruptionCost, removing any that we already know won't
// be viable consolidation options.
func (c *consolidation) sortAndFilterCandidates(ctx context.Context, candidates []*Candidate) ([]*Candidate, error) {
	candidates, err := filterCandidates(ctx, c.kubeClient, c.recorder, candidates)
	if err != nil {
		return nil, fmt.Errorf("filtering candidates, %w", err)
	}

	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].disruptionCost < candidates[j].disruptionCost
	})
	return candidates, nil
}

// isConsolidated returns true if nothing has changed since markConsolidated was called.
func (c *consolidation) isConsolidated() bool {
	return c.lastConsolidationState.Equal(c.cluster.ConsolidationState())
}

// markConsolidated records the current state of the cluster.
func (c *consolidation) markConsolidated() {
	c.lastConsolidationState = c.cluster.ConsolidationState()
}

// ShouldDisrupt is a predicate used to filter candidates
func (c *consolidation) ShouldDisrupt(_ context.Context, cn *Candidate) bool {
	// TODO: Remove the check for do-not-consolidate at v1
	if cn.Annotations()[v1alpha5.DoNotConsolidateNodeAnnotationKey] == "true" {
		c.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("%s annotation exists", v1alpha5.DoNotConsolidateNodeAnnotationKey))...)
		return false
	}
	// If we don't have the "WhenUnderutilized" policy set, we should not do any of the consolidation methods, but
	// we should also not fire an event here to users since this can be confusing when the field on the NodePool
	// is named "consolidationPolicy"
	if cn.nodePool.Spec.Disruption.ConsolidationPolicy != v1beta1.ConsolidationPolicyWhenUnderutilized {
		return false
	}
	if cn.nodePool.Spec.Disruption.ConsolidateAfter != nil && cn.nodePool.Spec.Disruption.ConsolidateAfter.Duration == nil {
		c.recorder.Publish(disruptionevents.Unconsolidatable(cn.Node, cn.NodeClaim, fmt.Sprintf("NodePool %q has consolidation disabled", cn.nodePool.Name))...)
		return false
	}
	return true
}

// computeConsolidation computes a consolidation action to take
//
// nolint:gocyclo
func (c *consolidation) computeConsolidation(ctx context.Context, candidates ...*Candidate) (Command, sets.Set[*pscheduling.ExistingNode], error) {
	// Run scheduling simulation to compute consolidation option
	results, err := simulateScheduling(ctx, c.kubeClient, c.cluster, c.provisioner, candidates...)
	if err != nil {
		// if a candidate node is now deleting, just retry
		if errors.Is(err, errCandidateDeleting) {
			return Command{}, nil, nil
		}
		return Command{}, nil, err
	}

	// if not all of the pods were scheduled, we can't do anything
	if !results.AllNonPendingPodsScheduled() {
		// This method is used by multi-node consolidation as well, so we'll only report in the single node case
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, results.NonPendingPodSchedulingErrors())...)
		}
		return Command{}, nil, nil
	}
	// Only return the existing nodes that had pods scheduled to it
	nominatedNodes := sets.New[*pscheduling.ExistingNode](lo.Filter(results.ExistingNodes, func(n *pscheduling.ExistingNode, _ int) bool {
		return len(n.Pods) > 0
	})...)

	// were we able to schedule all the pods on the inflight candidates?
	if len(results.NewNodeClaims) == 0 {
		return Command{
			candidates: candidates,
		}, nominatedNodes, nil
	}

	// we're not going to turn a single node into multiple candidates
	if len(results.NewNodeClaims) != 1 {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, fmt.Sprintf("Can't remove without creating %d candidates", len(results.NewNodeClaims)))...)
		}
		return Command{}, nil, nil
	}

	// get the current node price based on the offering
	// fallback if we can't find the specific zonal pricing data
	candidatePrice, err := getCandidatePrices(candidates)
	if err != nil {
		return Command{}, nil, fmt.Errorf("getting offering price from candidate node, %w", err)
	}
	results.NewNodeClaims[0].InstanceTypeOptions = filterByPrice(results.NewNodeClaims[0].InstanceTypeOptions, results.NewNodeClaims[0].Requirements, candidatePrice)
	if len(results.NewNodeClaims[0].InstanceTypeOptions) == 0 {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "Can't replace with a cheaper node")...)
		}
		// no instance types remain after filtering by price
		return Command{}, nil, nil
	}

	// If the existing candidates are all spot and the replacement is spot, we don't consolidate.  We don't have a reliable
	// mechanism to determine if this replacement makes sense given instance type availability (e.g. we may replace
	// a spot node with one that is less available and more likely to be reclaimed).
	allExistingAreSpot := true
	for _, cn := range candidates {
		if cn.capacityType != v1beta1.CapacityTypeSpot {
			allExistingAreSpot = false
		}
	}

	if allExistingAreSpot &&
		results.NewNodeClaims[0].Requirements.Get(v1beta1.CapacityTypeLabelKey).Has(v1beta1.CapacityTypeSpot) {
		if len(candidates) == 1 {
			c.recorder.Publish(disruptionevents.Unconsolidatable(candidates[0].Node, candidates[0].NodeClaim, "Can't replace a spot node with a spot node")...)
		}
		return Command{}, nil, nil
	}

	// We are consolidating a node from OD -> [OD,Spot] but have filtered the instance types by cost based on the
	// assumption, that the spot variant will launch. We also need to add a requirement to the node to ensure that if
	// spot capacity is insufficient we don't replace the node with a more expensive on-demand node.  Instead the launch
	// should fail and we'll just leave the node alone.
	ctReq := results.NewNodeClaims[0].Requirements.Get(v1beta1.CapacityTypeLabelKey)
	if ctReq.Has(v1beta1.CapacityTypeSpot) && ctReq.Has(v1beta1.CapacityTypeOnDemand) {
		results.NewNodeClaims[0].Requirements.Add(scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, v1beta1.CapacityTypeSpot))
	}

	return Command{
		candidates:   candidates,
		replacements: results.NewNodeClaims,
	}, nominatedNodes, nil
}

// getCandidatePrices returns the sum of the prices of the given candidates
func getCandidatePrices(candidates []*Candidate) (float64, error) {
	var price float64
	for _, c := range candidates {
		offering, ok := c.instanceType.Offerings.Get(c.capacityType, c.zone)
		if !ok {
			return 0.0, fmt.Errorf("unable to determine offering for %s/%s/%s", c.instanceType.Name, c.capacityType, c.zone)
		}
		price += offering.Price
	}
	return price, nil
}
