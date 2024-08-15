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

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
)

// CloudProvider is a subreconciler that deletes candidates according to cloud provider specific reasons.
// This should be methods that are
type CloudProvider struct {
	cloudprovider.CloudProvider
	kubeClient  client.Client
	recorder    events.Recorder
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
}

func NewCloudProvider(kubeClient client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, recorder events.Recorder, provisioner *provisioning.Provisioner) *CloudProvider {
	return &CloudProvider{
		CloudProvider: cloudProvider,
		kubeClient:    kubeClient,
		cluster:       cluster,
		provisioner:   provisioner,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (cp *CloudProvider) ShouldDisrupt(ctx context.Context, c *Candidate) bool {
	for _, reason := range cp.DisruptionReasons() {
		if c.NodeClaim.StatusConditions().Get(string(reason)).IsTrue() {
			return true
		}
	}
	return false
}

// ComputeCommand generates a disruption command given candidates
func (cp *CloudProvider) ComputeCommand(ctx context.Context, disruptionBudgetMapping map[string]map[v1.DisruptionReason]int, candidates ...*Candidate) (Command, scheduling.Results, error) {
	// Do a quick check through the candidates to see if they're empty.
	// For each candidate that is empty with a nodePool allowing its disruption
	// add it to the existing command.
	empty := make([]*Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if len(candidate.reschedulablePods) > 0 {
			continue
		}
		// If there's disruptions allowed for the candidate's nodepool,
		// add it to the list of candidates, and decrement the budget.
		if disruptionBudgetMapping[candidate.nodePool.Name][cp.Reason()] > 0 {
			empty = append(empty, candidate)
			disruptionBudgetMapping[candidate.nodePool.Name][cp.Reason()]--
		}
	}
	// Disrupt all empty CloudProvidered candidates, as they require no scheduling simulations.
	if len(empty) > 0 {
		return Command{
			candidates: empty,
		}, scheduling.Results{}, nil
	}

	for _, candidate := range candidates {
		// If the disruption budget doesn't allow this candidate to be disrupted,
		// continue to the next candidate. We don't need to decrement any budget
		// counter since CloudProvider commands can only have one candidate.
		if disruptionBudgetMapping[candidate.nodePool.Name][cp.Reason()] == 0 {
			continue
		}
		// Check if we need to create any NodeClaims.
		results, err := SimulateScheduling(ctx, cp.kubeClient, cp.cluster, cp.provisioner, candidate)
		if err != nil {
			// if a candidate is now deleting, just retry
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			return Command{}, scheduling.Results{}, err
		}
		// Emit an event that we couldn't reschedule the pods on the node.
		if !results.AllNonPendingPodsScheduled() {
			cp.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, results.NonPendingPodSchedulingErrors())...)
			continue
		}

		return Command{
			candidates:   []*Candidate{candidate},
			replacements: results.NewNodeClaims,
		}, results, nil
	}
	return Command{}, scheduling.Results{}, nil
}

func (cp *CloudProvider) Reason() v1.DisruptionReason {
	return "CloudProviderReason"
}

func (cp *CloudProvider) Class() string {
	return EventualDisruptionClass
}

func (cp *CloudProvider) ConsolidationType() string {
	return ""
}
