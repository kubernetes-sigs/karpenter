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

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	pscheduling "github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
)

// Emptiness is a subreconciler that deletes nodes that are empty after a ttl
type Emptiness struct {
	kubeClient client.Client
	clock      clock.Clock
	cluster    *state.Cluster
}

const emptinessName = "emptiness"

func (e *Emptiness) shouldNotBeDeprovisioned(ctx context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, pods []*v1.Pod) bool {
	if provisioner == nil || provisioner.Spec.TTLSecondsAfterEmpty == nil {
		return true
	}

	emptinessTimestamp, hasEmptinessTimestamp := n.Node.Annotations[v1alpha5.EmptinessTimestampAnnotationKey]
	ttl := time.Duration(ptr.Int64Value(provisioner.Spec.TTLSecondsAfterEmpty)) * time.Second

	emptinessTime, err := time.Parse(time.RFC3339, emptinessTimestamp)
	if err != nil {
		logging.FromContext(ctx).Debugf("Unable to parse emptiness timestamp, %s for node %s", emptinessTimestamp, n.Node.Name)
		return true
	}
	// node is not empty, does not have the emptiness timestamp, or is before the emptiness TTL
	return len(pods) != 0 || !hasEmptinessTimestamp || !e.clock.Now().After(emptinessTime.Add(ttl))
}

// Any empty node is equally prioritized since disruption cost is the same
func (e *Emptiness) sortCandidates(nodes []candidateNode) []candidateNode {
	return nodes
}

// computeCommand will always return deprovisioningActionDeleteEmpty as it is the only possible command for emptiness
func (e *Emptiness) computeCommand(_ context.Context, nodes ...candidateNode) (DeprovisioningCommand, error) {
	emptyNodes := lo.Filter(nodes, func(n candidateNode, _ int) bool { return len(n.pods) == 0})
	if len(emptyNodes) == 0 {
		return DeprovisioningCommand{action: deprovisioningActionDoNothing}, nil
	}
	return DeprovisioningCommand{
		nodesToRemove: lo.Map(emptyNodes, func(n candidateNode, _ int) *v1.Node { return n.Node }),
		action:        deprovisioningActionDeleteEmpty,
		created:       e.clock.Now(),
	}, nil
}

func (e *Emptiness) isExecutableCommand(cmd DeprovisioningCommand) bool {
	return len(cmd.nodesToRemove) > 0 && cmd.action == deprovisioningActionDeleteEmpty
}

func (e *Emptiness) validateCommand(_ context.Context, candidateNodes []candidateNode, replacementNode *pscheduling.Node) (bool, error) {
	if replacementNode != nil {
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

// mapNodes maps from a list of *v1.Node to candidateNode
func (e *Emptiness) mapNodes(nodes []*v1.Node, candidateNodes []candidateNode) ([]candidateNode, error) {
	verifyNodeNames := sets.NewString(lo.Map(nodes, func(t *v1.Node, i int) string { return t.Name })...)
	var ret []candidateNode
	for _, c := range candidateNodes {
		if verifyNodeNames.Has(c.Name) {
			ret = append(ret, c)
		}
	}
	return ret, nil
}

func (e *Emptiness) string() string {
	return emptinessName
}

// Don't wait since the action has already been TTL'd with the provisioner's `TTLSecondsAfterEmpty`
func (e *Emptiness) getTTL() time.Duration {
	return 0 * time.Second
}

func (e *Emptiness) deprovisionIncrementally() bool {
	return false
}
