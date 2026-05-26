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
	"bufio"
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"k8s.io/apimachinery/pkg/util/rand"
)

// ResourceSample represents a single point-in-time measurement of container resource usage
type ResourceSample struct {
	Timestamp time.Time
	MemoryMB  float64 // process resident memory in MB
	CPUCores  float64 // CPU usage rate in cores (computed from delta)
}

// ResourceStats holds aggregated statistics computed from a time series of samples
type ResourceStats struct {
	P95MemoryMB float64 // 95th percentile memory usage in MB
	AvgMemoryMB float64 // average memory usage in MB
	MaxMemoryMB float64 // peak memory usage in MB
	P95CPUCores float64 // 95th percentile CPU usage in cores
	AvgCPUCores float64 // average CPU usage in cores
	MaxCPUCores float64 // peak CPU usage in cores
	SampleCount int     // number of samples collected
}

// KarpenterMetricsPoller polls the Karpenter pod's /metrics endpoint directly
// for process-level CPU and memory usage. It uses a single long-lived port-forward
// and computes CPU rate from the delta of process_cpu_seconds_total between samples.
//
// This approach is more reliable than querying Prometheus because:
// - No dependency on Prometheus being installed or having scraped data
// - No scrape lag or rate() window issues
// - Single port-forward reused across all samples (no per-sample connection churn)
// - Metrics are authoritative (reported by the Go runtime itself)
type KarpenterMetricsPoller struct {
	env     *Environment
	mu      sync.Mutex
	samples []ResourceSample
	cancel  context.CancelFunc
	done    chan struct{}
	errors  int
}

// StartKarpenterMetricsPoller begins polling the Karpenter pod's /metrics endpoint
// for process resource usage every 5 seconds.
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

// Stop stops the poller and returns aggregated resource statistics.
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
		GinkgoWriter.Printf("KarpenterMetricsPoller:   CPU    - P95: %.4f cores, Avg: %.4f cores, Max: %.4f cores\n",
			stats.P95CPUCores, stats.AvgCPUCores, stats.MaxCPUCores)
	}
	return stats
}

func (mp *KarpenterMetricsPoller) run(ctx context.Context) {
	defer close(mp.done)

	localPort := rand.IntnRange(10000, 49151)
	portForwardCtx, portForwardCancel := context.WithCancel(ctx)
	defer portForwardCancel()

	GinkgoWriter.Printf("KarpenterMetricsPoller: starting, establishing port-forward on local port %d\n", localPort)

	// Establish a single long-lived port-forward to the Karpenter pod
	if err := mp.establishPortForward(portForwardCtx, localPort); err != nil {
		mp.recordError(fmt.Errorf("initial port-forward failed: %w", err))
		return
	}
	GinkgoWriter.Printf("KarpenterMetricsPoller: port-forward established successfully on port %d\n", localPort)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var prevCPUSeconds float64
	var prevTime time.Time
	firstSample := true
	sampleNum := 0

	for {
		now := time.Now()
		memBytes, cpuSeconds, err := mp.scrapeMetrics(localPort)
		if err != nil {
			mp.recordError(err)
			// Try to re-establish port-forward on failure
			GinkgoWriter.Printf("KarpenterMetricsPoller: scrape failed, attempting port-forward reconnect\n")
			portForwardCancel()
			localPort = rand.IntnRange(10000, 49151)
			portForwardCtx, portForwardCancel = context.WithCancel(ctx)
			if pfErr := mp.establishPortForward(portForwardCtx, localPort); pfErr != nil {
				mp.recordError(fmt.Errorf("port-forward re-establish failed: %w", pfErr))
				GinkgoWriter.Printf("KarpenterMetricsPoller: reconnect failed, giving up: %v\n", pfErr)
				portForwardCancel()
				return
			}
			GinkgoWriter.Printf("KarpenterMetricsPoller: reconnected successfully on port %d\n", localPort)
		} else if firstSample {
			// First sample: we can record memory but not CPU rate (need two points)
			prevCPUSeconds = cpuSeconds
			prevTime = now
			firstSample = false
			sampleNum++
			mp.mu.Lock()
			mp.samples = append(mp.samples, ResourceSample{
				Timestamp: now,
				MemoryMB:  memBytes / (1024 * 1024),
				CPUCores:  0,
			})
			mp.mu.Unlock()
			GinkgoWriter.Printf("KarpenterMetricsPoller: [sample %d] first sample - memory=%.2f MB, process_cpu_seconds_total=%.4f (CPU rate available after next sample)\n",
				sampleNum, memBytes/(1024*1024), cpuSeconds)
		} else {
			elapsed := now.Sub(prevTime).Seconds()
			cpuRate := 0.0
			if elapsed > 0 {
				cpuRate = (cpuSeconds - prevCPUSeconds) / elapsed
			}
			// Clamp negative rates (can happen if counter resets, e.g. pod restart)
			if cpuRate < 0 {
				cpuRate = 0
			}
			prevCPUSeconds = cpuSeconds
			prevTime = now
			sampleNum++

			mp.mu.Lock()
			mp.samples = append(mp.samples, ResourceSample{
				Timestamp: now,
				MemoryMB:  memBytes / (1024 * 1024),
				CPUCores:  cpuRate,
			})
			mp.mu.Unlock()

			// Log every 5th sample to avoid flooding, but always log first few
			if sampleNum <= 5 || sampleNum%5 == 0 {
				GinkgoWriter.Printf("KarpenterMetricsPoller: [sample %d] memory=%.2f MB, cpu=%.4f cores (elapsed=%.1fs, cpu_delta=%.4fs)\n",
					sampleNum, memBytes/(1024*1024), cpuRate, elapsed, cpuSeconds-prevCPUSeconds+cpuRate*elapsed)
			}
		}

		select {
		case <-ctx.Done():
			GinkgoWriter.Printf("KarpenterMetricsPoller: context canceled, collected %d samples total\n", sampleNum)
			portForwardCancel()
			return
		case <-ticker.C:
		}
	}
}

