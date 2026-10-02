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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

// scoreBuckets builds the production karpenter_consolidation_score bucket
// layout from cumulative counts, so the specs below exercise the same bounds
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

var _ = Describe("LatencyHarness", func() {
	Context("reduceHistogramDelta", func() {
		// Uniform bucket layout, single-series, easy percentiles.
		// 100 observations delta split evenly across buckets [0.1, 0.5, 1.0, 2.0].
		// P50 = 0.5 (median lands at bucket 2 upper edge), P90 = 1.6 (interpolated).
		It("should derive count, sum, mean and percentiles from a uniform distribution", func() {
			end := mkHistogram(100, 30.0, []*dto.Bucket{
				mkBucket(0.1, 25),
				mkBucket(0.5, 50),
				mkBucket(1.0, 75),
				mkBucket(2.0, 100),
			})
			stats := reduceHistogramDelta(end, nil)
			Expect(stats.Count).To(BeNumerically("==", 100))
			Expect(stats.Sum).To(BeNumerically("~", 30.0, 1e-9))
			Expect(stats.Mean).To(BeNumerically("~", 0.3, 1e-9))
			Expect(stats.P50).To(BeNumerically("~", 0.5, 1e-9))
			// P90 target = 90. prev bucket cum=75, cur=100, prev upper=1.0, cur upper=2.0.
			// P90 = 1.0 + (2.0 - 1.0) * (90 - 75) / (100 - 75) = 1.0 + 0.6 = 1.6.
			Expect(stats.P90).To(BeNumerically("~", 1.6, 1e-9))
			Expect(stats.BucketTruncationRate).To(BeZero())
			Expect(stats.Max).To(BeNumerically("~", 2.0, 1e-9))
		})

		// Delta reduction subtracts start-of-phase observations correctly.
		// Start snapshot has 50 total; end has 150. Delta is 100 with the tail newly
		// filled; P50 should reflect only the new observations, not the start pool.
		It("should subtract the start snapshot", func() {
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
			Expect(stats.Count).To(BeNumerically("==", 100))
			Expect(stats.Sum).To(BeNumerically("~", 50.0, 1e-9))
			// Delta cumulative buckets: 0, 25, 50, 100.
			// P50 target = 50. cum=50 at upper=1.0. Return 1.0 exactly.
			Expect(stats.P50).To(BeNumerically("~", 1.0, 1e-9))
			// P90 target = 90. prev cum=50 (upper=1.0), cur cum=100 (upper=2.0).
			// P90 = 1.0 + (2.0-1.0) * (90-50)/(100-50) = 1.0 + 0.8 = 1.8.
			Expect(stats.P90).To(BeNumerically("~", 1.8, 1e-9))
		})

		// Truncation-rate reports observations that fell into +Inf.
		// End buckets total 90 within the finite tail while sample_count is 100;
		// 10 observations exceeded the top bucket. Truncation rate = 0.10.
		It("should report bucket truncation and cap percentiles at the last finite bound", func() {
			end := mkHistogram(100, 500.0, []*dto.Bucket{
				mkBucket(1.0, 40),
				mkBucket(5.0, 70),
				mkBucket(10.0, 90),
			})
			stats := reduceHistogramDelta(end, nil)
			Expect(stats.Count).To(BeNumerically("==", 100))
			Expect(stats.BucketTruncationRate).To(BeNumerically("~", 0.10, 1e-9))
			// P95 target = 95. prev cum=90 (upper=10.0), no next finite bucket -> +Inf.
			// Falls back to last finite upper bound.
			Expect(stats.P95).To(BeNumerically("~", 10.0, 1e-9), "P95 under truncation")
		})

		// Concentrated distribution: all observations land in a middle bucket.
		// Because cumulative counts are non-decreasing, a naive right-to-left
		// scan over deltaCum would report the top bucket for any non-empty phase.
		// inferMaxBound must return the highest bucket that received a per-bucket
		// non-zero delta, not the highest index that carries a non-zero cumulative.
		It("should infer Max from the tightest bucket with samples under a concentrated distribution", func() {
			end := mkHistogram(100, 50.0, []*dto.Bucket{
				mkBucket(0.1, 0),
				mkBucket(0.5, 100),
				mkBucket(1.0, 100),
				mkBucket(5.0, 100),
				mkBucket(10.0, 100),
			})
			stats := reduceHistogramDelta(end, nil)
			Expect(stats.Max).To(BeNumerically("~", 0.5, 1e-9), "Max under concentrated distribution")
		})

		// inferMinBound returns the lower bound of the lowest bucket with samples.
		It("should infer Min from the lowest bucket with new samples", func() {
			start := mkHistogram(10, 1.0, []*dto.Bucket{mkBucket(0.1, 10), mkBucket(0.5, 10), mkBucket(1.0, 10)})
			end := mkHistogram(15, 4.0, []*dto.Bucket{mkBucket(0.1, 10), mkBucket(0.5, 12), mkBucket(1.0, 15)})
			Expect(reduceHistogramDelta(end, start).Min).To(BeNumerically("~", 0.1, 1e-9),
				"start-of-phase samples in the 0.1 bucket must not count")
			Expect(reduceHistogramDelta(start, nil).Min).To(BeZero(), "samples in the first bucket")
		})

		// Zero-observation phase yields zero-valued stats.
		It("should return zero-valued stats when no new observations landed", func() {
			same := mkHistogram(50, 5.0, []*dto.Bucket{
				mkBucket(0.1, 25),
				mkBucket(1.0, 50),
			})
			stats := reduceHistogramDelta(same, same)
			Expect(stats.Count).To(BeZero())
			Expect(stats.P50).To(BeZero())
			Expect(stats.P90).To(BeZero())
			Expect(stats.P95).To(BeZero())
			Expect(stats.P99).To(BeZero())
		})

		// Counter-reset (pod restart) between snapshots. end_count < start_count
		// should fall back to end as fresh observations.
		It("should treat end as fresh observations when the sample count went backwards", func() {
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
			Expect(stats.Count).To(BeNumerically("==", 30), "Count under reset")
			Expect(stats.Sum).To(BeNumerically("~", 3.0, 1e-9), "Sum under reset")
		})

		// Restart where the post-restart histogram overtakes the pre-restart
		// one on BOTH sample_count and sample_sum, so neither scalar reveals the reset.
		// Only a per-bucket comparison does. Subtracting the stale baseline here
		// under-reports Count and, because the resulting cumulative delta is no longer
		// monotonic, hides the highest occupied bucket from inferMaxBound. That turns a
		// rejected score of 9.0 into a reported Max of 0.33, which silently satisfies
		// the rejected arm's Max <= 0.5.
		It("should discard the stale baseline when neither sample_count nor sample_sum reveals the reset", func() {
			// Pre-restart: 100 observations at 0.9, so le=1.0 and above.
			start := mkHistogram(100, 90.0, scoreBuckets(0, 0, 0, 0, 100, 100, 100, 100))
			// Post-restart: 130 at 0.3 (le=0.33) plus 20 at 9.0 (le=10.0).
			// count 150 > 100 and sum 219 > 90, so both scalar checks pass.
			end := mkHistogram(150, 219.0, scoreBuckets(0, 0, 130, 130, 130, 130, 130, 150))

			Expect(end.GetSampleCount()).To(BeNumerically(">", start.GetSampleCount()),
				"fixture no longer exercises the scalar-checks-pass path")
			Expect(end.GetSampleSum()).To(BeNumerically(">", start.GetSampleSum()),
				"fixture no longer exercises the scalar-checks-pass path")

			stats := reduceHistogramDelta(end, start)
			Expect(stats.Count).To(BeNumerically("==", 150), "a reset must discard the stale baseline")
			Expect(stats.Max).To(BeNumerically("~", 10.0, 1e-9), "the 9.0 observations must stay visible")
			Expect(stats.Min).To(BeNumerically("~", 0.25, 1e-9))
		})

		// Min and Max over the production score layout. Pins the bounds the
		// Balanced threshold assertion compares against, which no other spec covers,
		// and pins the approved arm's blind interval so a later tightening has a
		// failing spec to work against.
		It("should pin Min and Max over the production score bucket layout", func() {
			// A score of exactly 0.5 is approved (>= threshold) and lands in le=0.5,
			// whose lower bound is 0.33. This is the case that forces the assertion's
			// bound down to 0.33 rather than 0.5.
			atThreshold := reduceHistogramDelta(mkHistogram(40, 20.0, scoreBuckets(0, 0, 0, 40, 40, 40, 40, 40)), nil)
			Expect(atThreshold.Min).To(BeNumerically("~", 0.33, 1e-9), "Min at score 0.5")
			// A score of 0.4 must be rejected, but it lands in that same le=0.5 bucket,
			// so a run that wrongly approves it is bucket-identical to the run above and
			// reports the same Min of 0.33. Min >= 0.33 cannot separate the two. That is
			// the (0.33, 0.5) blind interval, stated here as an equality on purpose.
			wronglyApproved := reduceHistogramDelta(mkHistogram(40, 16.0, scoreBuckets(0, 0, 0, 40, 40, 40, 40, 40)), nil)
			Expect(wronglyApproved.Min).To(Equal(atThreshold.Min),
				"blind interval closed unexpectedly; the approved arm can now distinguish 0.4 from 0.5, so tighten the assertion")
			// A comfortably approved score of 0.6 skips le=0.5 entirely, so Min rises.
			approved := reduceHistogramDelta(mkHistogram(40, 24.0, scoreBuckets(0, 0, 0, 0, 40, 40, 40, 40)), nil)
			Expect(approved.Min).To(BeNumerically("~", 0.5, 1e-9), "Min at score 0.6")
			// Rejected: 40 observations at 0.2, so le=0.25 is the only occupied bucket.
			rejected := reduceHistogramDelta(mkHistogram(40, 8.0, scoreBuckets(0, 40, 40, 40, 40, 40, 40, 40)), nil)
			Expect(rejected.Max).To(BeNumerically("~", 0.25, 1e-9), "rejected Max at score 0.2")
		})

		// PercentilesUnreliable tracks the minimum sample count.
		It("should flag percentiles unreliable below the minimum sample count", func() {
			low := reduceHistogramDelta(mkHistogram(5, 1.0, scoreBuckets(5, 5, 5, 5, 5, 5, 5, 5)), nil)
			Expect(low.PercentilesUnreliable).To(BeTrue(), "PercentilesUnreliable at Count=5")
			high := reduceHistogramDelta(mkHistogram(40, 8.0, scoreBuckets(0, 40, 40, 40, 40, 40, 40, 40)), nil)
			Expect(high.PercentilesUnreliable).To(BeFalse(), "PercentilesUnreliable at Count=40")
		})

		// The Prometheus text-parser retains the +Inf bucket. The reducer must
		// exclude it from percentile / Max derivation and derive truncation from the
		// last finite bucket instead. Regression for a subtle bug where percentiles
		// beyond the finite tail returned +Inf and truncation-rate silently
		// returned 0.
		It("should exclude the +Inf bucket from percentiles and Max", func() {
			end := mkHistogram(100, 800.0, []*dto.Bucket{
				mkBucket(1.0, 0),
				mkBucket(5.0, 20),
				mkBucket(10.0, 50),
				mkBucket(math.Inf(+1), 100),
			})
			stats := reduceHistogramDelta(end, nil)
			Expect(stats.Count).To(BeNumerically("==", 100))
			Expect(math.IsInf(stats.P95, +1)).To(BeFalse(), "P95 leaked +Inf")
			Expect(stats.P95).To(BeNumerically("~", 10.0, 1e-9), "P95 under truncation is the last finite bound")
			Expect(math.IsInf(stats.Max, +1)).To(BeFalse(), "Max leaked +Inf")
			Expect(stats.Max).To(BeNumerically("~", 10.0, 1e-9), "Max is the tightest non-zero finite bucket")
			Expect(stats.BucketTruncationRate).To(BeNumerically("~", 0.5, 1e-9))
		})
	})

	Context("snapshotWentBackwards", func() {
		// A counter that overtakes its pre-restart value carries no intrinsic
		// evidence of the reset, so deltaCounter's own end < start check cannot fire and
		// a stale baseline is subtracted. A bare counter has no structure to check
		// monotonicity across, so no per-series detector exists for this case.
		//
		// Resets are process-wide, which is the way out: a histogram in the same scrape
		// does carry the evidence, and a Karpenter restart zeroes every metric the
		// process exports, so one series going backwards invalidates the baseline for
		// all of them.
		It("should detect a reset the counter alone cannot reveal", func() {
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
			Expect(deltaCounter(counterName, start[counterName], end[counterName])[counterName]).To(BeNumerically("==", 30),
				"deltaCounter against a stale baseline; if this changed, a per-series counter detector now exists and this spec needs rewriting")

			series, backwards := snapshotWentBackwards(start, end)
			Expect(backwards).To(BeTrue(),
				"the score histogram went backwards, so the whole baseline is invalid")
			Expect(series).To(Equal(histName), "offending series")

			// With the baseline dropped, the counter reports the post-restart total.
			Expect(deltaCounter(counterName, nil, end[counterName])[counterName]).To(BeNumerically("==", 130),
				"deltaCounter after dropping the baseline")
		})

		// snapshotWentBackwards must not fire on a valid pair of scrapes, or
		// every delta would silently become an absolute total.
		It("should not treat valid progress as a reset", func() {
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
			series, backwards := snapshotWentBackwards(start, end)
			Expect(backwards).To(BeFalse(), "valid progress reported %q as backwards", series)

			// A series present only at end must not count as backwards movement either.
			end["karpenter_nodes_created_total"] = mkFamily("karpenter_nodes_created_total", dto.MetricType_COUNTER, mkCounterMetric(12, nil))
			series, backwards = snapshotWentBackwards(start, end)
			Expect(backwards).To(BeFalse(), "a new series at end reported %q as backwards", series)

			// A nil start snapshot is the already-reset case, not a reset to detect.
			_, backwards = snapshotWentBackwards(nil, end)
			Expect(backwards).To(BeFalse(), "a nil start snapshot is the already-reset case")
		})
	})

	Context("deltaHistogram", func() {
		// Multi-series histogram: same metric name, different label sets.
		// deltaHistogram should emit one HistogramStats per (name, label-fingerprint).
		It("should emit one HistogramStats per label fingerprint", func() {
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
			Expect(out).To(HaveLen(2))
			singleKey := name + "{consolidation_type=single,reason=underutilized}"
			multiKey := name + "{consolidation_type=multi,reason=underutilized}"
			Expect(out).To(HaveKey(singleKey))
			Expect(out).To(HaveKey(multiKey))
			Expect(out[singleKey].Count).To(BeNumerically("==", 10), "single count")
			Expect(out[multiKey].Count).To(BeNumerically("==", 5), "multi count")
			Expect(out[singleKey].Labels).To(HaveKeyWithValue("consolidation_type", "single"))
		})

		// deltaHistogram tolerates a missing metric family from either side.
		It("should return no series for a missing metric family", func() {
			Expect(deltaHistogram("karpenter_missing_metric", nil, nil)).To(BeEmpty())
		})
	})

	Context("seriesKey", func() {
		// seriesKey is deterministic under label reordering.
		It("should be stable under label reordering", func() {
			name := "karpenter_consolidation_score"
			a, av := "decision", "approved"
			b, bv := "nodepool", "pool-a"
			c, cv := "policy", "Balanced"
			forward := []*dto.LabelPair{{Name: &a, Value: &av}, {Name: &b, Value: &bv}, {Name: &c, Value: &cv}}
			reverse := []*dto.LabelPair{{Name: &c, Value: &cv}, {Name: &b, Value: &bv}, {Name: &a, Value: &av}}
			Expect(seriesKey(name, forward)).To(Equal(seriesKey(name, reverse)))
			Expect(seriesKey(name, forward)).To(Equal(name + "{decision=approved,nodepool=pool-a,policy=Balanced}"))
		})
	})

	Context("deltaCounter", func() {
		// Counter delta subtracts start value; reset falls back to end.
		It("should subtract the start value and fall back to end on a reset", func() {
			name := "karpenter_voluntary_disruption_consolidation_timeouts_total"
			lbl := map[string]string{"consolidation_type": "single"}
			start := mkFamily(name, dto.MetricType_COUNTER, mkCounterMetric(3, lbl))
			end := mkFamily(name, dto.MetricType_COUNTER, mkCounterMetric(8, lbl))
			key := name + "{consolidation_type=single}"
			Expect(deltaCounter(name, start, end)[key]).To(BeNumerically("==", 5), "counter delta")

			// Reset case: end < start -> take end as the delta.
			resetV := 2.0
			end.Metric[0].Counter.Value = &resetV
			Expect(deltaCounter(name, start, end)[key]).To(BeNumerically("==", 2), "counter reset")
		})
	})

	Context("compactFamilies", func() {
		It("should keep only the target metric families", func() {
			families := map[string]*dto.MetricFamily{
				"karpenter_pods_scheduling_decision_duration_seconds":         mkFamily("karpenter_pods_scheduling_decision_duration_seconds", dto.MetricType_HISTOGRAM),
				"karpenter_voluntary_disruption_consolidation_timeouts_total": mkFamily("karpenter_voluntary_disruption_consolidation_timeouts_total", dto.MetricType_COUNTER),
				"go_gc_duration_seconds":                                      mkFamily("go_gc_duration_seconds", dto.MetricType_SUMMARY),
				"process_open_fds":                                            mkFamily("process_open_fds", dto.MetricType_GAUGE),
				"workqueue_adds_total":                                        mkFamily("workqueue_adds_total", dto.MetricType_COUNTER),
			}
			out := compactFamilies(families)
			Expect(out).To(HaveKey("karpenter_pods_scheduling_decision_duration_seconds"), "compact dropped a target histogram")
			Expect(out).To(HaveKey("karpenter_voluntary_disruption_consolidation_timeouts_total"), "compact dropped a target counter")
			Expect(out).ToNot(HaveKey("go_gc_duration_seconds"), "compact retained a non-target family")
			Expect(out).To(HaveLen(2))
		})
	})
})
