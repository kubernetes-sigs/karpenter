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

package disruption

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/awslabs/operatorpkg/option"
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
)

// Emptiness is a subreconciler that deletes empty candidates.
type Emptiness struct {
	consolidation
	validator Validator
}

func NewEmptiness(c consolidation, opts ...option.Function[MethodOptions]) *Emptiness {
	o := option.Resolve(append([]option.Function[MethodOptions]{WithValidator(NewEmptinessValidator(c))}, opts...)...)
	return &Emptiness{consolidation: c, validator: o.validator}
}

// ShouldDisrupt is a predicate used to filter candidates
func (e *Emptiness) ShouldDisrupt(_ context.Context, c *Candidate) bool {
	if c.OwnedByStaticNodePool() {
		return false
	}
	// If consolidation is disabled, don't do anything. This emptiness should run for both WhenEmpty and WhenEmptyOrUnderutilized
	if c.NodePool.Spec.Disruption.ConsolidateAfter.Duration == nil {
		e.recorder.Publish(disruptionevents.Unconsolidatable(c.Node, c.NodeClaim, fmt.Sprintf("NodePool %q has consolidation disabled", c.NodePool.Name))...)
		return false
	}
	return c.IsEmpty() && c.NodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable).IsTrue()
}

// ComputeCommand generates a disruption command given candidates
//
//nolint:gocyclo
func (e *Emptiness) ComputeCommands(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	if e.IsConsolidated() {
		return []Command{}, nil
	}
	candidates = e.sortCandidates(ctx, candidates)
	// Nodes without buffer pods go first, so the buffer is checked against the nodes that remain
	withoutBuffer, withBuffer := lo.FilterReject(candidates, func(c *Candidate, _ int) bool { return !e.cluster.HasBufferPods(c.ProviderID()) })
	candidates = append(withoutBuffer, withBuffer...)

	empty := make([]*Candidate, 0, len(candidates))
	constrainedByBudgets := false
	for _, candidate := range candidates {
		if !candidate.IsEmpty() {
			continue
		}
		if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
			// set constrainedByBudgets to true if any node was a candidate but was constrained by a budget
			constrainedByBudgets = true
			continue
		}
		if e.cluster.HasBufferPods(candidate.ProviderID()) {
			fits, err := e.bufferFitsWithout(ctx, append(slices.Clone(empty), candidate)...)
			if errors.Is(err, errCandidateDeleting) {
				continue
			}
			if err != nil {
				return []Command{}, err
			}
			if !fits {
				e.recorder.Publish(disruptionevents.Unconsolidatable(candidate.Node, candidate.NodeClaim, fmt.Sprintf("Node %q has buffer pods", candidate.Node.Name))...)
				continue
			}
		}
		// If there's disruptions allowed for the candidate's nodepool,
		// add it to the list of candidates, and decrement the budget.
		empty = append(empty, candidate)
		disruptionBudgetMapping[candidate.NodePool.Name]--
	}
	// none empty, so do nothing
	if len(empty) == 0 {
		// if there are no candidates, but a nodepool had a fully blocking budget,
		// don't mark the cluster as consolidated, as it's possible this nodepool
		// should be consolidated the next time we try to disrupt.
		if !constrainedByBudgets {
			e.markConsolidated()
		}
		return []Command{}, nil
	}

	cmd := Command{
		Candidates:          empty,
		PoolDisruptionCosts: computePoolDisruptionCosts(empty),
	}
	validCmd, err := e.validator.Validate(ctx, cmd, commandValidationDelay)
	if err != nil {
		if IsValidationError(err) {
			log.FromContext(ctx).V(1).WithValues(cmd.LogValues()...).Info("abandoning empty node consolidation attempt due to pod churn, command is no longer valid")
			return []Command{}, nil
		}
		return []Command{}, err
	}
	// The validator doesn't re-check the buffer
	if lo.SomeBy(validCmd.Candidates, func(c *Candidate) bool { return e.cluster.HasBufferPods(c.ProviderID()) }) {
		fits, err := e.bufferFitsWithout(ctx, validCmd.Candidates...)
		if err != nil && !errors.Is(err, errCandidateDeleting) {
			return []Command{}, err
		}
		if !fits {
			log.FromContext(ctx).V(1).WithValues(validCmd.LogValues()...).Info("abandoning empty node consolidation attempt, buffer pods no longer fit elsewhere")
			return []Command{}, nil
		}
	}
	return []Command{validCmd}, nil
}

// bufferFitsWithout returns true if all virtual pods fit on existing nodes without the candidates.
// It returns false if the simulation has no virtual pods.
func (e *Emptiness) bufferFitsWithout(ctx context.Context, candidates ...*Candidate) (bool, error) {
	results, err := SimulateScheduling(ctx, e.kubeClient, e.cluster, e.provisioner, e.clock, e.recorder, []pscheduling.Options{pscheduling.IsConsolidationSimulation}, SimulationOptions{}, candidates...)
	if err != nil {
		return false, err
	}
	onExistingNodes, onNewNodeClaims, failed := virtualPodPlacement(results)
	return onExistingNodes && !onNewNodeClaims && !failed, nil
}

func (e *Emptiness) Reason() v1.DisruptionReason {
	return v1.DisruptionReasonEmpty
}

func (e *Emptiness) Class() string {
	return GracefulDisruptionClass
}

func (e *Emptiness) ConsolidationType() string {
	return EmptyConsolidationType.Name
}
