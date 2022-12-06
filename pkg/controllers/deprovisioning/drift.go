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

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/metrics"
)

// Drift is a subreconciler that deletes empty nodes.
// Drift will respect TTLSecondsAfterEmpty
type Drift struct {
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
}

func NewDrift(kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner) *Drift {
	return &Drift{
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: provisioner,
	}
}

// ShouldDeprovision is a predicate used to filter deprovisionable nodes
func (d *Drift) ShouldDeprovision(ctx context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, nodePods []*v1.Pod) bool {
	// Look up the feature flag to see if we should deprovision the node because of drift.
	if !settings.FromContext(ctx).DriftEnabled {
		return false
	}
	return n.Node.Annotations[v1alpha5.VoluntaryDisruptionAnnotationKey] == v1alpha5.VoluntaryDisruptionDriftedAnnotationValue
}

// ComputeCommand generates a deprovisioning command given deprovisionable nodes
func (d *Drift) ComputeCommand(ctx context.Context, candidates ...CandidateNode) (Command, error) {
	pdbs, err := NewPDBLimits(ctx, d.kubeClient)
	if err != nil {
		return Command{}, fmt.Errorf("tracking PodDisruptionBudgets, %w", err)
	}
	for _, candidate := range candidates {
		// is this a node that we can terminate?  This check is meant to be fast so we can save the expense of simulated
		// scheduling unless its really needed
		if _, canTerminate := canBeTerminated(candidate, pdbs); !canTerminate {
			continue
		}

		// Check if we need to create any nodes.
		newNodes, allPodsScheduled, err := simulateScheduling(ctx, d.kubeClient, d.cluster, d.provisioner, candidate)
		if err != nil {
			// if a candidate node is now deleting, just retry
			if errors.Is(err, errCandidateNodeDeleting) {
				continue
			}
			return Command{}, err
		}
		// Log when all pods can't schedule, as the command will get executed immediately.
		if !allPodsScheduled {
			logging.FromContext(ctx).With("node", candidate.Name).Debug("Continuing to terminate drifted node after scheduling simulation failed to schedule all pods")
		}
		// were we able to schedule all the pods on the inflight nodes?
		if len(newNodes) == 0 {
			return Command{
				nodesToRemove: []*v1.Node{candidate.Node},
				action:        actionDelete,
			}, nil
		}
		return Command{
			nodesToRemove:       []*v1.Node{candidate.Node},
			action:              actionReplace,
			replacementMachines: newNodes,
		}, nil
	}
	return Command{action: actionDoNothing}, nil
}

// String is the string representation of the deprovisioner
func (d *Drift) String() string {
	return metrics.DriftReason
}
