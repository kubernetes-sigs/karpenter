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
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/montanaflynn/stats"
	. "github.com/onsi/ginkgo/v2"
	dto "github.com/prometheus/client_model/go"
)

type ResourceSample struct {
	Timestamp time.Time
	MemoryMB  float64 // process resident memory in MB
	HeapMB    float64 // Go live heap (marked live by the last GC) in MB
	CPUCores  float64 // CPU usage rate in cores (computed from delta)
}

type ResourceStats struct {
	P95MemoryMB float64 // 95th percentile memory usage in MB
	AvgMemoryMB float64 // average memory usage in MB
	MaxMemoryMB float64 // peak memory usage in MB
	P95HeapMB   float64 // 95th percentile Go live heap in MB
	AvgHeapMB   float64 // average Go live heap in MB
	MaxHeapMB   float64 // peak Go live heap in MB
	P95CPUCores float64 // 95th percentile CPU usage in cores
	AvgCPUCores float64 // average CPU usage in cores
	MaxCPUCores float64 // peak CPU usage in cores
	SampleCount int     // number of samples collected
}

// KarpenterMetricsPoller polls the Karpenter pod's /metrics endpoint via the
// API server pod proxy for process-level CPU and memory usage and Go live
// heap. It computes CPU rate from the delta of process_cpu_seconds_total
// between samples.
type KarpenterMetricsPoller struct {
	env     *Environment
	mu      sync.Mutex
	samples []ResourceSample
	cancel  context.CancelFunc
	done    chan struct{}
	errors  int
}

func StartKarpenterMetricsPoller(env *Environment) *KarpenterMetricsPoller {
	ctx, cancel := context.WithCancel(env.Context)
	mp := &KarpenterMetricsPoller{
		env:    env,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go mp.run(ctx)
	return mp
}

func (mp *KarpenterMetricsPoller) Stop() ResourceStats {
	mp.cancel()
	<-mp.done
	mp.mu.Lock()
	defer mp.mu.Unlock()
	stats := computeStats(mp.samples)
	if len(mp.samples) == 0 {
		GinkgoWriter.Printf("KarpenterMetricsPoller: WARNING - stopped with 0 samples (%d errors). Ensure the Karpenter pod is running and exposing /metrics on port 8080.\n", mp.errors)
	} else {
		GinkgoWriter.Printf("KarpenterMetricsPoller: === RESULTS ===\n")
		GinkgoWriter.Printf("KarpenterMetricsPoller:   Samples: %d (errors: %d)\n", len(mp.samples), mp.errors)
		GinkgoWriter.Printf("KarpenterMetricsPoller:   Memory - P95: %.2f MB, Avg: %.2f MB, Max: %.2f MB\n",
			stats.P95MemoryMB, stats.AvgMemoryMB, stats.MaxMemoryMB)
		GinkgoWriter.Printf("KarpenterMetricsPoller:   Heap   - P95: %.2f MB, Avg: %.2f MB, Max: %.2f MB\n",
			stats.P95HeapMB, stats.AvgHeapMB, stats.MaxHeapMB)
		GinkgoWriter.Printf("KarpenterMetricsPoller:   CPU    - P95: %.4f cores, Avg: %.4f cores, Max: %.4f cores\n",
			stats.P95CPUCores, stats.AvgCPUCores, stats.MaxCPUCores)
	}
	return stats
}

type pollerState struct {
	podName        string
	prevCPUSeconds float64
	prevTime       time.Time
	firstSample    bool
	sampleNum      int
}

func (mp *KarpenterMetricsPoller) run(ctx context.Context) {
	defer close(mp.done)

	state := &pollerState{firstSample: true}

	pod, err := mp.env.EventuallyFindActiveKarpenterPod(ctx)
	if err != nil || pod == nil {
		mp.recordError(fmt.Errorf("finding karpenter pod: %w", err))
		return
	}
	state.podName = pod.Name
	GinkgoWriter.Printf("KarpenterMetricsPoller: starting, scraping pod %s/%s via API server proxy\n", pod.Namespace, pod.Name)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		mp.pollOnce(ctx, state)
		select {
		case <-ctx.Done():
			GinkgoWriter.Printf("KarpenterMetricsPoller: context canceled, collected %d samples total\n", state.sampleNum)
			return
		case <-ticker.C:
		}
	}
}

