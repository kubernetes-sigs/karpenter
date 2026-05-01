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
	"testing"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// RFC example pool: 10 nodes, eight m7i.xlarge ($4.84/day) and two m7i.2xlarge ($9.68/day).
// Total NodePool cost = $58.08/day. 80 pods, total disruption cost = 90 (10 nodes * 1.0 + 80 pods * 1.0).
var defaultTotals = NodePoolTotals{
	TotalCost:           58.08,
	TotalDisruptionCost: 90,
}

const defaultTolerance int32 = 2

func approxEqual(a, b, epsilon float64) bool {
	return math.Abs(a-b) < epsilon
}

// Test: Oversized node -> score 2.81, approved
// savings = 7.26, disruption = 1.0 (node) + 3.0 (pods) = 4.0
func TestScoreMove_OversizedNode_Approved(t *testing.T) {
	savings := 7.26       // m7i.2xlarge - m7i.large = 9.68 - 2.42
	disruptionCost := 4.0 // 1.0 per-node + 3 pods with default cost
	result := ScoreMove(savings, disruptionCost, defaultTotals, defaultTolerance)

	if !result.Approved {
		t.Errorf("expected approved, got rejected (score=%.2f)", result.Score)
	}
	if !approxEqual(result.Score, 2.81, 0.02) {
		t.Errorf("expected score ~2.81, got %.2f", result.Score)
	}
	if !approxEqual(result.SavingsFraction, 0.125, 0.001) {
		t.Errorf("expected savings_fraction ~0.125, got %.4f", result.SavingsFraction)
	}
	if !approxEqual(result.DisruptionFraction, 0.0444, 0.001) {
		t.Errorf("expected disruption_fraction ~0.0444, got %.4f", result.DisruptionFraction)
	}
}

// Test: Spare capacity delete -> score 1.50, approved
func TestScoreMove_SpareCapacityDelete_Approved(t *testing.T) {
	savings := 4.84       // full m7i.xlarge cost
	disruptionCost := 5.0 // 1.0 per-node + 4 pods
	result := ScoreMove(savings, disruptionCost, defaultTotals, defaultTolerance)

	if !result.Approved {
		t.Errorf("expected approved, got rejected (score=%.2f)", result.Score)
	}
	if !approxEqual(result.Score, 1.50, 0.02) {
		t.Errorf("expected score ~1.50, got %.2f", result.Score)
	}
}

// Test: Marginal move -> score 0.42, rejected
func TestScoreMove_MarginalMove_Rejected(t *testing.T) {
	savings := 2.42       // m7i.xlarge - m7i.large
	disruptionCost := 9.0 // 1.0 per-node + 8 pods
	result := ScoreMove(savings, disruptionCost, defaultTotals, defaultTolerance)

	if result.Approved {
		t.Errorf("expected rejected, got approved (score=%.2f)", result.Score)
	}
	if !approxEqual(result.Score, 0.42, 0.01) {
		t.Errorf("expected score ~0.42, got %.2f", result.Score)
	}
}

// Test: Well-packed node (0% savings) -> score 0, rejected
func TestScoreMove_WellPackedNode_Rejected(t *testing.T) {
	savings := 0.0
	disruptionCost := 11.0 // 1.0 per-node + 10 pods
	result := ScoreMove(savings, disruptionCost, defaultTotals, defaultTolerance)

	if result.Approved {
		t.Errorf("expected rejected, got approved (score=%.2f)", result.Score)
	}
	if result.Score != 0 {
		t.Errorf("expected score 0, got %.2f", result.Score)
	}
}

// Test: Uniform pool replace -> score 0.50, approved
func TestScoreMove_UniformPoolReplace_Approved(t *testing.T) {
	uniformTotals := NodePoolTotals{
		TotalCost:           48.40,
		TotalDisruptionCost: 90, // 10 nodes + 80 pods
	}
	savings := 2.42       // m7i.xlarge - m7i.large
	disruptionCost := 9.0 // 1.0 per-node + 8 pods
	result := ScoreMove(savings, disruptionCost, uniformTotals, defaultTolerance)

	if !result.Approved {
		t.Errorf("expected approved, got rejected (score=%.2f)", result.Score)
	}
	if !approxEqual(result.Score, 0.50, 0.01) {
		t.Errorf("expected score ~0.50, got %.2f", result.Score)
	}
}

// Test: Heterogeneous disruption
func TestScoreMove_HeterogeneousDisruption(t *testing.T) {
	// 10 nodes * 1.0 + 107 pod disruption = 117
	hetTotals := NodePoolTotals{
		TotalCost:           58.08,
		TotalDisruptionCost: 117,
	}

	// Node A: disruption = 1.0 (node) + 4.0 (pods) = 5.0
	resultA := ScoreMove(4.84, 5.0, hetTotals, defaultTolerance)
	if !resultA.Approved {
		t.Errorf("Node A: expected approved, got rejected (score=%.2f)", resultA.Score)
	}
	if !approxEqual(resultA.Score, 1.95, 0.02) {
		t.Errorf("Node A: expected score ~1.95, got %.2f", resultA.Score)
	}

	// Node B: disruption = 1.0 (node) + 31.0 (pods) = 32.0
	resultB := ScoreMove(4.84, 32.0, hetTotals, defaultTolerance)
	if resultB.Approved {
		t.Errorf("Node B: expected rejected, got approved (score=%.2f)", resultB.Score)
	}
	if !approxEqual(resultB.Score, 0.30, 0.01) {
		t.Errorf("Node B: expected score ~0.30, got %.2f", resultB.Score)
	}
}

