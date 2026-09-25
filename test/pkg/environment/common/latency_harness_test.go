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

// Test 3b. Concentrated distribution: all observations land in a middle
// bucket. Because cumulative counts are non-decreasing, a naive right-to-left
// scan over deltaCum would report the top bucket for any non-empty phase.
// inferMaxBound must return the highest bucket that received a per-bucket
// non-zero delta, not the highest index that carries a non-zero cumulative.
func TestReduceHistogramDelta_ConcentratedDistributionMaxBound(t *testing.T) {
	end := mkHistogram(100, 50.0, []*dto.Bucket{
		mkBucket(0.1, 0),
		mkBucket(0.5, 100),
		mkBucket(1.0, 100),
		mkBucket(5.0, 100),
		mkBucket(10.0, 100),
	})
	stats := reduceHistogramDelta(end, nil)
	if math.Abs(stats.Max-0.5) > 1e-9 {
		t.Errorf("Max under concentrated distribution: got %v, want 0.5 (tightest bucket with samples)", stats.Max)
	}
}

// inferMinBound returns the lower bound of the lowest bucket with samples.
func TestReduceHistogramDelta_MinBound(t *testing.T) {
	start := mkHistogram(10, 1.0, []*dto.Bucket{mkBucket(0.1, 10), mkBucket(0.5, 10), mkBucket(1.0, 10)})
	end := mkHistogram(15, 4.0, []*dto.Bucket{mkBucket(0.1, 10), mkBucket(0.5, 12), mkBucket(1.0, 15)})
	if stats := reduceHistogramDelta(end, start); math.Abs(stats.Min-0.1) > 1e-9 {
		t.Errorf("Min: got %v, want 0.1 (start-of-phase samples in the 0.1 bucket must not count)", stats.Min)
	}
	if stats := reduceHistogramDelta(start, nil); stats.Min != 0 {
		t.Errorf("Min: got %v, want 0 (samples in the first bucket)", stats.Min)
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

// scoreBuckets builds the production karpenter_consolidation_score bucket
// layout from cumulative counts, so the tests below exercise the same bounds
// the Balanced threshold assertion reads.
func scoreBuckets(cum ...uint64) []*dto.Bucket {
	bounds := []float64{0.1, 0.25, 0.33, 0.5, 1.0, 2.0, 5.0, 10.0}
	if len(cum) != len(bounds) {
		panic("scoreBuckets: cum length must match the production bucket count")
	}
	out := make([]*dto.Bucket, len(bounds))
	for i, b := range bounds {
		out[i] = mkBucket(b, cum[i])
	}
	return out
}

// Test 5b. Restart where the post-restart histogram overtakes the pre-restart
// one on BOTH sample_count and sample_sum, so neither scalar reveals the reset.
// Only a per-bucket comparison does. Subtracting the stale baseline here
// under-reports Count and, because the resulting cumulative delta is no longer
// monotonic, hides the highest occupied bucket from inferMaxBound. That turns a
// rejected score of 9.0 into a reported Max of 0.33, which silently satisfies
// the rejected arm's Max <= 0.5.
func TestReduceHistogramDelta_ResetWithHigherEndCountAndSum(t *testing.T) {
	// Pre-restart: 100 observations at 0.9, so le=1.0 and above.
	start := mkHistogram(100, 90.0, scoreBuckets(0, 0, 0, 0, 100, 100, 100, 100))
	// Post-restart: 130 at 0.3 (le=0.33) plus 20 at 9.0 (le=10.0).
	// count 150 > 100 and sum 219 > 90, so both scalar checks pass.
	end := mkHistogram(150, 219.0, scoreBuckets(0, 0, 130, 130, 130, 130, 130, 150))

	if end.GetSampleCount() <= start.GetSampleCount() || end.GetSampleSum() <= start.GetSampleSum() {
		t.Fatal("fixture no longer exercises the scalar-checks-pass path")
	}
	stats := reduceHistogramDelta(end, start)
	if stats.Count != 150 {
		t.Errorf("Count: got %d, want 150 (a reset must discard the stale baseline)", stats.Count)
	}
	if math.Abs(stats.Max-10.0) > 1e-9 {
		t.Errorf("Max: got %v, want 10.0 (the 9.0 observations must stay visible)", stats.Max)
	}
	if math.Abs(stats.Min-0.25) > 1e-9 {
		t.Errorf("Min: got %v, want 0.25", stats.Min)
	}
}

// Test 5c. Min and Max over the production score layout. Pins the bounds the
// Balanced threshold assertion compares against, which no other test covers,
// and pins the approved arm's blind interval so a later tightening has a
// failing test to work against.
func TestReduceHistogramDelta_ScoreBucketBounds(t *testing.T) {
	// A score of exactly 0.5 is approved (>= threshold) and lands in le=0.5,
	// whose lower bound is 0.33. This is the case that forces the assertion's
	// bound down to 0.33 rather than 0.5.
	atThreshold := reduceHistogramDelta(mkHistogram(40, 20.0, scoreBuckets(0, 0, 0, 40, 40, 40, 40, 40)), nil)
	if math.Abs(atThreshold.Min-0.33) > 1e-9 {
		t.Errorf("Min at score 0.5: got %v, want 0.33", atThreshold.Min)
	}
	// A score of 0.4 must be rejected, but it lands in that same le=0.5 bucket,
	// so a run that wrongly approves it is bucket-identical to the run above and
	// reports the same Min of 0.33. Min >= 0.33 cannot separate the two. That is
	// the (0.33, 0.5) blind interval, stated here as an equality on purpose.
	wronglyApproved := reduceHistogramDelta(mkHistogram(40, 16.0, scoreBuckets(0, 0, 0, 40, 40, 40, 40, 40)), nil)
	if wronglyApproved.Min != atThreshold.Min {
		t.Errorf("blind interval closed unexpectedly: Min %v vs %v; the approved arm can now distinguish 0.4 from 0.5, so tighten the assertion",
			wronglyApproved.Min, atThreshold.Min)
	}
	// A comfortably approved score of 0.6 skips le=0.5 entirely, so Min rises.
	approved := reduceHistogramDelta(mkHistogram(40, 24.0, scoreBuckets(0, 0, 0, 0, 40, 40, 40, 40)), nil)
	if math.Abs(approved.Min-0.5) > 1e-9 {
		t.Errorf("Min at score 0.6: got %v, want 0.5", approved.Min)
	}
	// Rejected: 40 observations at 0.2, so le=0.25 is the only occupied bucket.
	rejected := reduceHistogramDelta(mkHistogram(40, 8.0, scoreBuckets(0, 40, 40, 40, 40, 40, 40, 40)), nil)
	if math.Abs(rejected.Max-0.25) > 1e-9 {
		t.Errorf("rejected Max at score 0.2: got %v, want 0.25", rejected.Max)
	}
}

// Test 5d. PercentilesUnreliable tracks the minimum sample count.
func TestReduceHistogramDelta_PercentilesUnreliable(t *testing.T) {
	low := reduceHistogramDelta(mkHistogram(5, 1.0, scoreBuckets(5, 5, 5, 5, 5, 5, 5, 5)), nil)
	if !low.PercentilesUnreliable {
		t.Errorf("PercentilesUnreliable: got false at Count=5, want true")
	}
	high := reduceHistogramDelta(mkHistogram(40, 8.0, scoreBuckets(0, 40, 40, 40, 40, 40, 40, 40)), nil)
	if high.PercentilesUnreliable {
		t.Errorf("PercentilesUnreliable: got true at Count=40, want false")
	}
}

// Test 5e. A counter that overtakes its pre-restart value carries no intrinsic
// evidence of the reset, so deltaCounter's own end < start check cannot fire and
// a stale baseline is subtracted. A bare counter has no structure to check
// monotonicity across, so no per-series detector exists for this case.
//
// Resets are process-wide, which is the way out: a histogram in the same scrape
// does carry the evidence, and a Karpenter restart zeroes every metric the
// process exports, so one series going backwards invalidates the baseline for
// all of them.
func TestSnapshotWentBackwards_CounterOvertookAfterRestart(t *testing.T) {
	counterName := "karpenter_consolidation_moves_total"
	histName := "karpenter_consolidation_score"

	start := map[string]*dto.MetricFamily{
		counterName: mkFamily(counterName, dto.MetricType_COUNTER, mkCounterMetric(100, nil)),
		histName: mkFamily(histName, dto.MetricType_HISTOGRAM,
			mkMetric(mkHistogram(100, 90.0, scoreBuckets(0, 0, 0, 0, 100, 100, 100, 100)), nil)),
	}
	end := map[string]*dto.MetricFamily{
		// 130 >= 100, so on its own this counter looks like a clean +30.
		counterName: mkFamily(counterName, dto.MetricType_COUNTER, mkCounterMetric(130, nil)),
		// Post-restart: 130 observations at 0.3 plus 20 at 9.0. sample_count and
		// sample_sum both rose, so only the bucket monotonicity check can see it.
		histName: mkFamily(histName, dto.MetricType_HISTOGRAM,
			mkMetric(mkHistogram(150, 219.0, scoreBuckets(0, 0, 130, 130, 130, 130, 130, 150)), nil)),
	}

	// Pin the information limit: the counter alone reports the wrong delta and
	// has no way to know. This is the behavior that makes snapshot-wide
	// detection necessary, not the behavior we want to keep.
	if got := deltaCounter(counterName, start[counterName], end[counterName])[counterName]; got != 30 {
		t.Errorf("deltaCounter against a stale baseline: got %d, want 30; if this changed, a per-series counter detector now exists and this test needs rewriting", got)
	}

	series, backwards := snapshotWentBackwards(start, end)
	if !backwards {
		t.Fatal("snapshotWentBackwards: got false, want true; the score histogram went backwards, so the whole baseline is invalid")
	}
	if series != histName {
		t.Errorf("offending series: got %q, want %q", series, histName)
	}

	// With the baseline dropped, the counter reports the post-restart total.
	if got := deltaCounter(counterName, nil, end[counterName])[counterName]; got != 130 {
		t.Errorf("deltaCounter after dropping the baseline: got %d, want 130", got)
	}
}

// Test 5f. snapshotWentBackwards must not fire on a valid pair of scrapes, or
// every delta would silently become an absolute total.
func TestSnapshotWentBackwards_ValidProgressIsNotAReset(t *testing.T) {
	counterName := "karpenter_consolidation_moves_total"
	histName := "karpenter_consolidation_score"

	start := map[string]*dto.MetricFamily{
		counterName: mkFamily(counterName, dto.MetricType_COUNTER, mkCounterMetric(100, nil)),
		histName: mkFamily(histName, dto.MetricType_HISTOGRAM,
			mkMetric(mkHistogram(100, 60.0, scoreBuckets(0, 0, 0, 40, 100, 100, 100, 100)), nil)),
	}
	end := map[string]*dto.MetricFamily{
		counterName: mkFamily(counterName, dto.MetricType_COUNTER, mkCounterMetric(175, nil)),
		histName: mkFamily(histName, dto.MetricType_HISTOGRAM,
			mkMetric(mkHistogram(175, 130.0, scoreBuckets(0, 0, 0, 55, 175, 175, 175, 175)), nil)),
	}
	if series, backwards := snapshotWentBackwards(start, end); backwards {
		t.Errorf("snapshotWentBackwards on valid progress: got true for %q, want false", series)
	}
	// A series present only at end must not count as backwards movement either.
	end["karpenter_nodes_created_total"] = mkFamily("karpenter_nodes_created_total", dto.MetricType_COUNTER, mkCounterMetric(12, nil))
	if series, backwards := snapshotWentBackwards(start, end); backwards {
		t.Errorf("snapshotWentBackwards with a new series at end: got true for %q, want false", series)
	}
	// A nil start snapshot is the already-reset case, not a reset to detect.
	if _, backwards := snapshotWentBackwards(nil, end); backwards {
		t.Error("snapshotWentBackwards with nil start: got true, want false")
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

// Test 10a. Prometheus text-parser retains the +Inf bucket. The reducer must
// exclude it from percentile / Max derivation and derive truncation from the
// last finite bucket instead. Regression for a subtle bug where percentiles
// beyond the finite tail returned +Inf and truncation-rate silently
// returned 0.
func TestReduceHistogramDelta_InfBucketExcludedFromPercentiles(t *testing.T) {
	end := mkHistogram(100, 800.0, []*dto.Bucket{
		mkBucket(1.0, 0),
		mkBucket(5.0, 20),
		mkBucket(10.0, 50),
		mkBucket(math.Inf(+1), 100),
	})
	stats := reduceHistogramDelta(end, nil)
	if stats.Count != 100 {
		t.Errorf("Count: got %d, want 100", stats.Count)
	}
	if math.IsInf(stats.P95, +1) {
		t.Errorf("P95 leaked +Inf: got %v", stats.P95)
	}
	if math.Abs(stats.P95-10.0) > 1e-9 {
		t.Errorf("P95 under truncation: got %v, want 10.0 (last finite bound)", stats.P95)
	}
	if math.IsInf(stats.Max, +1) {
		t.Errorf("Max leaked +Inf: got %v", stats.Max)
	}
	if math.Abs(stats.Max-10.0) > 1e-9 {
		t.Errorf("Max: got %v, want 10.0 (tightest non-zero finite bucket)", stats.Max)
	}
	if math.Abs(stats.BucketTruncationRate-0.5) > 1e-9 {
		t.Errorf("BucketTruncationRate: got %v, want 0.5", stats.BucketTruncationRate)
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
