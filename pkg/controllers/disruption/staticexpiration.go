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
	"math"
	"sort"

	"github.com/samber/lo"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"

	"sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// StaticExpiration replaces expired NodeClaims in static NodePools while respecting the NodePool's disruption budget.
// Static pools cannot use the normal scheduling simulation because their replica count, rather than pending workload,
// determines the desired number of NodeClaims.
type StaticExpiration struct {
	cluster  *state.Cluster
	recorder events.Recorder
	clock    clock.Clock
}

func NewStaticExpiration(cluster *state.Cluster, recorder events.Recorder, clk clock.Clock) *StaticExpiration {
	return &StaticExpiration{cluster: cluster, recorder: recorder, clock: clk}
}

func (e *StaticExpiration) ShouldDisrupt(_ context.Context, c *Candidate) bool {
	return c.OwnedByStaticNodePool() && nodeclaim.IsExpired(c.NodeClaim, e.clock)
}

func (e *StaticExpiration) ComputeCommands(_ context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) ([]Command, error) {
	candidatesByNodePool := lo.GroupBy(candidates, func(candidate *Candidate) string { return candidate.NodePool.Name })
	var cmds []Command
	for npName, npCandidates := range candidatesByNodePool {
		np := npCandidates[0].NodePool
		if disruptionBudgetMapping[npName] == 0 {
			continue
		}
		sort.Slice(npCandidates, func(i, j int) bool {
			iTime, _ := nodeclaim.ExpirationTime(npCandidates[i].NodeClaim)
			jTime, _ := nodeclaim.ExpirationTime(npCandidates[j].NodeClaim)
			return iTime.Before(jTime)
		})

		limit, ok := np.Spec.Limits[resources.Node]
		nodeLimit := lo.Ternary(ok, limit.Value(), int64(math.MaxInt64))
		runningNodes, _, nodesPendingDisruptionCount := e.cluster.NodePoolState.GetNodeCount(npName)
		if int64(runningNodes+nodesPendingDisruptionCount) > lo.FromPtr(np.Spec.Replicas) {
			continue
		}
		maxExpirations := lo.Min([]int64{int64(disruptionBudgetMapping[npName]), int64(len(npCandidates))})
		maxAllowedExpirations := e.cluster.NodePoolState.ReserveNodeCount(npName, nodeLimit, maxExpirations)
		if maxAllowedExpirations == 0 {
			for _, candidate := range npCandidates[:maxExpirations] {
				e.recorder.Publish(disruptionevents.Blocked(candidate.Node, candidate.NodeClaim,
					"static NodePool is at its node limit and cannot stage an expiration replacement")...)
			}
			continue
		}

		for _, candidate := range npCandidates[:maxAllowedExpirations] {
			nct := scheduling.NewNodeClaimTemplate(np)
			result := scheduling.Results{NewNodeClaims: []*scheduling.NodeClaim{{NodeClaimTemplate: *nct}}}
			cmds = append(cmds, Command{
				Candidates:          []*Candidate{candidate},
				Replacements:        replacementsFromNodeClaims(result.NewNodeClaims...),
				Results:             result,
				PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
			})
		}
	}
	return cmds, nil
}

func (e *StaticExpiration) Reason() v1.DisruptionReason { return v1.DisruptionReasonExpired }
func (e *StaticExpiration) Class() string               { return EventualDisruptionClass }
func (e *StaticExpiration) ConsolidationType() string   { return "" }
