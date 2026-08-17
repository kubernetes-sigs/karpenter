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

package common

import (
	"math"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// mkBucket returns a *dto.Bucket with the given upper bound and cumulative count.
func mkBucket(upper float64, cum uint64) *dto.Bucket {
	return &dto.Bucket{UpperBound: &upper, CumulativeCount: &cum}
}

// mkHistogram returns a *dto.Histogram with the given cumulative buckets and
// total sample_count / sample_sum. The buckets slice MUST be sorted by
// upper bound ascending; cum is cumulative (Prometheus convention).
func mkHistogram(count uint64, sum float64, buckets []*dto.Bucket) *dto.Histogram {
	return &dto.Histogram{SampleCount: &count, SampleSum: &sum, Bucket: buckets}
}

// mkMetric wraps a histogram into a labeled dto.Metric.
func mkMetric(h *dto.Histogram, labels map[string]string) *dto.Metric {
	m := &dto.Metric{Histogram: h}
	for k, v := range labels {
		name, val := k, v
		m.Label = append(m.Label, &dto.LabelPair{Name: &name, Value: &val})
	}
	return m
}

// mkFamily wraps a set of Metric into a MetricFamily of the given type.
func mkFamily(name string, mtype dto.MetricType, metrics ...*dto.Metric) *dto.MetricFamily {
	n, t := name, mtype
	return &dto.MetricFamily{Name: &n, Type: &t, Metric: metrics}
}

// mkCounterMetric wraps a counter value into a labeled dto.Metric.
func mkCounterMetric(v float64, labels map[string]string) *dto.Metric {
	m := &dto.Metric{Counter: &dto.Counter{Value: &v}}
	for k, val := range labels {
		name, value := k, val
		m.Label = append(m.Label, &dto.LabelPair{Name: &name, Value: &value})
	}
	return m
}

// Test 1. Uniform bucket layout, single-series, easy percentiles.
// 100 observations delta split evenly across buckets [0.1, 0.5, 1.0, 2.0].
// P50 = 0.5 (median lands at bucket 2 upper edge), P90 = 1.6 (interpolated).
func TestReduceHistogramDelta_UniformDistribution(t *testing.T) {
	end := mkHistogram(100, 30.0, []*dto.Bucket{
		mkBucket(0.1, 25),
		mkBucket(0.5, 50),
		mkBucket(1.0, 75),
		mkBucket(2.0, 100),
	})
	stats := reduceHistogramDelta(end, nil)
	if stats.Count != 100 {
		t.Errorf("Count: got %d, want 100", stats.Count)
	}
	if math.Abs(stats.Sum-30.0) > 1e-9 {
		t.Errorf("Sum: got %v, want 30.0", stats.Sum)
	}
	if math.Abs(stats.Mean-0.3) > 1e-9 {
		t.Errorf("Mean: got %v, want 0.3", stats.Mean)
	}
	if math.Abs(stats.P50-0.5) > 1e-9 {
		t.Errorf("P50: got %v, want 0.5", stats.P50)
	}
	// P90 target = 90. prev bucket cum=75, cur=100, prev upper=1.0, cur upper=2.0.
	// P90 = 1.0 + (2.0 - 1.0) * (90 - 75) / (100 - 75) = 1.0 + 0.6 = 1.6.
	if math.Abs(stats.P90-1.6) > 1e-9 {
		t.Errorf("P90: got %v, want 1.6", stats.P90)
	}
	if stats.BucketTruncationRate != 0 {
		t.Errorf("BucketTruncationRate: got %v, want 0", stats.BucketTruncationRate)
	}
	if math.Abs(stats.Max-2.0) > 1e-9 {
		t.Errorf("Max: got %v, want 2.0", stats.Max)
	}
}

// Test 2. Delta reduction subtracts start-of-phase observations correctly.
// Start snapshot has 50 total; end has 150. Delta is 100 with the tail newly
// filled; P50 should reflect only the new observations, not the start pool.
func TestReduceHistogramDelta_SubtractsStartSnapshot(t *testing.T) {
	start := mkHistogram(50, 5.0, []*dto.Bucket{
		mkBucket(0.1, 50),
		mkBucket(0.5, 50),
		mkBucket(1.0, 50),
		mkBucket(2.0, 50),
	})
	end := mkHistogram(150, 55.0, []*dto.Bucket{
		mkBucket(0.1, 50), // no new observations in this bucket
		mkBucket(0.5, 75),
		mkBucket(1.0, 100),
		mkBucket(2.0, 150),
	})
	stats := reduceHistogramDelta(end, start)
	if stats.Count != 100 {
		t.Errorf("Count: got %d, want 100", stats.Count)
	}
	if math.Abs(stats.Sum-50.0) > 1e-9 {
		t.Errorf("Sum: got %v, want 50.0", stats.Sum)
	}
	// Delta cumulative buckets: 0, 25, 50, 100.
	// P50 target = 50. cum=50 at upper=1.0. Return 1.0 exactly.
	if math.Abs(stats.P50-1.0) > 1e-9 {
		t.Errorf("P50: got %v, want 1.0", stats.P50)
	}
	// P90 target = 90. prev cum=50 (upper=1.0), cur cum=100 (upper=2.0).
	// P90 = 1.0 + (2.0-1.0) * (90-50)/(100-50) = 1.0 + 0.8 = 1.8.
	if math.Abs(stats.P90-1.8) > 1e-9 {
		t.Errorf("P90: got %v, want 1.8", stats.P90)
	}
}

// Test 3. Truncation-rate reports observations that fell into +Inf.
// End buckets total 90 within the finite tail while sample_count is 100;
// 10 observations exceeded the top bucket. Truncation rate = 0.10.
func TestReduceHistogramDelta_BucketTruncation(t *testing.T) {
	end := mkHistogram(100, 500.0, []*dto.Bucket{
		mkBucket(1.0, 40),
		mkBucket(5.0, 70),
		mkBucket(10.0, 90),
	})
	stats := reduceHistogramDelta(end, nil)
	if stats.Count != 100 {
		t.Errorf("Count: got %d, want 100", stats.Count)
	}
	if math.Abs(stats.BucketTruncationRate-0.10) > 1e-9 {
		t.Errorf("BucketTruncationRate: got %v, want 0.10", stats.BucketTruncationRate)
	}
	// P95 target = 95. prev cum=90 (upper=10.0), no next finite bucket -> +Inf.
	// Falls back to last finite upper bound.
	if math.Abs(stats.P95-10.0) > 1e-9 {
		t.Errorf("P95 under truncation: got %v, want 10.0", stats.P95)
	}
}

// Test 4. Zero-observation phase yields zero-valued stats.
func TestReduceHistogramDelta_NoNewObservations(t *testing.T) {
	same := mkHistogram(50, 5.0, []*dto.Bucket{
		mkBucket(0.1, 25),
		mkBucket(1.0, 50),
	})
	stats := reduceHistogramDelta(same, same)
	if stats.Count != 0 {
		t.Errorf("Count: got %d, want 0", stats.Count)
	}
	if stats.P50 != 0 || stats.P90 != 0 || stats.P95 != 0 || stats.P99 != 0 {
		t.Errorf("percentiles under zero-count: want all zero, got P50=%v P90=%v P95=%v P99=%v",
			stats.P50, stats.P90, stats.P95, stats.P99)
	}
}

// Test 5. Counter-reset (pod restart) between snapshots. end_count < start_count
// should fall back to end as fresh observations.
func TestReduceHistogramDelta_CounterReset(t *testing.T) {
	start := mkHistogram(200, 50.0, []*dto.Bucket{
		mkBucket(1.0, 200),
		mkBucket(5.0, 200),
	})
	// Pod restarted; new counter is smaller than the pre-restart baseline.
	end := mkHistogram(30, 3.0, []*dto.Bucket{
		mkBucket(1.0, 20),
		mkBucket(5.0, 30),
	})
	stats := reduceHistogramDelta(end, start)
	if stats.Count != 30 {
		t.Errorf("Count under reset: got %d, want 30", stats.Count)
	}
	if math.Abs(stats.Sum-3.0) > 1e-9 {
		t.Errorf("Sum under reset: got %v, want 3.0", stats.Sum)
	}
}

// Test 6. Multi-series histogram: same metric name, different label sets.
// deltaHistogram should emit one HistogramStats per (name, label-fingerprint).
func TestDeltaHistogram_MultiSeries(t *testing.T) {
	name := "karpenter_voluntary_disruption_decision_evaluation_duration_seconds"
	single := mkMetric(mkHistogram(10, 1.0, []*dto.Bucket{
		mkBucket(0.1, 10),
	}), map[string]string{"consolidation_type": "single", "reason": "underutilized"})
	multi := mkMetric(mkHistogram(5, 2.5, []*dto.Bucket{
		mkBucket(0.1, 2),
		mkBucket(1.0, 5),
	}), map[string]string{"consolidation_type": "multi", "reason": "underutilized"})
	end := mkFamily(name, dto.MetricType_HISTOGRAM, single, multi)
	out := deltaHistogram(name, nil, end)
	if len(out) != 2 {
		t.Fatalf("series count: got %d, want 2 (%v)", len(out), out)
	}
	singleKey := name + "{consolidation_type=single,reason=underutilized}"
	multiKey := name + "{consolidation_type=multi,reason=underutilized}"
	if _, ok := out[singleKey]; !ok {
		t.Errorf("missing series key %q; got %v", singleKey, out)
	}
	if _, ok := out[multiKey]; !ok {
		t.Errorf("missing series key %q; got %v", multiKey, out)
	}
	if out[singleKey].Count != 10 {
		t.Errorf("single count: got %d, want 10", out[singleKey].Count)
	}
	if out[multiKey].Count != 5 {
		t.Errorf("multi count: got %d, want 5", out[multiKey].Count)
	}
	if lbl := out[singleKey].Labels["consolidation_type"]; lbl != "single" {
		t.Errorf("single labels.consolidation_type: got %q, want %q", lbl, "single")
	}
}

// Test 7. seriesKey is deterministic under label reordering.
func TestSeriesKey_StableSort(t *testing.T) {
	name := "karpenter_consolidation_score"
	a, av := "decision", "approved"
	b, bv := "nodepool", "pool-a"
	c, cv := "policy", "Balanced"
	forward := []*dto.LabelPair{{Name: &a, Value: &av}, {Name: &b, Value: &bv}, {Name: &c, Value: &cv}}
	reverse := []*dto.LabelPair{{Name: &c, Value: &cv}, {Name: &b, Value: &bv}, {Name: &a, Value: &av}}
	if seriesKey(name, forward) != seriesKey(name, reverse) {
		t.Errorf("seriesKey not stable under reorder: %q vs %q", seriesKey(name, forward), seriesKey(name, reverse))
	}
	want := name + "{decision=approved,nodepool=pool-a,policy=Balanced}"
	if got := seriesKey(name, forward); got != want {
		t.Errorf("seriesKey format: got %q, want %q", got, want)
	}
}

// Test 8. Counter delta subtracts start value; reset falls back to end.
func TestDeltaCounter_DeltaAndReset(t *testing.T) {
	name := "karpenter_voluntary_disruption_consolidation_timeouts_total"
	lbl := map[string]string{"consolidation_type": "single"}
	start := mkFamily(name, dto.MetricType_COUNTER, mkCounterMetric(3, lbl))
	end := mkFamily(name, dto.MetricType_COUNTER, mkCounterMetric(8, lbl))
	out := deltaCounter(name, start, end)
	key := name + "{consolidation_type=single}"
	if out[key] != 5 {
		t.Errorf("counter delta: got %d, want 5", out[key])
	}
	// Reset case: end < start -> take end as the delta.
	resetV := 2.0
	end.Metric[0].Counter.Value = &resetV
	out = deltaCounter(name, start, end)
	if out[key] != 2 {
		t.Errorf("counter reset: got %d, want 2", out[key])
	}
}

// Test 9. deltaHistogram tolerates a missing metric family from either side.
func TestDeltaHistogram_MissingMetric(t *testing.T) {
	out := deltaHistogram("karpenter_missing_metric", nil, nil)
	if len(out) != 0 {
		t.Errorf("missing metric: got %d series, want 0", len(out))
	}
}

// Test 10. compactFamilies keeps only the target metric families.
func TestCompactFamilies(t *testing.T) {
	families := map[string]*dto.MetricFamily{
		"karpenter_pods_scheduling_decision_duration_seconds":         mkFamily("karpenter_pods_scheduling_decision_duration_seconds", dto.MetricType_HISTOGRAM),
		"karpenter_voluntary_disruption_consolidation_timeouts_total": mkFamily("karpenter_voluntary_disruption_consolidation_timeouts_total", dto.MetricType_COUNTER),
		"go_gc_duration_seconds":                                      mkFamily("go_gc_duration_seconds", dto.MetricType_SUMMARY),
		"process_open_fds":                                            mkFamily("process_open_fds", dto.MetricType_GAUGE),
		"workqueue_adds_total":                                        mkFamily("workqueue_adds_total", dto.MetricType_COUNTER),
	}
	out := compactFamilies(families)
	if _, ok := out["karpenter_pods_scheduling_decision_duration_seconds"]; !ok {
		t.Errorf("compact dropped a target histogram")
	}
	if _, ok := out["karpenter_voluntary_disruption_consolidation_timeouts_total"]; !ok {
		t.Errorf("compact dropped a target counter")
	}
	if _, ok := out["go_gc_duration_seconds"]; ok {
		t.Errorf("compact retained a non-target family")
	}
	if len(out) != 2 {
		t.Errorf("compact size: got %d, want 2", len(out))
	}
}