func (mp *KarpenterMetricsPoller) pollOnce(ctx context.Context, state *pollerState) {
	now := time.Now()
	m, err := mp.scrapeMetrics(ctx, state.podName)
	if err != nil {
		var notFound *metricsNotFoundError
		if !errors.As(err, &notFound) {
			// Pod may have been replaced (e.g. leader election change). Try to find the new one.
			if pod, findErr := mp.env.FindActiveKarpenterPod(ctx); findErr == nil && pod != nil && pod.Name != state.podName {
				GinkgoWriter.Printf("KarpenterMetricsPoller: active pod changed from %s to %s\n", state.podName, pod.Name)
				state.podName = pod.Name
				state.firstSample = true
			}
		}
		mp.recordError(err)
		return
	}

	if state.firstSample {
		mp.recordFirstSample(state, now, m)
	} else {
		mp.recordSample(state, now, m)
	}
}

// metricsNotFoundError indicates the HTTP response was missing one or more
// expected metrics, e.g. because the pod was temporarily overloaded.
type metricsNotFoundError struct {
	foundMem  bool
	foundHeap bool
	foundCPU  bool
}

func (e *metricsNotFoundError) Error() string {
	return fmt.Sprintf("metrics not found in response (mem=%v, heap=%v, cpu=%v)", e.foundMem, e.foundHeap, e.foundCPU)
}

// processMetrics is one scrape of the Karpenter process's resource metrics.
type processMetrics struct {
	residentBytes float64 // process_resident_memory_bytes
	heapLiveBytes float64 // go_gc_heap_live_bytes
	cpuSeconds    float64 // process_cpu_seconds_total
}

func (mp *KarpenterMetricsPoller) recordFirstSample(state *pollerState, now time.Time, m processMetrics) {
	state.prevCPUSeconds = m.cpuSeconds
	state.prevTime = now
	state.firstSample = false
	state.sampleNum++
	mp.mu.Lock()
	mp.samples = append(mp.samples, ResourceSample{
		Timestamp: now,
		MemoryMB:  m.residentBytes / (1024 * 1024),
		HeapMB:    m.heapLiveBytes / (1024 * 1024),
		CPUCores:  0,
	})
	mp.mu.Unlock()
	GinkgoWriter.Printf("KarpenterMetricsPoller: [sample %d] first sample - memory=%.2f MB, heap=%.2f MB, process_cpu_seconds_total=%.4f (CPU rate available after next sample)\n",
		state.sampleNum, m.residentBytes/(1024*1024), m.heapLiveBytes/(1024*1024), m.cpuSeconds)
}

func (mp *KarpenterMetricsPoller) recordSample(state *pollerState, now time.Time, m processMetrics) {
	elapsed := now.Sub(state.prevTime).Seconds()
	cpuRate := 0.0
	if elapsed > 0 {
		cpuRate = (m.cpuSeconds - state.prevCPUSeconds) / elapsed
	}

	// Negative rate means counter reset (pod restart). Reset baseline and skip this sample.
	if cpuRate < 0 {
		GinkgoWriter.Printf("KarpenterMetricsPoller: CPU counter reset detected (delta=%.4f), resetting baseline\n",
			m.cpuSeconds-state.prevCPUSeconds)
		state.prevCPUSeconds = m.cpuSeconds
		state.prevTime = now
		return
	}

	state.prevCPUSeconds = m.cpuSeconds
	state.prevTime = now
	state.sampleNum++

	mp.mu.Lock()
	mp.samples = append(mp.samples, ResourceSample{
		Timestamp: now,
		MemoryMB:  m.residentBytes / (1024 * 1024),
		HeapMB:    m.heapLiveBytes / (1024 * 1024),
		CPUCores:  cpuRate,
	})
	mp.mu.Unlock()

	if state.sampleNum <= 5 || state.sampleNum%5 == 0 {
		GinkgoWriter.Printf("KarpenterMetricsPoller: [sample %d] memory=%.2f MB, heap=%.2f MB, cpu=%.4f cores (elapsed=%.1fs)\n",
			state.sampleNum, m.residentBytes/(1024*1024), m.heapLiveBytes/(1024*1024), cpuRate, elapsed)
	}
}

