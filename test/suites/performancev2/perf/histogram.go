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

package perf

import (
	"math"
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
)

type HistogramStats struct {
	MetricName           string            `json:"metric_name"`
	Labels               map[string]string `json:"labels,omitempty"`
	Count                uint64            `json:"count"`
	Sum                  float64           `json:"sum"`
	Mean                 float64           `json:"mean"`
	Min                  float64           `json:"min"`
	Max                  float64           `json:"max"`
	BucketTruncationRate float64           `json:"bucket_truncation_rate"`
}

func bucketCumByBound(h *dto.Histogram) map[float64]uint64 {
	buckets := h.GetBucket()
	out := make(map[float64]uint64, len(buckets))
	for _, b := range buckets {
		out[b.GetUpperBound()] = b.GetCumulativeCount()
	}
	return out
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
		Count:                deltaCount,
		Sum:                  deltaSum,
		Mean:                 deltaSum / float64(deltaCount),
		Min:                  inferMinBound(finiteBuckets, finiteCum),
		Max:                  inferMaxBound(finiteBuckets, finiteCum),
		BucketTruncationRate: trunc,
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
