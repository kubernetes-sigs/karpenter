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
	"math"
	"time"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// reconcileCyclePeriod is the approximate duration of one disruption reconciliation cycle.
const reconcileCyclePeriod = pollingPeriod

// driftSLOState holds computed drift SLO state for a NodePool.
type driftSLOState struct {
	remainingDrifted int
	oldestDrift      time.Time
}

// getDriftSLOState computes the drift SLO state for a NodePool from cluster nodes.
func getDriftSLOState(nodePool *v1.NodePool, nodes state.StateNodes) driftSLOState {
	var s driftSLOState
	for _, node := range nodes {
		if node.NodeClaim == nil {
			continue
		}
		if node.Labels()[v1.NodePoolLabelKey] != nodePool.Name {
			continue
		}
		cond := node.NodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)
		if cond == nil || !cond.IsTrue() {
			continue
		}
		s.remainingDrifted++
		t := cond.LastTransitionTime.Time
		if s.oldestDrift.IsZero() || t.Before(s.oldestDrift) {
			s.oldestDrift = t
		}
	}
	return s
}

// ComputeDriftShare computes the number of budget slots to reserve for drift
// based on the drift SLO annotation on the NodePool. It returns 0 if no SLO
// is configured or no nodes are drifted.
func ComputeDriftShare(nodePool *v1.NodePool, nodes state.StateNodes, totalBudget int, now time.Time) int {
	slo := nodePool.GetDriftSLO()
	if slo == nil {
		return 0
	}
	s := getDriftSLOState(nodePool, nodes)
	if s.remainingDrifted == 0 {
		return 0
	}

	deadline := s.oldestDrift.Add(*slo)
	remainingTime := deadline.Sub(now)

	// Past deadline: drift gets maximum priority
	if remainingTime <= 0 {
		return totalBudget
	}

	remainingCycles := math.Max(float64(remainingTime)/float64(reconcileCyclePeriod), 1)
	driftShare := int(math.Ceil(float64(s.remainingDrifted) / remainingCycles))

	if driftShare > totalBudget {
		return totalBudget
	}
	return driftShare
}

// emitDriftSLOWarnings emits warning events and updates metrics for drift SLO state.
func emitDriftSLOWarnings(nodePool *v1.NodePool, nodes state.StateNodes, availableBudget int, now time.Time, recorder events.Recorder) {
	slo := nodePool.GetDriftSLO()
	if slo == nil {
		return
	}
	s := getDriftSLOState(nodePool, nodes)
	if s.remainingDrifted == 0 {
		return
	}

	deadline := s.oldestDrift.Add(*slo)
	remainingSeconds := deadline.Sub(now).Seconds()

	// Update metrics
	DriftSLORemainingSeconds.Set(remainingSeconds, map[string]string{
		metrics.NodePoolLabel: nodePool.Name,
	})
	DriftSLORemainingNodes.Set(float64(s.remainingDrifted), map[string]string{
		metrics.NodePoolLabel: nodePool.Name,
	})

	// Emit warning if deadline has passed
	if remainingSeconds <= 0 {
		recorder.Publish(disruptionevents.DriftSLOExceeded(nodePool, s.remainingDrifted))
		return
	}

	// Emit warning if projected drift rate won't meet SLO
	remainingCycles := math.Max(remainingSeconds/reconcileCyclePeriod.Seconds(), 1)
	requiredPerCycle := math.Ceil(float64(s.remainingDrifted) / remainingCycles)
	if int(requiredPerCycle) > availableBudget {
		recorder.Publish(disruptionevents.DriftSLOAtRisk(nodePool, s.remainingDrifted, availableBudget))
	}
}
