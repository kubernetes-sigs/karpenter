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

// LatencyHarness measures Karpenter's own histograms and counters across a test
// phase. Start scrapes /metrics from the active Karpenter pod and keeps the
// target series; Stop scrapes again and reports the difference, so earlier
// phases of the same suite do not leak into the numbers.
//
// Percentiles come from the per-bucket count delta, interpolated the way
// Prometheus histogram_quantile does it: uniform within a bucket, linear from
// the previous upper bound to the current one. Min and Max are bucket bounds
// rather than observations, so they answer which buckets samples landed in, not
// what the smallest sample was. A percentile landing past the last finite bucket
// is reported as that bound, and BucketTruncationRate says how much of the
// distribution the finite tail missed. Below minPercentileSamples observations
// the percentiles are still reported but flagged PercentilesUnreliable, because
// the sample count rather than the estimator is what makes them meaningless.
//
// A Karpenter restart zeroes every metric the process exports, so the start
// snapshot becomes a wrong baseline rather than a stale one and the window the
// deltas cover is no longer the window the spec asked for. Stop reads
// process_start_time_seconds and fails the spec when it moved, because a
// controller that died under load is the result, not an inconvenience to work
// around. reduceHistogramDelta's non-monotonic bucket check is the per-series
// fallback for a scrape carrying no process collector.
//
// TargetHistograms omits the metrics that time the KWOK fake provider rather
// than Karpenter. A provider running this harness against real infrastructure
// should add them back.

type HistogramStats struct {
	MetricName            string            `json:"metric_name"`
	Labels                map[string]string `json:"labels,omitempty"`
	Count                 uint64            `json:"count"`
	Sum                   float64           `json:"sum"`
	Mean                  float64           `json:"mean"`
	P50                   float64           `json:"p50"`
	P90                   float64           `json:"p90"`
	P95                   float64           `json:"p95"`
	P99                   float64           `json:"p99"`
	Min                   float64           `json:"min"`
	Max                   float64           `json:"max"`
	PercentilesUnreliable bool              `json:"percentiles_unreliable,omitempty"`
	BucketTruncationRate  float64           `json:"bucket_truncation_rate"`
}

const minPercentileSamples = 20

var TargetHistograms = []string{
	"karpenter_pods_scheduling_decision_duration_seconds",
	"karpenter_pods_bound_duration_seconds",
	"karpenter_pods_provisioning_bound_duration_seconds",
	"karpenter_pods_provisioning_startup_duration_seconds",
	"karpenter_scheduler_scheduling_duration_seconds",
	"karpenter_voluntary_disruption_decision_evaluation_duration_seconds",
	"karpenter_nodeclaims_termination_duration_seconds",
	"karpenter_consolidation_score",
}

var TargetCounters = []string{
	"karpenter_voluntary_disruption_consolidation_timeouts_total",
	"karpenter_consolidation_moves_total",
	"karpenter_nodeclaims_created_total",
	"karpenter_nodes_created_total",
}

type LatencyResult struct {
	LatencyStats map[string]HistogramStats
	Counters     map[string]uint64
}

const processStartTimeMetric = "process_start_time_seconds"

type LatencyHarness struct {
	env              *Environment
	podName          string
	start            map[string]*dto.MetricFamily
	startProcessTime float64
}

func StartLatencyHarness(env *Environment) (*LatencyHarness, error) {
	pod, err := env.EventuallyFindActiveKarpenterPod(env.Context)
	if err != nil {
		return nil, fmt.Errorf("finding karpenter pod: %w", err)
	}
	h := &LatencyHarness{env: env, podName: pod.Name}
	families, err := scrapeKarpenterMetricFamilies(env.Context, env, pod.Name)
	if err != nil {
		return nil, fmt.Errorf("initial scrape: %w", err)
	}
	h.start = compactFamilies(families)
	h.startProcessTime = getGaugeValue(families, processStartTimeMetric)
	GinkgoWriter.Printf("LatencyHarness: started, scraping pod kube-system/%s\n", pod.Name)
	return h, nil
}

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
	start, err := h.startSnapshot(end)
	if err != nil {
		return nil, err
	}
	res := &LatencyResult{
		LatencyStats: map[string]HistogramStats{},
		Counters:     map[string]uint64{},
	}
	for _, name := range TargetHistograms {
		for key, stats := range deltaHistogram(name, start[name], end[name]) {
			res.LatencyStats[key] = stats
		}
	}
	for _, name := range TargetCounters {
		for key, delta := range deltaCounter(name, start[name], end[name]) {
			res.Counters[key] = delta
		}
	}
	GinkgoWriter.Printf("LatencyHarness: stopped, %d histogram series, %d counter series\n",
		len(res.LatencyStats), len(res.Counters))
	return res, nil
}

