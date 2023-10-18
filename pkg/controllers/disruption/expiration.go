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

package disruption

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"k8s.io/utils/clock"

	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	disruptionevents "github.com/aws/karpenter-core/pkg/controllers/disruption/events"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	recorder "github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
)

// Expiration is a subreconciler that deletes empty candidates.
// Expiration will respect TTLSecondsAfterEmpty
type Expiration struct {
	clock       clock.Clock
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
}

func NewExpiration(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner) *Expiration {
	return &Expiration{
		clock:       clk,
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: provisioner,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (e *Expiration) ShouldDisrupt(_ context.Context, c *Candidate) bool {
	return c.nodePool.Spec.Disruption.ExpireAfter.Duration != nil &&
		c.NodeClaim.StatusConditions().GetCondition(v1beta1.Expired).IsTrue()
}

// SortCandidates orders expired candidates by when they've expired
func (e *Expiration) filterAndSortCandidates(ctx context.Context, candidates []*Candidate) ([]*Candidate, error) {
	candidates, err := filterCandidates(ctx, e.kubeClient, candidates)
	if err != nil {
		return nil, fmt.Errorf("filtering candidates, %w", err)
	}
	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].NodeClaim.StatusConditions().GetCondition(v1beta1.Expired).LastTransitionTime.Inner.Time.Before(
			candidates[j].NodeClaim.StatusConditions().GetCondition(v1beta1.Expired).LastTransitionTime.Inner.Time)
	})
	return candidates, nil
}

// ComputeCommand generates a disrpution command given candidates
func (e *Expiration) ComputeCommand(ctx context.Context, candidates ...*Candidate) (Command, error) {
	candidates, err := e.filterAndSortCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("filtering candidates, %w", err)
	}
	deprovisioningEligibleMachinesGauge.WithLabelValues(e.Type()).Set(float64(len(candidates)))
	disruptionEligibleNodesGauge.With(map[string]string{
		methodLabel:            e.Type(),
		consolidationTypeLabel: e.ConsolidationType(),
	}).Set(float64(len(candidates)))

	// Disrupt all empty expired candidates, as they require no scheduling simulations.
	if empty := lo.Filter(candidates, func(c *Candidate, _ int) bool {
		return len(c.pods) == 0
	}); len(empty) > 0 {
		return Command{
			candidates: empty,
		}, nil
	}

	for _, candidate := range candidates {
		// Check if we need to create any NodeClaims.
		results, err := simulateScheduling(ctx, e.kubeClient, e.cluster, e.provisioner, candidate)
		if err != nil {
			// if a candidate node is now deleting, just retry
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			return Command{}, err
		}
		// Log when all pods can't schedule, as the command will get executed immediately.
		if !results.AllNonPendingPodsScheduled() {
			logging.FromContext(ctx).With(lo.Ternary(candidate.NodeClaim.IsMachine, "machine", "nodeclaim"), candidate.NodeClaim.Name, "node", candidate.Node.Name).Debugf("cannot terminate since scheduling simulation failed to schedule all pods, %s", results.NonPendingPodSchedulingErrors())
			recorder.FromContext(ctx).Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim, "Scheduling simulation failed to schedule all pods")...)
			continue
		}

		logging.FromContext(ctx).With("ttl", candidates[0].nodePool.Spec.Disruption.ExpireAfter.String()).Infof("triggering termination for expired node after TTL")
		return Command{
			candidates:   []*Candidate{candidate},
			replacements: results.NewNodeClaims,
		}, nil
	}
	return Command{}, nil
}

func (e *Expiration) Type() string {
	return metrics.ExpirationReason
}

func (e *Expiration) ConsolidationType() string {
	return ""
}
