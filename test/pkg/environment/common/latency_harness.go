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
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// HistogramStats is the derived percentile summary of one labeled histogram
// series over the observations added between LatencyHarness.Start and
// LatencyHarness.Stop.
type HistogramStats struct {
	MetricName           string            `json:"metric_name"`
	Labels               map[string]string `json:"labels,omitempty"`
	Count                uint64            `json:"count"`
	Sum                  float64           `json:"sum"`
	Mean                 float64           `json:"mean"`
	P50                  float64           `json:"p50"`
	P90                  float64           `json:"p90"`
	P95                  float64           `json:"p95"`
	P99                  float64           `json:"p99"`
	Max                  float64           `json:"max"`
	BucketTruncationRate float64           `json:"bucket_truncation_rate"`
}

// TargetHistograms is the Karpenter histogram set the harness scrapes.
var TargetHistograms = []string{
	"karpenter_pods_scheduling_decision_duration_seconds",
	"karpenter_pods_bound_duration_seconds",
	"karpenter_pods_provisioning_bound_duration_seconds",
	"karpenter_pods_provisioning_startup_duration_seconds",
	"karpenter_scheduler_scheduling_duration_seconds",
	"karpenter_voluntary_disruption_decision_evaluation_duration_seconds",
	"karpenter_cloudprovider_duration_seconds",
	"karpenter_nodeclaims_instance_termination_duration_seconds",
	"karpenter_nodeclaims_termination_duration_seconds",
	"karpenter_consolidation_score",
}

// TargetCounters is the Karpenter counter set the harness reports as deltas
// between Start and Stop, keyed the same way as LatencyStats.
var TargetCounters = []string{
	"karpenter_voluntary_disruption_consolidation_timeouts_total",
	"karpenter_consolidation_moves_total",
	"karpenter_nodeclaims_created_total",
	"karpenter_nodes_created_total",
}

// LatencyResult is what LatencyHarness.Stop returns. Keys of LatencyStats and
// Counters are series fingerprints (see seriesKey). Process-level memory and
// CPU are covered by KarpenterMetricsPoller; run both harnesses in tandem if
// resource-usage stats are needed.
type LatencyResult struct {
	LatencyStats map[string]HistogramStats
	Counters     map[string]uint64
}

// LatencyHarness captures a start-of-phase snapshot of Karpenter's /metrics
// endpoint and produces per-histogram percentile summaries by bucket-count
// delta at Stop. It reuses the pod-proxy scrape pattern from
// KarpenterMetricsPoller.
type LatencyHarness struct {
	env     *Environment
	podName string
	start   map[string]*dto.MetricFamily
}

// StartLatencyHarness discovers the active Karpenter pod, scrapes /metrics
// once, and stores a compacted snapshot (target series only) for later delta
// reduction. Symmetric with StartKarpenterMetricsPoller.
func StartLatencyHarness(env *Environment) (*LatencyHarness, error) {
	pod, err := env.FindActiveKarpenterPod(env.Context)
	if err != nil || pod == nil {
		return nil, fmt.Errorf("finding karpenter pod: %w", err)
	}
	h := &LatencyHarness{env: env, podName: pod.Name}
	families, err := scrapeKarpenterMetricFamilies(env.Context, env, pod.Name)
	if err != nil {
		return nil, fmt.Errorf("initial scrape: %w", err)
	}
	h.start = compactFamilies(families)
	GinkgoWriter.Printf("LatencyHarness: started, scraping pod kube-system/%s\n", pod.Name)
	return h, nil
}

// Stop scrapes the end snapshot and reduces the histogram / counter deltas
// into a LatencyResult. On the first scrape failure the harness refreshes the
// active-pod name once (matches KarpenterMetricsPoller's leader-election
// handling) and retries; a second failure returns the error.
func (h *LatencyHarness) Stop() (*LatencyResult, error) {
	ctx := h.env.Context
	end, err := scrapeKarpenterMetricFamilies(ctx, h.env, h.podName)
	if err != nil {
		if pod, findErr := h.env.FindActiveKarpenterPod(ctx); findErr == nil && pod != nil && pod.Name != h.podName {
			GinkgoWriter.Printf("LatencyHarness: active pod changed from %s to %s, retrying scrape\n", h.podName, pod.Name)
			h.podName = pod.Name
			end, err = scrapeKarpenterMetricFamilies(ctx, h.env, pod.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("end scrape: %w", err)
		}
	}
	res := &LatencyResult{
		LatencyStats: map[string]HistogramStats{},
		Counters:     map[string]uint64{},
	}
	for _, name := range TargetHistograms {
		for key, stats := range deltaHistogram(name, h.start[name], end[name]) {
			res.LatencyStats[key] = stats
		}
	}
	for _, name := range TargetCounters {
		for key, delta := range deltaCounter(name, h.start[name], end[name]) {
			res.Counters[key] = delta
		}
	}
	GinkgoWriter.Printf("LatencyHarness: stopped, %d histogram series, %d counter series\n",
		len(res.LatencyStats), len(res.Counters))
	return res, nil
}

// scrapeKarpenterMetricFamilies fetches and parses /metrics from a Karpenter
// pod via the API-server pod proxy. Shared between LatencyHarness and
// KarpenterMetricsPoller.
func scrapeKarpenterMetricFamilies(ctx context.Context, env *Environment, podName string) (map[string]*dto.MetricFamily, error) {
	data, err := env.KubeClient.CoreV1().Pods("kube-system").ProxyGet("http", podName, "8080", "/metrics", nil).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxy GET /metrics: %w", err)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parsing metrics: %w", err)
	}
	return families, nil
}