func (h *LatencyHarness) startSnapshot(end map[string]*dto.MetricFamily) (map[string]*dto.MetricFamily, error) {
	endProcessTime := getGaugeValue(end, processStartTimeMetric)
	if h.startProcessTime != 0 && endProcessTime != 0 && endProcessTime != h.startProcessTime {
		return nil, fmt.Errorf("karpenter process restarted mid-measurement (process_start_time_seconds %.0f -> %.0f); the measured window is not the window under test",
			h.startProcessTime, endProcessTime)
	}
	return h.start, nil
}

func bucketCumByBound(h *dto.Histogram) map[float64]uint64 {
	buckets := h.GetBucket()
	out := make(map[float64]uint64, len(buckets))
	for _, b := range buckets {
		out[b.GetUpperBound()] = b.GetCumulativeCount()
	}
	return out
}

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

func compactFamilies(families map[string]*dto.MetricFamily) map[string]*dto.MetricFamily {
	keep := make(map[string]*dto.MetricFamily, len(TargetHistograms)+len(TargetCounters)+1)
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
	if f, ok := families[processStartTimeMetric]; ok {
		keep[processStartTimeMetric] = f
	}
	return keep
}

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
			delta = endV
		}
		out[key] = uint64(delta)
	}
	return out
}

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

func reduceHistogramDelta(end *dto.Histogram, startHistogram *dto.Histogram) HistogramStats {
	if end == nil {
		return HistogramStats{}
	}
	endCount := end.GetSampleCount()
	endSum := end.GetSampleSum()
	endBuckets := end.GetBucket()
	startCount, startSum, startCumBy := resolveDeltaBaseline(startHistogram, end)
	deltaCum, ok := cumulativeDelta(endBuckets, startCumBy)
	if !ok {
		startCount, startSum = 0, 0
		deltaCum, _ = cumulativeDelta(endBuckets, nil)
	}
	deltaCount := endCount - startCount
	if deltaCount == 0 {
		return HistogramStats{Count: 0, Sum: endSum - startSum}
	}
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
	return HistogramStats{
		Count:                 deltaCount,
		Sum:                   deltaSum,
		Mean:                  deltaSum / float64(deltaCount),
		P50:                   interpolatePercentile(finiteBuckets, finiteCum, deltaCount, 0.50),
		P90:                   interpolatePercentile(finiteBuckets, finiteCum, deltaCount, 0.90),
		P95:                   interpolatePercentile(finiteBuckets, finiteCum, deltaCount, 0.95),
		P99:                   interpolatePercentile(finiteBuckets, finiteCum, deltaCount, 0.99),
		Min:                   inferMinBound(finiteBuckets, finiteCum),
		Max:                   inferMaxBound(finiteBuckets, finiteCum),
		PercentilesUnreliable: deltaCount < minPercentileSamples,
		BucketTruncationRate:  trunc,
	}
}

func resolveDeltaBaseline(startHistogram, end *dto.Histogram) (uint64, float64, map[float64]uint64) {
	if startHistogram == nil {
		return 0, 0, nil
	}
	if end.GetSampleCount() < startHistogram.GetSampleCount() || end.GetSampleSum() < startHistogram.GetSampleSum() {
		return 0, 0, nil
	}
	return startHistogram.GetSampleCount(), startHistogram.GetSampleSum(), bucketCumByBound(startHistogram)
}

func cumulativeDelta(endBuckets []*dto.Bucket, startCumBy map[float64]uint64) ([]uint64, bool) {
	out := make([]uint64, len(endBuckets))
	prev := uint64(0)
	for i, b := range endBuckets {
		endCum := b.GetCumulativeCount()
		startCum := startCumBy[b.GetUpperBound()]
		if endCum < startCum {
			return out, false
		}
		out[i] = endCum - startCum
		if out[i] < prev {
			return out, false
		}
		prev = out[i]
	}
	return out, true
}

func inferMaxBound(endBuckets []*dto.Bucket, deltaCum []uint64) float64 {
	if len(endBuckets) == 0 {
		return 0
	}
	prevCum := uint64(0)
	maxIdx := -1
	for i, c := range deltaCum {
		if c > prevCum {
			maxIdx = i
		}
		prevCum = c
	}
	if maxIdx < 0 {
		return endBuckets[len(endBuckets)-1].GetUpperBound()
	}
	return endBuckets[maxIdx].GetUpperBound()
}

func inferMinBound(endBuckets []*dto.Bucket, deltaCum []uint64) float64 {
	prevUpper := 0.0
	for i, c := range deltaCum {
		if c > 0 {
			return prevUpper
		}
		prevUpper = endBuckets[i].GetUpperBound()
	}
	return prevUpper
}

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