// scrapeMetrics uses the API server pod proxy to fetch /metrics from the Karpenter pod.
func (mp *KarpenterMetricsPoller) scrapeMetrics(ctx context.Context, podName string) (processMetrics, error) {
	families, err := scrapeKarpenterMetricFamilies(ctx, mp.env, podName)
	if err != nil {
		return processMetrics{}, err
	}
	return parseProcessMetrics(families)
}

// parseProcessMetrics extracts the resource metrics from a parsed Prometheus
// exposition. Every metric must be present: a sample missing heap would
// silently lower the heap statistics that the performance tests gate on.
func parseProcessMetrics(families map[string]*dto.MetricFamily) (processMetrics, error) {
	m := processMetrics{
		residentBytes: getGaugeValue(families, "process_resident_memory_bytes"),
		heapLiveBytes: getGaugeValue(families, "go_gc_heap_live_bytes"),
		cpuSeconds:    getCounterValue(families, "process_cpu_seconds_total"),
	}
	if m.residentBytes == 0 || m.heapLiveBytes == 0 || m.cpuSeconds == 0 {
		return processMetrics{}, &metricsNotFoundError{foundMem: m.residentBytes != 0, foundHeap: m.heapLiveBytes != 0, foundCPU: m.cpuSeconds != 0}
	}
	return m, nil
}

func getGaugeValue(families map[string]*dto.MetricFamily, name string) float64 {
	if mf, ok := families[name]; ok && len(mf.GetMetric()) > 0 {
		return mf.GetMetric()[0].GetGauge().GetValue()
	}
	return 0
}

func getCounterValue(families map[string]*dto.MetricFamily, name string) float64 {
	if mf, ok := families[name]; ok && len(mf.GetMetric()) > 0 {
		return mf.GetMetric()[0].GetCounter().GetValue()
	}
	return 0
}

func (mp *KarpenterMetricsPoller) recordError(err error) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.errors++
	if mp.errors <= 5 {
		GinkgoWriter.Printf("KarpenterMetricsPoller: error %d: %v\n", mp.errors, err)
	}
}

func computeStats(samples []ResourceSample) ResourceStats {
	if len(samples) == 0 {
		return ResourceStats{}
	}

	memValues := make(stats.Float64Data, len(samples))
	heapValues := make(stats.Float64Data, len(samples))
	for i, s := range samples {
		memValues[i] = s.MemoryMB
		heapValues[i] = s.HeapMB
	}

	// Skip the first sample for CPU (always 0 since rate needs two points)
	var cpuValues stats.Float64Data
	if len(samples) > 1 {
		cpuValues = make(stats.Float64Data, len(samples)-1)
		for i, s := range samples[1:] {
			cpuValues[i] = s.CPUCores
		}
	}

	memP95, _ := stats.Percentile(memValues, 95)
	memAvg, _ := stats.Mean(memValues)
	memMax, _ := stats.Max(memValues)
	heapP95, _ := stats.Percentile(heapValues, 95)
	heapAvg, _ := stats.Mean(heapValues)
	heapMax, _ := stats.Max(heapValues)

	result := ResourceStats{
		P95MemoryMB: memP95,
		AvgMemoryMB: memAvg,
		MaxMemoryMB: memMax,
		P95HeapMB:   heapP95,
		AvgHeapMB:   heapAvg,
		MaxHeapMB:   heapMax,
		SampleCount: len(samples),
	}

	if len(cpuValues) > 0 {
		result.P95CPUCores, _ = stats.Percentile(cpuValues, 95)
		result.AvgCPUCores, _ = stats.Mean(cpuValues)
		result.MaxCPUCores, _ = stats.Max(cpuValues)
	}

	return result
}
