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

	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/utils/node"
)

// Expiration is a subreconciler that deletes empty nodes.
// Expiration will respect TTLSecondsAfterEmpty
type Expiration struct {
	clock       clock.Clock
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
	recorder    events.Recorder
}

func NewExpiration(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner, recorder events.Recorder) *Expiration {
	return &Expiration{
		clock:       clk,
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: provisioner,
		recorder:    recorder,
	}
}

// ShouldDeprovision is a predicate used to filter deprovisionable nodes
func (e *Expiration) ShouldDeprovision(ctx context.Context, c *Candidate) bool {
	// Filter out nodes without the TTL defined or expired.
	if c.provisioner == nil || c.provisioner.Spec.TTLSecondsUntilExpired == nil {
		return false
	}

	return c.Node.Annotations[v1alpha5.VoluntaryDisruptionAnnotationKey] == v1alpha5.VoluntaryDisruptionExpiredAnnotationValue
}

// SortCandidates orders expired nodes by when they've expired
func (e *Expiration) filterAndSortCandidates(ctx context.Context, nodes []*Candidate) ([]*Candidate, error) {
	candidates, err := filterCandidates(ctx, e.kubeClient, e.recorder, nodes)
	if err != nil {
		return nil, fmt.Errorf("filtering candidates, %w", err)
	}
	sort.Slice(candidates, func(i int, j int) bool {
		return node.GetExpirationTime(candidates[i].Node, candidates[i].provisioner).Before(node.GetExpirationTime(candidates[j].Node, candidates[j].provisioner))
	})
	return candidates, nil
}

// ComputeCommand generates a deprovisioning command given deprovisionable nodes
func (e *Expiration) ComputeCommand(ctx context.Context, nodes ...*Candidate) (Command, error) {
	candidates, err := e.filterAndSortCandidates(ctx, nodes)
	if err != nil {
		return Command{}, fmt.Errorf("filtering candidates, %w", err)
	}

	// Deprovision all empty expired nodes, as they require no scheduling simulations.
	if empty := lo.Filter(candidates, func(c *Candidate, _ int) bool {
		return len(c.pods) == 0
	}); len(empty) > 0 {
		return Command{
			candidates: empty,
			action:     actionDelete,
		}, nil
	}

	for _, candidate := range candidates {
		// Check if we need to create any nodes.
		results, err := simulateScheduling(ctx, e.kubeClient, e.cluster, e.provisioner, candidate)
		if err != nil {
			// if a candidate node is now deleting, just retry
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			return Command{}, err
		}
		// Log when all pods can't schedule, as the command will get executed immediately.
		if !results.AllPodsScheduled() {
			logging.FromContext(ctx).With("node", candidate.Name).Debugf("continuing to expire node after scheduling simulation failed to schedule all pods, %s", results.PodSchedulingErrors())
		}

		logging.FromContext(ctx).With("ttl", time.Duration(ptr.Int64Value(candidates[0].provisioner.Spec.TTLSecondsUntilExpired))*time.Second).
			With("delay", time.Since(node.GetExpirationTime(candidates[0].Node, candidates[0].provisioner))).Infof("triggering termination for expired node after TTL")
		return Command{
			candidates:   []*Candidate{candidate},
			action:       actionReplace,
			replacements: results.NewMachines,
		}, nil
	}
	return Command{action: actionDoNothing}, nil
}

// String is the string representation of the deprovisioner
func (e *Expiration) String() string {
	return metrics.ExpirationReason
}
