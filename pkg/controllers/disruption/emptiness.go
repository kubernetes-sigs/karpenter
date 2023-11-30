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

package disruption

import (
	"context"

	"github.com/samber/lo"
	"k8s.io/utils/clock"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// Emptiness is a subreconciler that deletes empty candidates.
// Emptiness will respect TTLSecondsAfterEmpty
type Emptiness struct {
	clock clock.Clock
}

func NewEmptiness(clk clock.Clock) *Emptiness {
	return &Emptiness{
		clock: clk,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (e *Emptiness) ShouldDisrupt(_ context.Context, c *Candidate) bool {
	return c.nodePool.Spec.Disruption.ConsolidateAfter != nil &&
		c.nodePool.Spec.Disruption.ConsolidateAfter.Duration != nil &&
		c.nodePool.Spec.Disruption.ConsolidationPolicy == v1beta1.ConsolidationPolicyWhenEmpty &&
		c.NodeClaim.StatusConditions().GetCondition(v1beta1.Empty).IsTrue() &&
		!e.clock.Now().Before(c.NodeClaim.StatusConditions().GetCondition(v1beta1.Empty).LastTransitionTime.Inner.Add(*c.nodePool.Spec.Disruption.ConsolidateAfter.Duration))
}

// ComputeCommand generates a disruption command given candidates
func (e *Emptiness) ComputeCommand(_ context.Context, candidates ...*Candidate) (Command, error) {
	emptyCandidates := lo.Filter(candidates, func(cn *Candidate, _ int) bool {
		return cn.NodeClaim.DeletionTimestamp.IsZero() && len(cn.pods) == 0
	})
	disruptionEligibleNodesGauge.With(map[string]string{
		methodLabel:            e.Type(),
		consolidationTypeLabel: e.ConsolidationType(),
	}).Set(float64(len(candidates)))

	return Command{
		candidates: emptyCandidates,
	}, nil
}

func (e *Emptiness) Type() string {
	return metrics.EmptinessReason
}

func (e *Emptiness) ConsolidationType() string {
	return ""
}
