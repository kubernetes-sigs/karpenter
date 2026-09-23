/*
Copyright The Kubernetes Authors.

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

package provisioning

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// SimulationSession owns immutable scheduler preparation for one controller
// pass. Every NewScheduler call still creates fresh mutable solve state.
type SimulationSession struct {
	provisioner *Provisioner
	nodes       state.StateNodes
	catalog     *schedulerCatalog
	prepared    *scheduling.PreparedSchedulerInputs
}

// NewSimulationSession prepares candidate-independent scheduler inputs from one node snapshot.
func (p *Provisioner) NewSimulationSession(ctx context.Context, nodes state.StateNodes, opts ...scheduling.Options) (*SimulationSession, error) {
	catalog, err := p.newSchedulerCatalog(ctx)
	if err != nil {
		return nil, err
	}
	prepared, err := scheduling.NewPreparedSchedulerInputs(
		ctx,
		catalog.nodePools,
		nodes.Active(),
		catalog.instanceTypes,
		catalog.daemonSetPods,
		p.recorder,
		p.clock,
		opts...,
	)
	if err != nil {
		return nil, err
	}
	return &SimulationSession{
		provisioner: p,
		nodes:       nodes,
		catalog:     catalog,
		prepared:    prepared,
	}, nil
}

// Nodes returns the immutable node snapshot owned by this session.
func (s *SimulationSession) Nodes() state.StateNodes {
	return s.nodes
}

// Stats returns bounded shared-input sizes for observability.
func (s *SimulationSession) Stats() scheduling.PreparedSchedulerStats {
	return s.prepared.Stats()
}

// NewScheduler forks fresh mutable scheduling state from prepared immutable inputs.
func (s *SimulationSession) NewScheduler(
	ctx context.Context,
	pods []*corev1.Pod,
	stateNodes []*state.StateNode,
	deletingPodUIDs sets.Set[types.UID],
	opts ...scheduling.Options,
) (*scheduling.Scheduler, error) {
	opts = append(opts, scheduling.WithPreparedSchedulerInputs(s.prepared))
	return s.provisioner.newScheduler(ctx, pods, stateNodes, deletingPodUIDs, s.catalog, opts...)
}
