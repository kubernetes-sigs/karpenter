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
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
)

// EmptyNodeConsolidation is the consolidation controller that performs multi-node consolidation of entirely empty nodes
type EmptyNodeConsolidation struct {
	consolidation
}

func NewEmptyNodeConsolidation(clk clock.Clock, cluster *state.Cluster, kubeClient client.Client,
	provisioner *provisioning.Provisioner, cp cloudprovider.CloudProvider, reporter *Reporter) *EmptyNodeConsolidation {
	return &EmptyNodeConsolidation{consolidation: makeConsolidation(clk, cluster, kubeClient, provisioner, cp, reporter)}
}

// ComputeCommand generates a deprovisioning command given deprovisionable nodes
func (c *EmptyNodeConsolidation) ComputeCommand(ctx context.Context, candidates ...CandidateNode) (Command, error) {
	if !c.ShouldAttemptConsolidation() {
		return Command{action: actionDoNothing}, nil
	}
	candidates, err := c.sortAndFilterCandidates(ctx, candidates)
	if err != nil {
		return Command{}, fmt.Errorf("sorting candidates, %w", err)
	}

	// select the entirely empty nodes
	emptyNodes := lo.Filter(candidates, func(n CandidateNode, _ int) bool { return len(n.pods) == 0 })
	if len(emptyNodes) == 0 {
		return Command{action: actionDoNothing}, nil
	}

	cmd := Command{
		nodesToRemove: lo.Map(emptyNodes, func(n CandidateNode, _ int) *v1.Node { return n.Node }),
		action:        actionDelete,
	}

	// empty node consolidation doesn't use Validation as we get to take advantage of cluster.IsNodeNominated.  This
	// lets us avoid a scheduling simulation (which is performed periodically while pending pods exist and drives
	// cluster.IsNodeNominated already).
	select {
	case <-ctx.Done():
		return Command{}, errors.New("interrupted")
	case <-c.clock.After(consolidationTTL):
	}
	validationCandidates, err := candidateNodes(ctx, c.cluster, c.kubeClient, c.clock, c.cloudProvider, c.ShouldDeprovision)
	if err != nil {
		logging.FromContext(ctx).Errorf("computing validation candidates %s", err)
		return Command{}, err
	}
	nodesToDelete := mapNodes(cmd.nodesToRemove, validationCandidates)

	// the deletion of empty nodes is easy to validate, we just ensure that all the nodesToDelete are still empty and that
	// the node isn't a target of a recent scheduling simulation
	for _, n := range nodesToDelete {
		if len(n.pods) != 0 && !c.cluster.IsNodeNominated(n.Name) {
			return Command{action: actionRetry}, nil
		}
	}

	return cmd, nil
}
