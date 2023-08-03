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
	"fmt"
	"time"

	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
)

const SingleMachineConsolidationTimeoutDuration = 3 * time.Minute

// SingleMachineConsolidation is the consolidation controller that performs single machine consolidation.
type SingleMachineConsolidation struct {
	consolidation
}

func NewSingleMachineConsolidation(clk clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cp cloudprovider.CloudProvider, recorder events.Recorder) *SingleMachineConsolidation {
	return &SingleMachineConsolidation{consolidation: makeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder)}
}

func (s *SingleMachineConsolidation) ShouldDeprovision(ctx context.Context, cn *Candidate) bool {
	return s.consolidation.ShouldDeprovision(ctx, cn) &&
		cn.nodePool.Spec.Deprovisioning.ConsolidationPolicy == v1beta1.ConsolidationPolicyWhenUnderutilized
}

// ComputeCommand generates a deprovisioning command given deprovisionable machines
// nolint:gocyclo
func (s *SingleMachineConsolidation) ComputeCommand(ctx context.Context, candidates ...*Candidate) (Command, error) {
	if s.isConsolidated() {
		return Command{}, nil
	}
	candidates, err := s.sortAndFilterCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("sorting candidates, %w", err)
	}
	deprovisioningEligibleMachinesGauge.WithLabelValues(s.String()).Set(float64(len(candidates)))

	// Set a timeout
	timeout := s.clock.Now().Add(SingleMachineConsolidationTimeoutDuration)
	// binary search to find the maximum number of machines we can terminate
	for i, candidate := range candidates {
		if s.clock.Now().After(timeout) {
			deprovisioningConsolidationTimeoutsCounter.WithLabelValues(singleMachineConsolidationLabelValue).Inc()
			logging.FromContext(ctx).Debugf("abandoning single-machine consolidation due to timeout after evaluating %d candidates", i)
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

		v := NewValidation(consolidationTTL(cmd.candidates), s.clock, s.cluster, s.kubeClient, s.provisioner, s.cloudProvider, s.recorder)
		isValid, err := v.IsValid(ctx, cmd)
		if err != nil {
			logging.FromContext(ctx).Errorf("validating consolidation %s", err)
			continue
		}
		if !isValid {
			return Command{}, fmt.Errorf("command is no longer valid, %s", cmd)
		}
		return cmd, nil
	}
	// couldn't remove any candidate
	s.markConsolidated()
	return Command{}, nil
}
