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
	"fmt"
	"maps"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

type nodeClaimPriceSnapshot struct {
	expectedNewConsumers sets.Set[string]
	claims               map[string]*v1.NodeClaim
	states               map[string]cloudprovider.NodeClaimState
	scopes               map[string]string
}

func refreshNodeClaimPrices(ctx context.Context, cluster *state.Cluster, provider cloudprovider.CloudProvider, additional ...*v1.NodeClaim) *nodeClaimPriceSnapshot {
	snapshot := &nodeClaimPriceSnapshot{claims: map[string]*v1.NodeClaim{}, states: map[string]cloudprovider.NodeClaimState{}, scopes: map[string]string{}}
	snapshot.claims = mutableNodeClaims(cluster.DeepCopyNodes(), additional)
	claims := []*v1.NodeClaim{}
	for _, nc := range snapshot.claims {
		claims = append(claims, nc)
	}
	if len(claims) == 0 {
		return snapshot
	}
	observations, err := cloudprovider.RefreshNodeClaimStates(ctx, provider, claims)
	if err != nil {
		log.FromContext(ctx).Error(err, "refreshing running-node costs; deferring affected consolidation")
	}
	for id, observation := range observations {
		snapshot.scopes[id] = observation.Scope
		if _, ok := snapshot.claims[id]; ok && observation.ValidAt(time.Now()) {
			observation.Labels = maps.Clone(observation.Labels)
			snapshot.states[id] = observation
		}
	}
	return snapshot
}

func (s *nodeClaimPriceSnapshot) unavailablePools() sets.Set[string] {
	result := sets.New[string]()
	unknownScopes := sets.New[string]()
	for id, nc := range s.claims {
		if _, ok := s.states[id]; !ok {
			result.Insert(nc.Labels[v1.NodePoolLabelKey])
			if s.scopes[id] != "" {
				unknownScopes.Insert(s.scopes[id])
			}
		}
	}
	for id, nc := range s.claims {
		if unknownScopes.Has(s.scopes[id]) {
			result.Insert(nc.Labels[v1.NodePoolLabelKey])
		}
	}
	return result
}

// validate compares all consumers contributing to the selected pools' costs and
// all peers sharing their reservation scope. Coverage reassignment across pools
// invalidates the original economics even before Kubernetes watches converge.
func (s *nodeClaimPriceSnapshot) validate(fresh *nodeClaimPriceSnapshot, pools sets.Set[string]) error {
	scopes := s.relevantScopes(pools).Union(fresh.relevantScopes(pools))
	ids := s.relevantIDs(pools, scopes).Union(fresh.relevantIDs(pools, scopes))
	for id := range ids {
		if fresh.expectedNewConsumers.Has(id) {
			continue
		}
		if !s.sameState(fresh, id) {
			return NewValidationError(serrors.Wrap(fmt.Errorf("running-node coverage or price changed; recompute consolidation"), "provider-id", id))
		}
	}
	return nil
}

func validateCommandPrices(ctx context.Context, cmd Command, cluster *state.Cluster, provider cloudprovider.CloudProvider, kubeClient client.Client) error {
	if cmd.Reason() != v1.DisruptionReasonUnderutilized {
		return nil
	}
	pools := sets.New[string]()
	var baseline *nodeClaimPriceSnapshot
	for _, candidate := range cmd.Candidates {
		pools.Insert(candidate.NodePool.Name)
		if candidate.priceSnapshot != nil {
			baseline = candidate.priceSnapshot
		}
	}
	if baseline == nil {
		return nil
	}
	replacements, err := getReplacementClaims(ctx, kubeClient, cmd.Replacements)
	if err != nil {
		return err
	}
	fresh := refreshNodeClaimPrices(ctx, cluster, provider, replacements...)
	if err := baseline.validateReplacements(fresh, cmd.Replacements); err != nil {
		return err
	}
	return baseline.validate(fresh, pools)
}

func hasMutableCapacityType(node *state.StateNode) bool {
	return (node.NodeClaim != nil && node.NodeClaim.Annotations[v1.CapacityTypeTransitionsAnnotationKey] != "") ||
		(node.Node != nil && node.Node.Annotations[v1.CapacityTypeTransitionsAnnotationKey] != "")
}

func (s *nodeClaimPriceSnapshot) relevantScopes(pools sets.Set[string]) sets.Set[string] {
	scopes := sets.New[string]()
	for id, nc := range s.claims {
		if pools.Has(nc.Labels[v1.NodePoolLabelKey]) && s.scopes[id] != "" {
			scopes.Insert(s.scopes[id])
		}
	}
	return scopes
}

func (s *nodeClaimPriceSnapshot) relevantIDs(pools, scopes sets.Set[string]) sets.Set[string] {
	ids := sets.New[string]()
	for id, nc := range s.claims {
		if pools.Has(nc.Labels[v1.NodePoolLabelKey]) || (s.scopes[id] != "" && scopes.Has(s.scopes[id])) {
			ids.Insert(id)
		}
	}
	return ids
}