// compactFamilies retains only the metric families the harness reduces at
// Stop, plus process gauges the poller reads. The full Karpenter /metrics
// response contains hundreds of families; retaining only the target set
// keeps memory bounded across long test phases.
func compactFamilies(families map[string]*dto.MetricFamily) map[string]*dto.MetricFamily {
	keep := make(map[string]*dto.MetricFamily, len(TargetHistograms)+len(TargetCounters))
	for _, n := range TargetHistograms {
		if f, ok := families[n]; ok {
			keep[n] = f
		}
	}
	for _, n := range TargetCounters {
		if f, ok := families[n]; ok {
			keep[n] = f
		}
	}
	return keep
}

// seriesKey returns the canonical fingerprint for a labeled sample:
// "metric_name" or "metric_name{k=v,k2=v2,...}" with keys sorted lexically.
func seriesKey(name string, labels []*dto.LabelPair) string {
	if len(labels) == 0 {
		return name
	}
	pairs := make([]string, 0, len(labels))
	for _, l := range labels {
		pairs = append(pairs, l.GetName()+"="+l.GetValue())
	}
	sort.Strings(pairs)
	return name + "{" + strings.Join(pairs, ",") + "}"
}

// labelMap returns the labels of a Metric as a plain map for HistogramStats.
func labelMap(labels []*dto.LabelPair) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for _, l := range labels {
		out[l.GetName()] = l.GetValue()
	}
	return out
}

// deltaHistogram computes per-series stats from the count delta between two
// snapshots of the same MetricFamily. Nil start or end families are treated
// as empty. Series present only at end are emitted with their end histogram
// as the whole delta.
func deltaHistogram(name string, start, end *dto.MetricFamily) map[string]HistogramStats {
	out := map[string]HistogramStats{}
	if end == nil {
		return out
	}
	startBySeries := indexBySeries(name, start)
	for _, m := range end.GetMetric() {
		if m.GetHistogram() == nil {
			continue
		}
		key := seriesKey(name, m.GetLabel())
		s := reduceHistogramDelta(m.GetHistogram(), startBySeries[key].GetHistogram())
		s.MetricName = name
		s.Labels = labelMap(m.GetLabel())
		out[key] = s
	}
	return out
}

// deltaCounter computes counter-value deltas between two snapshots. Nil start
// yields the raw end value; a counter reset (end < start) yields end (the new
// baseline is treated as fresh observation).
func deltaCounter(name string, start, end *dto.MetricFamily) map[string]uint64 {
	out := map[string]uint64{}
	if end == nil {
		return out
	}
	startBySeries := indexBySeries(name, start)
	for _, m := range end.GetMetric() {
		if m.GetCounter() == nil {
			continue
		}
		key := seriesKey(name, m.GetLabel())
		endV := m.GetCounter().GetValue()
		startV := 0.0
		if prev, ok := startBySeries[key]; ok && prev.GetCounter() != nil {
			startV = prev.GetCounter().GetValue()
		}
		delta := endV - startV
		if delta < 0 {
			// Counter reset (pod restart); take end as-is. endV is a Prometheus
			// counter value and cannot be negative.
			delta = endV
		}
		out[key] = uint64(delta)
	}
	return out
}

// indexBySeries returns metrics from mf keyed by seriesKey.
func indexBySeries(name string, mf *dto.MetricFamily) map[string]*dto.Metric {
	out := map[string]*dto.Metric{}
	if mf == nil {
		return out
	}
	for _, m := range mf.GetMetric() {
		out[seriesKey(name, m.GetLabel())] = m
	}
	return out
}

