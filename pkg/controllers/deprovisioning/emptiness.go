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
	"time"

	"k8s.io/utils/clock"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/metrics"
)

// Emptiness is a subreconciler that deletes empty nodes.
// Emptiness will respect TTLSecondsAfterEmpty
type Emptiness struct {
	clock      clock.Clock
	kubeClient client.Client
	cluster    *state.Cluster
}

func NewEmptiness(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster) *Emptiness {
	return &Emptiness{
		clock:      clk,
		kubeClient: kubeClient,
		cluster:    cluster,
	}
}

// ShouldDeprovision is a predicate used to filter deprovisionable nodes
func (e *Emptiness) ShouldDeprovision(ctx context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, nodePods []*v1.Pod) bool {
	if provisioner == nil || provisioner.Spec.TTLSecondsAfterEmpty == nil || len(nodePods) != 0 {
		return false
	}

	emptinessTimestamp, hasEmptinessTimestamp := n.Node.Annotations[v1alpha5.EmptinessTimestampAnnotationKey]
	if !hasEmptinessTimestamp {
		return false
	}
	ttl := time.Duration(ptr.Int64Value(provisioner.Spec.TTLSecondsAfterEmpty)) * time.Second

	emptinessTime, err := time.Parse(time.RFC3339, emptinessTimestamp)
	if err != nil {
		logging.FromContext(ctx).With("emptiness-timestamp", emptinessTimestamp).Debugf("unable to parse emptiness timestamp")
		return true
	}
	// Don't deprovision if node's emptiness timestamp is before the emptiness TTL
	return e.clock.Now().After(emptinessTime.Add(ttl))
}

// ComputeCommand generates a deprovisioning command given deprovisionable nodes
func (e *Emptiness) ComputeCommand(_ context.Context, nodes ...CandidateNode) (Command, error) {
	emptyNodes := lo.Filter(nodes, func(n CandidateNode, _ int) bool { return len(n.pods) == 0 })
	if len(emptyNodes) == 0 {
		return Command{action: actionDoNothing}, nil
	}
	return Command{
		nodesToRemove: lo.Map(emptyNodes, func(n CandidateNode, _ int) *v1.Node { return n.Node }),
		action:        actionDelete,
	}, nil
}

// string is the string representation of the deprovisioner
func (e *Emptiness) String() string {
	return metrics.EmptinessReason
}
