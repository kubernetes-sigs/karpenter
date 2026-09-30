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

func mkBucket(upper float64, cum uint64) *dto.Bucket {
	return &dto.Bucket{UpperBound: &upper, CumulativeCount: &cum}
}

func mkHistogram(count uint64, sum float64, buckets []*dto.Bucket) *dto.Histogram {
	return &dto.Histogram{SampleCount: &count, SampleSum: &sum, Bucket: buckets}
}

func mkMetric(h *dto.Histogram, labels map[string]string) *dto.Metric {
	m := &dto.Metric{Histogram: h}
	for k, v := range labels {
		name, val := k, v
		m.Label = append(m.Label, &dto.LabelPair{Name: &name, Value: &val})
	}
	return m
}

func mkFamily(name string, mtype dto.MetricType, metrics ...*dto.Metric) *dto.MetricFamily {
	n, t := name, mtype
	return &dto.MetricFamily{Name: &n, Type: &t, Metric: metrics}
}

func mkCounterMetric(v float64, labels map[string]string) *dto.Metric {
	m := &dto.Metric{Counter: &dto.Counter{Value: &v}}
	for k, val := range labels {
		name, value := k, val
		m.Label = append(m.Label, &dto.LabelPair{Name: &name, Value: &value})
	}
	return m
}

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
		It("should derive count, sum and mean from a uniform distribution", func() {
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
			Expect(stats.BucketTruncationRate).To(BeZero())
			Expect(stats.Min).To(BeZero())
			Expect(stats.Max).To(BeNumerically("~", 2.0, 1e-9))
		})

		It("should subtract the start snapshot", func() {
			start := mkHistogram(50, 5.0, []*dto.Bucket{
				mkBucket(0.1, 50),
				mkBucket(0.5, 50),
				mkBucket(1.0, 50),
				mkBucket(2.0, 50),
			})
			end := mkHistogram(150, 55.0, []*dto.Bucket{
				mkBucket(0.1, 50),
				mkBucket(0.5, 75),
				mkBucket(1.0, 100),
				mkBucket(2.0, 150),
			})
			stats := reduceHistogramDelta(end, start)
			Expect(stats.Count).To(BeNumerically("==", 100))
			Expect(stats.Sum).To(BeNumerically("~", 50.0, 1e-9))
			Expect(stats.Min).To(BeNumerically("~", 0.1, 1e-9),
				"Min over the delta buckets; an unsubtracted 0.1 bucket would report 0")
		})

		It("should report bucket truncation and cap Max at the last finite bound", func() {
			end := mkHistogram(100, 500.0, []*dto.Bucket{
				mkBucket(1.0, 40),
				mkBucket(5.0, 70),
				mkBucket(10.0, 90),
			})
			stats := reduceHistogramDelta(end, nil)
			Expect(stats.Count).To(BeNumerically("==", 100))
			Expect(stats.BucketTruncationRate).To(BeNumerically("~", 0.10, 1e-9))
			Expect(stats.Max).To(BeNumerically("~", 10.0, 1e-9), "Max under truncation")
		})

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

		It("should infer Min from the lowest bucket with new samples", func() {
			start := mkHistogram(10, 1.0, []*dto.Bucket{mkBucket(0.1, 10), mkBucket(0.5, 10), mkBucket(1.0, 10)})
			end := mkHistogram(15, 4.0, []*dto.Bucket{mkBucket(0.1, 10), mkBucket(0.5, 12), mkBucket(1.0, 15)})
			Expect(reduceHistogramDelta(end, start).Min).To(BeNumerically("~", 0.1, 1e-9),
				"start-of-phase samples in the 0.1 bucket must not count")
			Expect(reduceHistogramDelta(start, nil).Min).To(BeZero(), "samples in the first bucket")
		})

		It("should return zero-valued stats when no new observations landed", func() {
			same := mkHistogram(50, 5.0, []*dto.Bucket{
				mkBucket(0.1, 25),
				mkBucket(1.0, 50),
			})
			stats := reduceHistogramDelta(same, same)
			Expect(stats.Count).To(BeZero())
			Expect(stats.Sum).To(BeZero())
			Expect(stats.Mean).To(BeZero(), "Mean must not divide by a zero Count")
			Expect(stats.Min).To(BeZero())
			Expect(stats.Max).To(BeZero(), "Max must not report a bucket bound nothing landed in")
			Expect(stats.BucketTruncationRate).To(BeZero())
		})

		It("should treat end as fresh observations when the sample count went backwards", func() {
			start := mkHistogram(200, 50.0, []*dto.Bucket{
				mkBucket(1.0, 200),
				mkBucket(5.0, 200),
			})
			end := mkHistogram(30, 3.0, []*dto.Bucket{
				mkBucket(1.0, 20),
				mkBucket(5.0, 30),
			})
			stats := reduceHistogramDelta(end, start)
			Expect(stats.Count).To(BeNumerically("==", 30), "Count under reset")
			Expect(stats.Sum).To(BeNumerically("~", 3.0, 1e-9), "Sum under reset")
		})

		It("should discard the stale baseline when neither sample_count nor sample_sum reveals the reset", func() {
			start := mkHistogram(100, 90.0, scoreBuckets(0, 0, 0, 0, 100, 100, 100, 100))
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

		It("should pin Min and Max over the production score bucket layout", func() {
			atThreshold := reduceHistogramDelta(mkHistogram(40, 20.0, scoreBuckets(0, 0, 0, 40, 40, 40, 40, 40)), nil)
			Expect(atThreshold.Min).To(BeNumerically("~", 0.33, 1e-9), "Min at score 0.5")
			wronglyApproved := reduceHistogramDelta(mkHistogram(40, 16.0, scoreBuckets(0, 0, 0, 40, 40, 40, 40, 40)), nil)
			Expect(wronglyApproved.Min).To(Equal(atThreshold.Min),
				"blind interval closed unexpectedly; the approved arm can now distinguish 0.4 from 0.5, so tighten the assertion")
			approved := reduceHistogramDelta(mkHistogram(40, 24.0, scoreBuckets(0, 0, 0, 0, 40, 40, 40, 40)), nil)
			Expect(approved.Min).To(BeNumerically("~", 0.5, 1e-9), "Min at score 0.6")
			rejected := reduceHistogramDelta(mkHistogram(40, 8.0, scoreBuckets(0, 40, 40, 40, 40, 40, 40, 40)), nil)
			Expect(rejected.Max).To(BeNumerically("~", 0.25, 1e-9), "rejected Max at score 0.2")
		})

		It("should exclude the +Inf bucket from Max and the truncation rate", func() {
			end := mkHistogram(100, 800.0, []*dto.Bucket{
				mkBucket(1.0, 0),
				mkBucket(5.0, 20),
				mkBucket(10.0, 50),
				mkBucket(math.Inf(+1), 100),
			})
			stats := reduceHistogramDelta(end, nil)
			Expect(stats.Count).To(BeNumerically("==", 100))
			Expect(math.IsInf(stats.Max, +1)).To(BeFalse(), "Max leaked +Inf")
			Expect(stats.Max).To(BeNumerically("~", 10.0, 1e-9), "Max is the tightest non-zero finite bucket")
			Expect(stats.BucketTruncationRate).To(BeNumerically("~", 0.5, 1e-9))
		})
	})

	Context("deltaHistogram", func() {
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

		It("should return no series for a missing metric family", func() {
			Expect(deltaHistogram("karpenter_missing_metric", nil, nil)).To(BeEmpty())
		})
	})

	Context("seriesKey", func() {
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
		It("should subtract the start value and fall back to end on a reset", func() {
			name := "karpenter_voluntary_disruption_consolidation_timeouts_total"
			lbl := map[string]string{"consolidation_type": "single"}
			start := mkFamily(name, dto.MetricType_COUNTER, mkCounterMetric(3, lbl))
			end := mkFamily(name, dto.MetricType_COUNTER, mkCounterMetric(8, lbl))
			key := name + "{consolidation_type=single}"
			Expect(deltaCounter(name, start, end)[key]).To(BeNumerically("==", 5), "counter delta")

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
