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

	"github.com/samber/lo"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
)

// EmptyMachineConsolidation is the consolidation controller that performs multi-machine consolidation of entirely empty machines
type EmptyMachineConsolidation struct {
	consolidation
}

func NewEmptyMachineConsolidation(clk clock.Clock, cluster *state.Cluster, kubeClient client.Client,
	provisioner *provisioning.Provisioner, cp cloudprovider.CloudProvider, recorder events.Recorder) *EmptyMachineConsolidation {
	return &EmptyMachineConsolidation{consolidation: makeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder)}
}

// ComputeCommand generates a deprovisioning command given deprovisionable machines
func (c *EmptyMachineConsolidation) ComputeCommand(ctx context.Context, candidates ...*Candidate) (Command, error) {
	if c.cluster.Consolidated() {
		return Command{action: actionDoNothing}, nil
	}
	candidates, err := c.sortAndFilterCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("sorting candidates, %w", err)
	}

	// select the entirely empty nodes
	emptyCandidates := lo.Filter(candidates, func(n *Candidate, _ int) bool { return len(n.pods) == 0 })
	if len(emptyCandidates) == 0 {
		return Command{action: actionDoNothing}, nil
	}

	cmd := Command{
		candidates: emptyCandidates,
		action:     actionDelete,
	}

	// empty node consolidation doesn't use Validation as we get to take advantage of cluster.IsNodeNominated.  This
	// lets us avoid a scheduling simulation (which is performed periodically while pending pods exist and drives
	// cluster.IsNodeNominated already).
	select {
	case <-ctx.Done():
		return Command{}, errors.New("interrupted")
	case <-c.clock.After(consolidationTTL):
	}

	validationCandidates, err := c.getValidationCandidates(ctx, cmd)
	if err != nil {
		return Command{}, fmt.Errorf("getting validation candidates, %w", err)
	}

	// the deletion of empty machines is easy to validate, we just ensure that all the candidatesToDelete are still empty and that
	// the machine isn't a target of a recent scheduling simulation
	for _, n := range mapCandidates(cmd.candidates, validationCandidates) {
		if len(n.pods) != 0 && !c.cluster.IsNodeNominated(n.Name()) {
			return Command{action: actionRetry}, nil
		}
	}

	return cmd, nil
}

func (c *EmptyMachineConsolidation) getValidationCandidates(ctx context.Context, cmd Command) ([]*Candidate, error) {
	// Re-compute Machine Disruption Gates
	gates, err := buildDisruptionGates(ctx, c.kubeClient, c.clock)
	if err != nil {
		return nil, fmt.Errorf("building machine disruption gates mapping, %w", err)
	}
	names, err := getProhibitedMachineNames(ctx, c.kubeClient, gates[consolidationMethod])
	if err != nil {
		return nil, fmt.Errorf("getting prohibited machine names, %w", err)
	}
	validationCandidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.clock, c.cloudProvider, c.ShouldDeprovision, c.recorder, names)
	if err != nil {
		logging.FromContext(ctx).Errorf("computing validation candidates %s", err)
		return nil, err
	}

	return mapCandidates(cmd.candidates, validationCandidates), nil
}
