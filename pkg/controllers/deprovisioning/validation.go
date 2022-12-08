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
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
)

// Validation is used to perform validation on a consolidation command.  It makes an assumption that when re-used, all
// of the commands passed to IsValid were constructed based off of the same consolidation state.  This allows it to
// skip the validation TTL for all but the first command.
type Validation struct {
	validationPeriod time.Duration
	start            time.Time
	clock            clock.Clock
	cluster          *state.Cluster
	kubeClient       client.Client
	cloudProvider    cloudprovider.CloudProvider
	provisioner      *provisioning.Provisioner
	once             sync.Once
	// validationCandidates are the cached validation candidates.  We capture these when validating the first command and reuse them for
	// validating subsequent commands.
	validationCandidates []CandidateNode
}

func NewValidation(validationPeriod time.Duration, clk clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner, cp cloudprovider.CloudProvider) *Validation {
	return &Validation{
		validationPeriod: validationPeriod,
		clock:            clk,
		cluster:          cluster,
		kubeClient:       kubeClient,
		provisioner:      provisioner,
		cloudProvider:    cp,
	}
}

func (v *Validation) IsValid(ctx context.Context, cmd Command) (bool, error) {
	var err error
	v.once.Do(func() {
		v.start = v.clock.Now()
	})

	waitDuration := v.validationPeriod - v.clock.Since(v.start)
	if waitDuration > 0 {
		select {
		case <-ctx.Done():
			return false, errors.New("context canceled")
		case <-v.clock.After(waitDuration):
		}
	}

	if len(v.validationCandidates) == 0 {
		v.validationCandidates, err = candidateNodes(ctx, v.cluster, v.kubeClient, v.clock, v.cloudProvider, v.ShouldDeprovision)
		if err != nil {
			return false, fmt.Errorf("constructing validation candidates, %w", err)
		}
	}

	// a node we are about to delete is a target of a currently pending pod, wait for that to settle
	// before continuing consolidation
	for _, n := range cmd.nodesToRemove {
		if v.cluster.IsNodeNominated(n.Name) {
			return false, nil
		}
	}

	isValid, err := v.ValidateCommand(ctx, cmd, v.validationCandidates)
	if err != nil {
		return false, fmt.Errorf("validating command, %w", err)
	}

	return isValid, nil
}

// ShouldDeprovision is a predicate used to filter deprovisionable nodes
func (v *Validation) ShouldDeprovision(_ context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, _ []*v1.Pod) bool {
	if val, ok := n.Node.Annotations[v1alpha5.DoNotConsolidateNodeAnnotationKey]; ok {
		return val != "true"
	}
	return provisioner != nil && provisioner.Spec.Consolidation != nil && ptr.BoolValue(provisioner.Spec.Consolidation.Enabled)
}

// ValidateCommand validates a command for a deprovisioner
func (v *Validation) ValidateCommand(ctx context.Context, cmd Command, candidateNodes []CandidateNode) (bool, error) {
	// map from nodes we are about to remove back into candidate nodes with cluster state
	nodesToDelete := mapNodes(cmd.nodesToRemove, candidateNodes)
	// None of the chosen candidate nodes are valid for execution, so retry
	if len(nodesToDelete) == 0 {
		return false, nil
	}

	newNodes, allPodsScheduled, err := simulateScheduling(ctx, v.kubeClient, v.cluster, v.provisioner, nodesToDelete...)
	if err != nil {
		return false, fmt.Errorf("simluating scheduling, %w", err)
	}
	if !allPodsScheduled {
		return false, nil
	}

	// We want to ensure that the re-simulated scheduling using the current cluster state produces the same result.
	// There are three possible options for the number of new nodesToDelete that we need to handle:
	// len(newNodes) == 0, as long as we weren't expecting a new node, this is valid
	// len(newNodes) > 1, something in the cluster changed so that the nodesToDelete we were going to delete can no longer
	//                    be deleted without producing more than one node
	// len(newNodes) == 1, as long as the node looks like what we were expecting, this is valid
	if len(newNodes) == 0 {
		if len(cmd.replacementMachines) == 0 {
			// scheduling produced zero new nodes and we weren't expecting any, so this is valid.
			return true, nil
		}
		// if it produced no new nodes, but we were expecting one we should re-simulate as there is likely a better
		// consolidation option now
		return false, nil
	}

	// we need more than one replacement node which is never valid currently (all of our node replacement is m->1, never m->n)
	if len(newNodes) > 1 {
		return false, nil
	}

	// we now know that scheduling simulation wants to create one new node
	if len(cmd.replacementMachines) == 0 {
		// but we weren't expecting any new nodes, so this is invalid
		return false, nil
	}

	// We know that the scheduling simulation wants to create a new node and that the command we are verifying wants
	// to create a new node. The scheduling simulation doesn't apply any filtering to instance types, so it may include
	// instance types that we don't want to launch which were filtered out when the lifecycleCommand was created.  To
	// check if our lifecycleCommand is valid, we just want to ensure that the list of instance types we are considering
	// creating are a subset of what scheduling says we should create.  We check for a subset since the scheduling
	// simulation here does no price filtering, so it will include more expensive types.
	//
	// This is necessary since consolidation only wants cheaper nodes.  Suppose consolidation determined we should delete
	// a 4xlarge and replace it with a 2xlarge. If things have changed and the scheduling simulation we just performed
	// now says that we need to launch a 4xlarge. It's still launching the correct number of nodes, but it's just
	// as expensive or possibly more so we shouldn't validate.
	if !instanceTypesAreSubset(cmd.replacementMachines[0].InstanceTypeOptions, newNodes[0].InstanceTypeOptions) {
		return false, nil
	}

	// Now we know:
	// - current scheduling simulation says to create a new node with types T = {T_0, T_1, ..., T_n}
	// - our lifecycle command says to create a node with types {U_0, U_1, ..., U_n} where U is a subset of T
	return true, nil
}
