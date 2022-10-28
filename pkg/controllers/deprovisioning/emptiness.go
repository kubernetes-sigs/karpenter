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

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/state"
)

// Emptiness is a subreconciler that deletes empty nodes.
// Emptiness will respect TTLSecondsAfterEmpty
type Emptiness struct {
	kubeClient client.Client
	clock      clock.Clock
	cluster    *state.Cluster
}

const emptinessName = "emptiness"

// shouldNotBeDeprovisioned is a predicate used to filter deprovisionable nodes
func (e *Emptiness) shouldNotBeDeprovisioned(ctx context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, nodePods []*v1.Pod) bool {
	if provisioner == nil || provisioner.Spec.TTLSecondsAfterEmpty == nil {
		return true
	}

	// If TTLSecondsAfterEmpty is used, we know the node will have an emptiness timestamp
	if provisioner.Spec.TTLSecondsAfterEmpty != nil {
		emptinessTimestamp, hasEmptinessTimestamp := n.Node.Annotations[v1alpha5.EmptinessTimestampAnnotationKey]
		ttl := time.Duration(ptr.Int64Value(provisioner.Spec.TTLSecondsAfterEmpty)) * time.Second

		emptinessTime, err := time.Parse(time.RFC3339, emptinessTimestamp)
		if err != nil {
			logging.FromContext(ctx).Debugf("Unable to parse emptiness timestamp, %s for node %s", emptinessTimestamp, n.Node.Name)
			return true
		}
		// Don't deprovision if node is not empty, does not have the emptiness timestamp, or is before the emptiness TTL
		return len(nodePods) != 0 || !hasEmptinessTimestamp || !e.clock.Now().After(emptinessTime.Add(ttl))
	}

	// For Consolidation, all we care about is if the node is empty
	return len(nodePods) != 0
}

// sortCandidates orders deprovisionable nodes by the disruptionCost
func (e *Emptiness) sortCandidates(nodes []candidateNode) []candidateNode {
	return nodes
}

// computeCommand generates a deprovisioning command given deprovisionable nodes
func (e *Emptiness) computeCommand(_ context.Context, _ int, nodes ...candidateNode) (deprovisioningCommand, error) {
	emptyNodes := lo.Filter(nodes, func(n candidateNode, _ int) bool { return len(n.pods) == 0 })
	if len(emptyNodes) == 0 {
		return deprovisioningCommand{action: actionDoNothing}, nil
	}
	return deprovisioningCommand{
		nodesToRemove: lo.Map(emptyNodes, func(n candidateNode, _ int) *v1.Node { return n.Node }),
		action:        actionDeleteEmpty,
		created:       e.clock.Now(),
	}, nil
}

// validateCommand validates a command for a deprovisioner
func (e *Emptiness) validateCommand(_ context.Context, candidateNodes []candidateNode, cmd deprovisioningCommand) (bool, error) {
	if cmd.replacementNode != nil {
		return false, fmt.Errorf("expected no replacement node for emptiness")
	}
	// the deletion of empty nodes is easy to validate, we just ensure that all the nodesToDelete are still empty and that
	// the node isn't a target of a recent scheduling simulation
	for _, n := range candidateNodes {
		if len(n.pods) != 0 && !e.cluster.IsNodeNominated(n.Name) {
			return false, nil
		}
	}
	return true, nil
}

// isExecutableCommand checks that a command can be executed by the deprovisioner
func (e *Emptiness) isExecutableCommand(cmd deprovisioningCommand) bool {
	return len(cmd.nodesToRemove) > 0 && cmd.action == actionDeleteEmpty
}

// getTTL returns the time to wait for a deprovisioner's validation
// Don't wait since the action has already been TTL'd with the provisioner's `TTLSecondsAfterEmpty`
func (e *Emptiness) getTTL() time.Duration {
	return 0 * time.Second
}

// string is the string representation of the deprovisioner
func (e *Emptiness) string() string {
	return emptinessName
}