// reduceHistogramDelta subtracts the start histogram from end (bucket-wise
// and on sample_count / sample_sum) and derives percentile stats over the
// resulting bucket distribution. Both histograms MUST share the same bucket
// layout; deltas for missing start-buckets treat startCumulative as 0.
func reduceHistogramDelta(end *dto.Histogram, startHistogram *dto.Histogram) HistogramStats {
	if end == nil {
		return HistogramStats{}
	}
	endCount := end.GetSampleCount()
	endSum := end.GetSampleSum()
	startCount, startSum, startCumBy := resolveDeltaBaseline(startHistogram, endCount)
	deltaCount := endCount - startCount
	if deltaCount == 0 {
		return HistogramStats{Count: 0, Sum: endSum - startSum}
	}
	endBuckets := end.GetBucket()
	deltaCum := make([]uint64, len(endBuckets))
	for i, b := range endBuckets {
		endCum := b.GetCumulativeCount()
		startCum := startCumBy[b.GetUpperBound()]
		if endCum < startCum {
			startCum = 0
		}
		deltaCum[i] = endCum - startCum
	}
	// Prometheus's text parser retains the +Inf bucket in end.GetBucket().
	// Percentile / Max derivation must run against the finite tail only;
	// truncation rate is what the finite tail failed to capture.
	finiteBuckets, finiteCum := endBuckets, deltaCum
	if n := len(endBuckets); n > 0 && math.IsInf(endBuckets[n-1].GetUpperBound(), +1) {
		finiteBuckets = endBuckets[:n-1]
		finiteCum = deltaCum[:n-1]
	}
	lastFiniteCum := uint64(0)
	if len(finiteCum) > 0 {
		lastFiniteCum = finiteCum[len(finiteCum)-1]
	}
	trunc := 0.0
	if deltaCount > lastFiniteCum {
		trunc = float64(deltaCount-lastFiniteCum) / float64(deltaCount)
	}
	deltaSum := endSum - startSum
	if deltaSum < 0 {
		deltaSum = endSum
	}
	return HistogramStats{
		Count:                deltaCount,
		Sum:                  deltaSum,
		Mean:                 deltaSum / float64(deltaCount),
		P50:                  interpolatePercentile(finiteBuckets, finiteCum, deltaCount, 0.50),
		P90:                  interpolatePercentile(finiteBuckets, finiteCum, deltaCount, 0.90),
		P95:                  interpolatePercentile(finiteBuckets, finiteCum, deltaCount, 0.95),
		P99:                  interpolatePercentile(finiteBuckets, finiteCum, deltaCount, 0.99),
		Max:                  inferMaxBound(finiteBuckets, finiteCum),
		BucketTruncationRate: trunc,
	}
}

// resolveDeltaBaseline returns the baseline sample count, sum, and cumulative
// bucket counts (keyed by upper bound) that the delta reduction subtracts
// from end. A nil startHistogram or a counter-reset (endCount < startCount,
// typically from a pod restart) yields a zero baseline so end is treated as
// the whole delta.
func resolveDeltaBaseline(startHistogram *dto.Histogram, endCount uint64) (uint64, float64, map[float64]uint64) {
	if startHistogram == nil {
		return 0, 0, nil
	}
	startCount := startHistogram.GetSampleCount()
	if endCount < startCount {
		return 0, 0, nil
	}
	buckets := startHistogram.GetBucket()
	cumBy := make(map[float64]uint64, len(buckets))
	for _, b := range buckets {
		cumBy[b.GetUpperBound()] = b.GetCumulativeCount()
	}
	return startCount, startHistogram.GetSampleSum(), cumBy
}

// inferMaxBound returns the tightest finite bucket upper bound that saw a
// non-zero delta. When every delta observation fell beyond the last finite
// bucket (the +Inf bucket), the last finite bound is returned; callers pair
// this with BucketTruncationRate to detect that condition.
func inferMaxBound(endBuckets []*dto.Bucket, deltaCum []uint64) float64 {
	if len(endBuckets) == 0 {
		return 0
	}
	for i := len(deltaCum) - 1; i >= 0; i-- {
		if deltaCum[i] > 0 {
			return endBuckets[i].GetUpperBound()
		}
	}
	return endBuckets[len(endBuckets)-1].GetUpperBound()
}

// interpolatePercentile returns the linearly-interpolated percentile from a
// cumulative delta distribution. Follows Prometheus' histogram_quantile
// convention: uniform-within-bucket, linear from the previous upper bound to
// the current upper bound. Percentiles landing beyond the last finite bucket
// return the last finite bound (a coarse under-estimate under truncation).
func interpolatePercentile(buckets []*dto.Bucket, cum []uint64, total uint64, q float64) float64 {
	if total == 0 || len(buckets) == 0 {
		return 0
	}
	target := q * float64(total)
	prevCum := uint64(0)
	prevUpper := 0.0
	for i, b := range buckets {
		c := cum[i]
		if float64(c) >= target {
			upper := b.GetUpperBound()
			bucketDelta := c - prevCum
			if bucketDelta == 0 {
				return upper
			}
			return prevUpper + (upper-prevUpper)*(target-float64(prevCum))/float64(bucketDelta)
		}
		prevCum = c
		prevUpper = b.GetUpperBound()
	}
	return prevUpper
}
