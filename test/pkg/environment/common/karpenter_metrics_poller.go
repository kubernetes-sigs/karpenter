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
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ResourceSample represents a single point-in-time measurement of container resource usage
type ResourceSample struct {
	Timestamp time.Time
	MemoryMB  float64 // container working set memory in MB
	CPUCores  float64 // container CPU usage rate in cores
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

// prometheusQueryResponse represents the JSON response from Prometheus instant query API
type prometheusQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value []json.RawMessage `json:"value"` // [timestamp, "value"]
		} `json:"result"`
	} `json:"data"`
}

// KarpenterMetricsPoller polls Prometheus for real container-level CPU and memory
// usage of the active Karpenter pod via cAdvisor metrics. It collects a time series
// of samples that can be aggregated into P95, average, and max statistics.
//
// This uses the Prometheus instance already deployed in the cluster (via
// kube-prometheus-stack) which scrapes cAdvisor metrics from the kubelet.
// No metrics-server installation is required.
type KarpenterMetricsPoller struct {
	env     *Environment
	mu      sync.Mutex
	samples []ResourceSample
	cancel  context.CancelFunc
	done    chan struct{}
	errors  int
}

// StartKarpenterMetricsPoller begins polling Prometheus for the Karpenter pod's
// container resource usage every 5 seconds.
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
		GinkgoWriter.Printf("KarpenterMetricsPoller: WARNING - stopped with 0 samples (%d errors). Karpenter resource metrics will be empty. Ensure Prometheus is installed and scraping cAdvisor metrics.\n", mp.errors)
	} else {
		GinkgoWriter.Printf("KarpenterMetricsPoller: Stopped with %d samples (%d errors), P95 memory=%.2f MB, P95 CPU=%.4f cores\n",
			len(mp.samples), mp.errors, stats.P95MemoryMB, stats.P95CPUCores)
	}
	return stats
}

func (mp *KarpenterMetricsPoller) run(ctx context.Context) {
	defer close(mp.done)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		mp.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (mp *KarpenterMetricsPoller) poll(ctx context.Context) {
	defer GinkgoRecover()

	sample, err := mp.collectSample(ctx)
	if err != nil {
		mp.recordError(err)
		return
	}

	mp.mu.Lock()
	mp.samples = append(mp.samples, sample)
	if len(mp.samples) == 1 {
		GinkgoWriter.Printf("KarpenterMetricsPoller: first sample collected - memory=%.2f MB, cpu=%.4f cores\n", sample.MemoryMB, sample.CPUCores)
	}
	mp.mu.Unlock()
}

func (mp *KarpenterMetricsPoller) collectSample(ctx context.Context) (ResourceSample, error) {
	pod, err := mp.env.FindActiveKarpenterPod(ctx)
	if err != nil || pod == nil {
		return ResourceSample{}, fmt.Errorf("finding karpenter pod: %w", err)
	}

	promPod := mp.findPrometheusPod(ctx)
	if promPod == nil {
		return ResourceSample{}, fmt.Errorf("could not find Prometheus pod in monitoring namespace")
	}

	localPort := rand.IntnRange(10000, 49151)
	portCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := mp.env.PortForwardPod(portCtx, promPod, 9090, localPort); err != nil {
		return ResourceSample{}, fmt.Errorf("port-forward to Prometheus: %w", err)
	}

	memBytes := mp.queryPrometheusValue(localPort,
		fmt.Sprintf(`sum(container_memory_working_set_bytes{namespace="kube-system",pod="%s",container!=""})`, pod.Name))
	cpuCores := mp.queryPrometheusValue(localPort,
		fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{namespace="kube-system",pod="%s",container!=""}[30s]))`, pod.Name))

	if memBytes == 0 && cpuCores == 0 {
		return ResourceSample{}, fmt.Errorf("got zero values from Prometheus for pod %s", pod.Name)
	}

	return ResourceSample{
		Timestamp: time.Now(),
		MemoryMB:  memBytes / (1024 * 1024),
		CPUCores:  cpuCores,
	}, nil
}

func (mp *KarpenterMetricsPoller) recordError(err error) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.errors++
	if mp.errors <= 3 {
		GinkgoWriter.Printf("KarpenterMetricsPoller: %v\n", err)
	}
}

func (mp *KarpenterMetricsPoller) findPrometheusPod(ctx context.Context) *corev1.Pod {
	podList := &corev1.PodList{}
	if err := mp.env.Client.List(ctx, podList,
		client.InNamespace("monitoring"),
		client.MatchingLabels{"app.kubernetes.io/name": "prometheus"},
	); err != nil || len(podList.Items) == 0 {
		return nil
	}
	for i := range podList.Items {
		if podList.Items[i].Status.Phase == corev1.PodRunning {
			return &podList.Items[i]
		}
	}
	return nil
}
func (mp *KarpenterMetricsPoller) queryPrometheusValue(port int, query string) float64 {
	u := fmt.Sprintf("http://127.0.0.1:%d/api/v1/query?query=%s", port, url.QueryEscape(query))
	resp, err := http.Get(u) //nolint:gosec // URL is constructed from trusted local port-forward, not user input
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0
	}

	var result prometheusQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0
	}

	if result.Status != "success" || len(result.Data.Result) == 0 {
		return 0
	}

	// The value is the second element: [timestamp, "value_string"]
	if len(result.Data.Result[0].Value) < 2 {
		return 0
	}

	var valueStr string
	if err := json.Unmarshal(result.Data.Result[0].Value[1], &valueStr); err != nil {
		return 0
	}

	return parseFloat(valueStr)
}

// computeStats calculates P95, average, and max from a slice of resource samples.
func computeStats(samples []ResourceSample) ResourceStats {
	if len(samples) == 0 {
		return ResourceStats{}
	}

	memValues := make([]float64, len(samples))
	cpuValues := make([]float64, len(samples))
	var memSum, cpuSum float64
	var memMax, cpuMax float64

	for i, s := range samples {
		memValues[i] = s.MemoryMB
		cpuValues[i] = s.CPUCores
		memSum += s.MemoryMB
		cpuSum += s.CPUCores
		memMax = math.Max(memMax, s.MemoryMB)
		cpuMax = math.Max(cpuMax, s.CPUCores)
	}

	n := float64(len(samples))
	return ResourceStats{
		P95MemoryMB: percentile(memValues, 0.95),
		AvgMemoryMB: memSum / n,
		MaxMemoryMB: memMax,
		P95CPUCores: percentile(cpuValues, 0.95),
		AvgCPUCores: cpuSum / n,
		MaxCPUCores: cpuMax,
		SampleCount: len(samples),
	}
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

func parseFloat(s string) float64 {
	var v float64
	_, _ = fmt.Sscanf(s, "%f", &v)
	return v
}
