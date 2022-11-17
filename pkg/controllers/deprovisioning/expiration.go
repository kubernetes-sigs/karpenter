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

	"k8s.io/utils/clock"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/metrics"
)

// Expiration is a subreconciler that deletes empty nodes.
// Expiration will respect TTLSecondsAfterEmpty
type Expiration struct {
	kubeClient  client.Client
	clock       clock.Clock
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
}

// ShouldDeprovision is a predicate used to filter deprovisionable nodes
func (e *Expiration) ShouldDeprovision(ctx context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, nodePods []*v1.Pod) bool {
	return e.clock.Now().After(getExpirationTime(n.Node, provisioner))
}

// SortCandidates orders expired nodes by when they've expired
func (e *Expiration) SortCandidates(nodes []CandidateNode) []CandidateNode {
	sort.Slice(nodes, func(i int, j int) bool {
		return getExpirationTime(nodes[i].Node, nodes[i].provisioner).Before(getExpirationTime(nodes[j].Node, nodes[j].provisioner))
	})
	return nodes
}

// ComputeCommand generates a deprovisioning command given deprovisionable nodes
func (e *Expiration) ComputeCommand(ctx context.Context, attempt int, candidates ...CandidateNode) (Command, error) {
	pdbs, err := NewPDBLimits(ctx, e.kubeClient)
	if err != nil {
		return Command{action: actionFailed}, fmt.Errorf("tracking PodDisruptionBudgets, %w", err)
	}
	// is this a node that we can terminate?  This check is meant to be fast so we can save the expense of simulated
	// scheduling unless its really needed
	if !canBeTerminated(candidates[attempt], pdbs) {
		return Command{action: actionNotPossible}, nil
	}

	// Only expire one node at a time.
	node := candidates[attempt]
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("node", node.Name))
	// Check if we need to create any nodes.
	newNodes, allPodsScheduled, err := simulateScheduling(ctx, e.kubeClient, e.cluster, e.provisioner, node)
	if err != nil {
		// if a candidate node is now deleting, just retry
		if errors.Is(err, errCandidateNodeDeleting) {
			return Command{action: actionDoNothing}, nil
		}
		return Command{}, err
	}
	// Log when all pods can't schedule, as the command will get executed immediately.
	if !allPodsScheduled {
		logging.FromContext(ctx).Infof("continuing to expire node after scheduling simulation failed to schedule all pods")
	}

	// were we able to schedule all the pods on the inflight nodes?
	if len(newNodes) == 0 {
		return Command{
			nodesToRemove: []*v1.Node{node.Node},
			action:        actionDelete,
			createdAt:     e.clock.Now(),
		}, nil
	}

	return Command{
		nodesToRemove:    []*v1.Node{node.Node},
		action:           actionReplace,
		replacementNodes: newNodes,
		createdAt:        e.clock.Now(),
	}, nil
}

// ValidateCommand validates a command for a deprovisioner
// We don't need to do another scheduling simulation since TTL is 0 seconds.
// TODO @njtran remove from interface and use only for Consolidation
func (e *Expiration) ValidateCommand(ctx context.Context, candidates []CandidateNode, cmd Command) (bool, error) {
	// Once validation passes, log the deprovisioning result.
	logging.FromContext(ctx).Infof("triggering termination for expired node after %s (+%s)",
		time.Duration(ptr.Int64Value(candidates[0].provisioner.Spec.TTLSecondsUntilExpired))*time.Second, time.Since(getExpirationTime(candidates[0].Node, candidates[0].provisioner)))
	return true, nil
}

// TTL returns the time to wait for a deprovisioner's validation
// Don't wait since the action has already been TTL'd with the provisioner's `TTLSecondsUntilExpired`
func (e *Expiration) TTL() time.Duration {
	return 0 * time.Second
}

// String is the string representation of the deprovisioner
func (e *Expiration) String() string {
	return metrics.ExpirationReason
}

func getExpirationTime(node *v1.Node, provisioner *v1alpha5.Provisioner) time.Time {
	if provisioner == nil || provisioner.Spec.TTLSecondsUntilExpired == nil {
		// If not defined, return some much larger time.
		return time.Date(5000, 0, 0, 0, 0, 0, 0, time.UTC)
	}
	expirationTTL := time.Duration(ptr.Int64Value(provisioner.Spec.TTLSecondsUntilExpired)) * time.Second
	return node.CreationTimestamp.Add(expirationTTL)
}
