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
	"fmt"
	"time"

	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
)

const SingleNodeConsolidationTimeoutDuration = 3 * time.Minute

// SingleNodeConsolidation is the consolidation controller that performs single-node consolidation.
type SingleNodeConsolidation struct {
	consolidation
}

func NewSingleNodeConsolidation(clk clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cp cloudprovider.CloudProvider, recorder events.Recorder) *SingleNodeConsolidation {
	return &SingleNodeConsolidation{consolidation: makeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder)}
}

// ComputeCommand generates a disruption command given candidates
// nolint:gocyclo
func (s *SingleNodeConsolidation) ComputeCommand(ctx context.Context, candidates ...*Candidate) (Command, error) {
	if s.isConsolidated() {
		return Command{}, nil
	}
	candidates, err := s.sortAndFilterCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("sorting candidates, %w", err)
	}
	deprovisioningEligibleMachinesGauge.WithLabelValues(s.Type()).Set(float64(len(candidates)))
	disruptionEligibleNodesGauge.With(map[string]string{
		methodLabel:            s.Type(),
		consolidationTypeLabel: s.ConsolidationType(),
	}).Set(float64(len(candidates)))

	v := NewValidation(consolidationTTL, s.clock, s.cluster, s.kubeClient, s.provisioner, s.cloudProvider, s.recorder)

	// Set a timeout
	timeout := s.clock.Now().Add(SingleNodeConsolidationTimeoutDuration)
	// binary search to find the maximum number of NodeClaims we can terminate
	for i, candidate := range candidates {
		if s.clock.Now().After(timeout) {
			deprovisioningConsolidationTimeoutsCounter.WithLabelValues(singleMachineConsolidationLabelValue).Inc()
			disruptionConsolidationTimeoutTotalCounter.WithLabelValues(s.ConsolidationType()).Inc()
			logging.FromContext(ctx).Debugf("abandoning single-node consolidation due to timeout after evaluating %d candidates", i)
			return Command{}, nil
		}
		// compute a possible consolidation option
		cmd, err := s.computeConsolidation(ctx, candidate)
		if err != nil {
			logging.FromContext(ctx).Errorf("computing consolidation %s", err)
			continue
		}
		if cmd.Action() == NoOpAction {
			continue
		}
		isValid, err := v.IsValid(ctx, cmd)
		if err != nil {
			return Command{}, fmt.Errorf("validating consolidation, %w", err)
		}
		if !isValid {
			logging.FromContext(ctx).Debugf("abandoning single-node consolidation attempt due to pod churn, command is no longer valid, %s", cmd)
			return Command{}, nil
		}
		return cmd, nil
	}
	// couldn't remove any candidate
	s.markConsolidated()
	return Command{}, nil
}

func (s *SingleNodeConsolidation) Type() string {
	return metrics.ConsolidationReason
}

func (s *SingleNodeConsolidation) ConsolidationType() string {
	return "single"
}