func (s *nodeClaimPriceSnapshot) sameState(fresh *nodeClaimPriceSnapshot, id string) bool {
	before, okBefore := s.states[id]
	after, okAfter := fresh.states[id]
	return okBefore && okAfter && before.Price == after.Price && before.Scope == after.Scope &&
		maps.Equal(before.Labels, after.Labels) && s.claims[id].UID == fresh.claims[id].UID
}

// Include non-candidate peers and valid zero costs in mutable-pool denominators.
func (s *nodeClaimPriceSnapshot) updateTotals(totals map[string]NodePoolTotals, nodes []*state.StateNode, instanceTypes map[string]map[string]*cloudprovider.InstanceType) {
	mutablePools := sets.New[string]()
	for _, nc := range s.claims {
		mutablePools.Insert(nc.Labels[v1.NodePoolLabelKey])
	}
	for pool := range mutablePools {
		total := totals[pool]
		total.TotalCost = 0
		for _, node := range nodes {
			if node.Labels()[v1.NodePoolLabelKey] != pool {
				continue
			}
			if observation, ok := s.states[node.ProviderID()]; ok {
				total.TotalCost += observation.Price
			} else {
				total.TotalCost += disruptionutils.ResolveOfferingPrice(node.Labels(), instanceTypes[pool][node.Labels()[corev1.LabelInstanceTypeStable]])
			}
		}
		totals[pool] = total
	}
}

func validateCandidatePrices(candidates, validated []*Candidate) error {
	if len(candidates) == 0 || len(validated) == 0 || candidates[0].priceSnapshot == nil || validated[0].priceSnapshot == nil {
		return nil
	}
	pools := sets.New[string]()
	for _, candidate := range candidates {
		pools.Insert(candidate.NodePool.Name)
	}
	return candidates[0].priceSnapshot.validate(validated[0].priceSnapshot, pools)
}

func (c *Command) validatePriceState(ctx context.Context) error {
	if c.validatePrices == nil {
		return nil
	}
	return c.validatePrices(ctx)
}

func mutableNodeClaims(nodes []*state.StateNode, additional []*v1.NodeClaim) map[string]*v1.NodeClaim {
	for _, nc := range additional {
		nodes = append(nodes, &state.StateNode{NodeClaim: nc})
	}
	claims := map[string]*v1.NodeClaim{}
	for _, node := range nodes {
		if node.NodeClaim != nil && node.NodeClaim.Status.ProviderID != "" && hasMutableCapacityType(node) {
			claims[node.ProviderID()] = node.NodeClaim
		}
	}
	return claims
}

func getReplacementClaims(ctx context.Context, kubeClient client.Client, replacements []*Replacement) ([]*v1.NodeClaim, error) {
	result := []*v1.NodeClaim{}
	for _, replacement := range replacements {
		if replacement.Name == "" {
			continue
		}
		nc := &v1.NodeClaim{}
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: replacement.Name}, nc); err != nil {
			return nil, NewValidationError(serrors.Wrap(err, "NodeClaim", klog.KRef("", replacement.Name)))
		}
		result = append(result, nc)
	}
	return result, nil
}

// A replacement can acquire coverage before source deletion. Usually this makes
// it cheaper, but reserved-only price overlays can make its observed cost higher
// than the original On-Demand estimate. Reject those changed economics too.
func (s *nodeClaimPriceSnapshot) validateReplacements(fresh *nodeClaimPriceSnapshot, replacements []*Replacement) error {
	fresh.expectedNewConsumers = sets.New[string]()
	byName := map[string]*Replacement{}
	for _, replacement := range replacements {
		if replacement.Name != "" {
			byName[replacement.Name] = replacement
		}
	}
	for id, nc := range fresh.claims {
		replacement, createdByCommand := byName[nc.Name]
		if !createdByCommand {
			continue
		}
		if _, existed := s.claims[id]; existed {
			continue
		}
		observation, observed := fresh.states[id]
		if !observed || !replacementPriceWithinEstimate(replacement, nc, observation.Price) {
			return NewValidationError(serrors.Wrap(fmt.Errorf("replacement running-node price unavailable or exceeds launch estimate"), "NodeClaim", klog.KObj(nc)))
		}
		// Retain its scope while excluding only this expected new identity.
		// Its existing peers must still be compared across NodePools.
		fresh.expectedNewConsumers.Insert(id)
	}
	return nil
}

func replacementPriceWithinEstimate(replacement *Replacement, nc *v1.NodeClaim, price float64) bool {
	for _, it := range replacement.InstanceTypeOptions {
		if it.Name != nc.Labels[corev1.LabelInstanceTypeStable] {
			continue
		}
		estimate, found := it.OfferingPrice(nc.Labels[corev1.LabelTopologyZone], v1.CapacityTypeOnDemand)
		return found && price <= estimate
	}
	return false
}