// Test: Scale invariance (10x pool, same score)
func TestScoreMove_ScaleInvariance(t *testing.T) {
	result10 := ScoreMove(7.26, 4.0, defaultTotals, defaultTolerance)

	scaledTotals := NodePoolTotals{
		TotalCost:           580.80,
		TotalDisruptionCost: 900, // 100 nodes + 800 pods
	}
	result100 := ScoreMove(7.26, 4.0, scaledTotals, defaultTolerance)

	if !approxEqual(result10.Score, result100.Score, 0.01) {
		t.Errorf("expected identical scores, got %.2f (10-node) vs %.2f (100-node)", result10.Score, result100.Score)
	}
	if !approxEqual(result10.Score, 2.81, 0.02) {
		t.Errorf("expected score ~2.81, got %.2f", result10.Score)
	}
}

// Test: Zero disruption cost -> +Inf score, approved
func TestScoreMove_ZeroDisruptionCost_Approved(t *testing.T) {
	result := ScoreMove(4.84, 0.0, defaultTotals, defaultTolerance)
	if !result.Approved {
		t.Errorf("expected approved for zero disruption, got rejected")
	}
	if !math.IsInf(result.Score, 1) {
		t.Errorf("expected +Inf score for zero disruption, got %.2f", result.Score)
	}

	resultZero := ScoreMove(0.0, 0.0, defaultTotals, defaultTolerance)
	if resultZero.Approved {
		t.Errorf("expected rejected for zero-savings zero-disruption, got approved")
	}
}

// Test: Zero nodepool cost -> no consolidation
func TestScoreMove_ZeroNodePoolCost(t *testing.T) {
	zeroTotals := NodePoolTotals{TotalCost: 0, TotalDisruptionCost: 90}
	result := ScoreMove(4.84, 5.0, zeroTotals, defaultTolerance)

	if result.Approved {
		t.Errorf("expected rejected when nodepool cost is zero, got approved")
	}
}

// Test: Zero total disruption cost in pool -> any positive savings approved
func TestScoreMove_ZeroTotalDisruptionCost(t *testing.T) {
	zeroDisruptionTotals := NodePoolTotals{TotalCost: 58.08, TotalDisruptionCost: 0}

	result := ScoreMove(4.84, 5.0, zeroDisruptionTotals, defaultTolerance)
	if !result.Approved {
		t.Errorf("expected approved when pool total disruption cost is zero, got rejected")
	}

	resultZero := ScoreMove(0.0, 0.0, zeroDisruptionTotals, defaultTolerance)
	if resultZero.Approved {
		t.Errorf("expected rejected when savings is zero even with zero pool disruption, got approved")
	}
}

// Test: Division by zero when disruption_cost is 0
func TestScoreMove_DivisionByZero_MoveDisruption(t *testing.T) {
	result := ScoreMove(4.84, 0.0, defaultTotals, defaultTolerance)
	if !result.Approved {
		t.Errorf("expected approved for zero move disruption, got rejected")
	}
}

// Test: Division by zero when nodepool_total_disruption_cost is 0
func TestScoreMove_DivisionByZero_PoolDisruption(t *testing.T) {
	totals := NodePoolTotals{TotalCost: 100, TotalDisruptionCost: 0}
	result := ScoreMove(10, 5.0, totals, defaultTolerance)
	if !result.Approved {
		t.Errorf("expected approved for zero pool disruption, got rejected")
	}
}

// Test: Different consolidationThreshold values
func TestScoreMove_ConsolidationThreshold(t *testing.T) {
	savings := 2.42
	disruptionCost := 9.0

	result2 := ScoreMove(savings, disruptionCost, defaultTotals, 2)
	if result2.Approved {
		t.Errorf("k=2: expected rejected (score=%.2f, threshold=0.50)", result2.Score)
	}

	result3 := ScoreMove(savings, disruptionCost, defaultTotals, 3)
	if !result3.Approved {
		t.Errorf("k=3: expected approved (score=%.2f, threshold=0.33)", result3.Score)
	}

	result1 := ScoreMove(savings, disruptionCost, defaultTotals, 1)
	if result1.Approved {
		t.Errorf("k=1: expected rejected (score=%.2f, threshold=1.00)", result1.Score)
	}
}

// Test: GetConsolidationThreshold defaults
func TestGetConsolidationThreshold(t *testing.T) {
	np := &v1.NodePool{}
	if got := GetConsolidationThreshold(np); got != 2 {
		t.Errorf("expected default threshold 2, got %d", got)
	}

	val := int32(3)
	np.Spec.Disruption.ConsolidationThreshold = &val
	if got := GetConsolidationThreshold(np); got != 3 {
		t.Errorf("expected threshold 3, got %d", got)
	}
}

// Test: Cross-NodePool example
func TestScoreMove_CrossNodePool(t *testing.T) {
	odTotals := NodePoolTotals{TotalCost: 48.40, TotalDisruptionCost: 90}
	spotTotals := NodePoolTotals{TotalCost: 14.50, TotalDisruptionCost: 90}

	odResult := ScoreMove(4.84, 4.0, odTotals, defaultTolerance)
	spotResult := ScoreMove(1.45, 4.0, spotTotals, defaultTolerance)

	if !approxEqual(odResult.Score, spotResult.Score, 0.01) {
		t.Errorf("expected identical scores, got OD=%.2f Spot=%.2f", odResult.Score, spotResult.Score)
	}
	if !odResult.Approved || !spotResult.Approved {
		t.Errorf("expected both approved, got OD=%v Spot=%v", odResult.Approved, spotResult.Approved)
	}
}
