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

	"github.com/samber/lo"
	"k8s.io/utils/clock"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/metrics"
)

// Emptiness is a subreconciler that deletes empty machines.
// Emptiness will respect TTLSecondsAfterEmpty
type Emptiness struct {
	clock clock.Clock
}

func NewEmptiness(clk clock.Clock) *Emptiness {
	return &Emptiness{
		clock: clk,
	}
}

// ShouldDeprovision is a predicate used to filter deprovisionable machines
func (e *Emptiness) ShouldDeprovision(_ context.Context, c *Candidate) bool {
	cond := c.Machine.StatusConditions().GetCondition(v1alpha5.MachineVoluntarilyDisrupted)
	return cond.IsTrue() && cond.Reason == v1alpha5.VoluntarilyDisruptedReasonEmpty
}

// ComputeCommand generates a deprovisioning command given deprovisionable machines
func (e *Emptiness) ComputeCommand(_ context.Context, candidates ...*Candidate) (Command, error) {
	emptyCandidates := lo.Filter(candidates, func(cn *Candidate, _ int) bool {
		return cn.Machine.DeletionTimestamp.IsZero() && len(cn.pods) == 0
	})
	deprovisioningEligibleMachinesGauge.WithLabelValues(e.String()).Set(float64(len(candidates)))

	return Command{
		candidates: emptyCandidates,
	}, nil
}

// string is the string representation of the deprovisioner
func (e *Emptiness) String() string {
	return metrics.EmptinessReason
}
