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

	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning/orchestration"
	"github.com/samber/lo"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
)

// EmptyNodeConsolidation is the consolidation controller that performs multi-nodeclaim consolidation of entirely empty nodes
type EmptyNodeConsolidation struct {
	consolidation
}

func NewEmptyNodeConsolidation(clk clock.Clock, cluster *state.Cluster, kubeClient client.Client,
	provisioner *provisioning.Provisioner, cp cloudprovider.CloudProvider, recorder events.Recorder,
	queue *orchestration.Queue) *EmptyNodeConsolidation {
	return &EmptyNodeConsolidation{consolidation: makeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder, queue)}
}

// ComputeCommand generates a disruption command given candidates
func (c *EmptyNodeConsolidation) ComputeCommand(ctx context.Context, candidates ...*Candidate) (Command, error) {
	if c.isConsolidated() {
		return Command{}, nil
	}
	candidates, err := c.sortAndFilterCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("sorting candidates, %w", err)
	}
	disruptionEligibleNodesGauge.With(map[string]string{
		methodLabel:            c.Type(),
		consolidationTypeLabel: c.ConsolidationType(),
	}).Set(float64(len(candidates)))

	// select the entirely empty NodeClaims
	emptyCandidates := lo.Filter(candidates, func(n *Candidate, _ int) bool { return len(n.pods) == 0 })
	if len(emptyCandidates) == 0 {
		// none empty, so do nothing
		c.markConsolidated()
		return Command{}, nil
	}

	cmd := Command{
		candidates: emptyCandidates,
	}

	// Empty Node Consolidation doesn't use Validation as we get to take advantage of cluster.IsNodeNominated.  This
	// lets us avoid a scheduling simulation (which is performed periodically while pending pods exist and drives
	// cluster.IsNodeNominated already).
	select {
	case <-ctx.Done():
		return Command{}, errors.New("interrupted")
	case <-c.clock.After(consolidationTTL):
	}
	validationCandidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.recorder, c.clock, c.cloudProvider, c.ShouldDeprovision, c.queue)
	if err != nil {
		logging.FromContext(ctx).Errorf("computing validation candidates %s", err)
		return Command{}, err
	}
	// Get the current representation of the proposed candidates from before the validation timeout
	// We do this so that we can re-validate that the candidates that were computed before we made the decision are the same
	candidatesToDelete := mapCandidates(cmd.candidates, validationCandidates)

	// The deletion of empty NodeClaims is easy to validate, we just ensure that:
	// 1. All the candidatesToDelete are still empty
	// 2. The node isn't a target of a recent scheduling simulation
	for _, n := range candidatesToDelete {
		if len(n.pods) != 0 || c.cluster.IsNodeNominated(n.ProviderID()) {
			logging.FromContext(ctx).Debugf("abandoning empty node consolidation attempt due to pod churn, command is no longer valid, %s", cmd)
			return Command{}, nil
		}
	}
	return cmd, nil
}

func (c *EmptyNodeConsolidation) Type() string {
	return metrics.ConsolidationReason
}

func (c *EmptyNodeConsolidation) ConsolidationType() string {
	return "empty"
}
