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
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	deprovisioningevents "github.com/aws/karpenter-core/pkg/controllers/deprovisioning/events"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/utils/pod"
)

// Drift is a subreconciler that deletes empty nodes.
// Drift will respect TTLSecondsAfterEmpty
type Drift struct {
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
	recorder    events.Recorder
}

func NewDrift(kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner, recorder events.Recorder) *Drift {
	return &Drift{
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: provisioner,
		recorder:    recorder,
	}
}

// ShouldDeprovision is a predicate used to filter deprovisionable nodes
func (d *Drift) ShouldDeprovision(ctx context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, nodePods []*v1.Pod) bool {
	// Look up the feature flag to see if we should deprovision the node because of drift.
	if !settings.FromContext(ctx).DriftEnabled {
		return false
	}
	return n.Annotations()[v1alpha5.VoluntaryDisruptionAnnotationKey] == v1alpha5.VoluntaryDisruptionDriftedAnnotationValue
}

// ComputeCommand generates a deprovisioning command given deprovisionable nodes
func (d *Drift) ComputeCommand(ctx context.Context, candidates ...CandidateNode) (Command, error) {
	pdbs, err := NewPDBLimits(ctx, d.kubeClient)
	if err != nil {
		return Command{}, fmt.Errorf("tracking PodDisruptionBudgets, %w", err)
	}
	// filter out nodes that can't be terminated
	candidates = lo.Filter(candidates, func(cn CandidateNode, _ int) bool {
		if !cn.DeletionTimestamp.IsZero() {
			return false
		}
		if pdbs != nil {
			if pdb, ok := pdbs.CanEvictPods(cn.pods); !ok {
				d.recorder.Publish(deprovisioningevents.BlockedDeprovisioning(cn.Node, fmt.Sprintf("pdb %s prevents pod evictions", pdb)))
				return false
			}
		}
		if p, ok := lo.Find(cn.pods, func(p *v1.Pod) bool {
			if pod.IsTerminating(p) || pod.IsTerminal(p) || pod.IsOwnedByNode(p) {
				return false
			}
			return pod.HasDoNotEvict(p)
		}); ok {
			d.recorder.Publish(deprovisioningevents.BlockedDeprovisioning(cn.Node,
				fmt.Sprintf("pod %s/%s has do not evict annotation", p.Namespace, p.Name)))
			return false
		}
		return true
	})

	for _, candidate := range candidates {
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
			nodesToRemove:    []*v1.Node{candidate.Node},
			action:           actionReplace,
			replacementNodes: newNodes,
		}, nil
	}
	return Command{action: actionDoNothing}, nil
}

// String is the string representation of the deprovisioner
func (d *Drift) String() string {
	return metrics.DriftReason
}