func (mp *KarpenterMetricsPoller) establishPortForward(ctx context.Context, localPort int) error {
	findCtx, findCancel := context.WithTimeout(ctx, 10*time.Second)
	defer findCancel()

	pod, err := mp.env.FindActiveKarpenterPod(findCtx)
	if err != nil || pod == nil {
		return fmt.Errorf("finding karpenter pod: %w", err)
	}
	GinkgoWriter.Printf("KarpenterMetricsPoller: found karpenter pod %s/%s, establishing port-forward %d->8080\n",
		pod.Namespace, pod.Name, localPort)

	// Pass the long-lived ctx so the port-forward stays alive until ctx is canceled.
	// PortForwardPod blocks until the connection is ready or fails.
	if err := mp.env.PortForwardPod(ctx, pod, 8080, localPort); err != nil {
		return fmt.Errorf("port-forward to karpenter pod %s: %w", pod.Name, err)
	}

	// Verify connectivity with a quick scrape
	memBytes, cpuSeconds, err := mp.scrapeMetrics(localPort)
	if err != nil {
		return fmt.Errorf("connectivity check failed after port-forward: %w", err)
	}
	GinkgoWriter.Printf("KarpenterMetricsPoller: connectivity verified - process_resident_memory_bytes=%.0f, process_cpu_seconds_total=%.4f\n",
		memBytes, cpuSeconds)
	return nil
}

// scrapeMetrics fetches /metrics from the given local port and extracts
// process_resident_memory_bytes and process_cpu_seconds_total.
func (mp *KarpenterMetricsPoller) scrapeMetrics(port int) (memBytes float64, cpuSeconds float64, err error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port)) //nolint:gosec
	if err != nil {
		return 0, 0, fmt.Errorf("GET /metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("GET /metrics returned status %d", resp.StatusCode)
	}

	var foundMem, foundCPU bool
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "process_resident_memory_bytes ") {
			if v, e := strconv.ParseFloat(strings.TrimPrefix(line, "process_resident_memory_bytes "), 64); e == nil {
				memBytes = v
				foundMem = true
			}
		} else if strings.HasPrefix(line, "process_cpu_seconds_total ") {
			if v, e := strconv.ParseFloat(strings.TrimPrefix(line, "process_cpu_seconds_total "), 64); e == nil {
				cpuSeconds = v
				foundCPU = true
			}
		}
		if foundMem && foundCPU {
			break
		}
	}

	if !foundMem || !foundCPU {
		return 0, 0, fmt.Errorf("metrics not found in response (mem=%v, cpu=%v)", foundMem, foundCPU)
	}
	return memBytes, cpuSeconds, nil
}

func (mp *KarpenterMetricsPoller) recordError(err error) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.errors++
	if mp.errors <= 5 {
		GinkgoWriter.Printf("KarpenterMetricsPoller: error %d: %v\n", mp.errors, err)
	}
}

// computeStats calculates P95, average, and max from a slice of resource samples.
// The first sample's CPU value (always 0) is excluded from CPU statistics.
func computeStats(samples []ResourceSample) ResourceStats {
	if len(samples) == 0 {
		return ResourceStats{}
	}

	memValues := make([]float64, len(samples))
	var memSum, memMax float64

	for i, s := range samples {
		memValues[i] = s.MemoryMB
		memSum += s.MemoryMB
		memMax = math.Max(memMax, s.MemoryMB)
	}

	// For CPU, skip the first sample (which has CPUCores=0 since we need two points for a rate)
	var cpuValues []float64
	var cpuSum, cpuMax float64
	for _, s := range samples {
		if s.CPUCores > 0 || len(cpuValues) > 0 {
			cpuValues = append(cpuValues, s.CPUCores)
			cpuSum += s.CPUCores
			cpuMax = math.Max(cpuMax, s.CPUCores)
		}
	}

	n := float64(len(samples))
	stats := ResourceStats{
		P95MemoryMB: percentile(memValues, 0.95),
		AvgMemoryMB: memSum / n,
		MaxMemoryMB: memMax,
		SampleCount: len(samples),
	}

	if len(cpuValues) > 0 {
		stats.P95CPUCores = percentile(cpuValues, 0.95)
		stats.AvgCPUCores = cpuSum / float64(len(cpuValues))
		stats.MaxCPUCores = cpuMax
	}

	return stats
}

// percentile returns the p-th percentile of a slice of float64 values using
// linear interpolation. The input slice is sorted in place.
func percentile(values []float64, p float64) float64 {
	sort.Float64s(values)
	if len(values) == 0 {
		return 0
	}
	if len(values) == 1 {
		return values[0]
	}
	rank := p * float64(len(values)-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))
	if lower == upper {
		return values[lower]
	}
	frac := rank - float64(lower)
	return values[lower]*(1-frac) + values[upper]*frac
}
