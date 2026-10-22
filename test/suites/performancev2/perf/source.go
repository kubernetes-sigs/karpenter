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
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const samplePeriod = 2 * time.Second

type ResourceStats struct {
	P95HeapMB   float64 `json:"p95_heap_mb"`
	MaxHeapMB   float64 `json:"max_heap_mb"`
	P95RSSMB    float64 `json:"p95_rss_mb"`
	AvgCPUCores float64 `json:"avg_cpu_cores"`
	P95CPUCores float64 `json:"p95_cpu_cores"`
	Samples     int     `json:"samples"`
}

type sample struct {
	heapMB, rssMB, cpuSeconds float64
	at                        time.Time
}

type source struct {
	p      *Provider
	pod    string
	commit string
	first  map[string]*dto.MetricFamily
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	samples []sample
	last    map[string]*dto.MetricFamily
	err     error
}

func startSource(ctx context.Context, p *Provider) *source {
	GinkgoHelper()
	pod, families := eventuallyLeader(ctx, p, 2*time.Minute)
	ctx, cancel := context.WithCancel(ctx)
	s := &source{p: p, pod: pod, commit: buildCommit(families), first: families, last: families, cancel: cancel, done: make(chan struct{})}
	s.record(families)
	go s.run(ctx)
	return s
}

func (s *source) run(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(samplePeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if f, err := scrape(ctx, s.p, s.pod); err == nil {
				s.record(f)
			} else if ctx.Err() == nil {
				s.mu.Lock()
				s.err = err
				s.mu.Unlock()
			}
		}
	}
}

func (s *source) record(f map[string]*dto.MetricFamily) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = f
	s.samples = append(s.samples, sample{
		heapMB:     gauge(f, "go_gc_heap_live_bytes") / (1 << 20),
		rssMB:      gauge(f, "process_resident_memory_bytes") / (1 << 20),
		cpuSeconds: counter(f, "process_cpu_seconds_total"),
		at:         time.Now(),
	})
}

func (s *source) stop(ctx context.Context) (ResourceStats, map[string]HistogramStats, map[string]uint64, error) {
	s.cancel()
	<-s.done
	f, err := scrape(ctx, s.p, s.pod)
	if err != nil {
		return ResourceStats{}, nil, nil, fmt.Errorf("final scrape of %s: %w (controller restarted?)", s.pod, err)
	}
	s.record(f)
	if a, b := gauge(s.first, "process_start_time_seconds"), gauge(f, "process_start_time_seconds"); a != b {
		return ResourceStats{}, nil, nil, fmt.Errorf("controller %s restarted mid-phase (process_start_time_seconds %.0f -> %.0f)", s.pod, a, b)
	}
	hists, counters := deltas(s.first, f)
	return s.stats(), hists, counters, nil
}

func deltas(first, last map[string]*dto.MetricFamily) (map[string]HistogramStats, map[string]uint64) {
	hists, counters := map[string]HistogramStats{}, map[string]uint64{}
	for name, mf := range last {
		karpenter := strings.HasPrefix(name, "karpenter_")
		switch {
		case mf.GetType() == dto.MetricType_HISTOGRAM && karpenter:
			for k, v := range deltaHistogram(name, first[name], mf) {
				if v.Count > 0 {
					hists[k] = v
				}
			}
		case mf.GetType() == dto.MetricType_COUNTER && (karpenter || name == "controller_runtime_reconcile_total"):
			for k, v := range deltaCounter(name, first[name], mf) {
				if v > 0 {
					counters[k] = v
				}
			}
		}
	}
	return hists, counters
}

func (s *source) stats() ResourceStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var heap, rss, cpu []float64
	for i, x := range s.samples {
		heap, rss = append(heap, x.heapMB), append(rss, x.rssMB)
		if i > 0 {
			if dt := x.at.Sub(s.samples[i-1].at).Seconds(); dt > 0 {
				cpu = append(cpu, (x.cpuSeconds-s.samples[i-1].cpuSeconds)/dt)
			}
		}
	}
	r := ResourceStats{P95HeapMB: p95(heap), MaxHeapMB: maxOf(heap), P95RSSMB: p95(rss), P95CPUCores: p95(cpu), Samples: len(s.samples)}
	if n := len(s.samples); n > 1 {
		r.AvgCPUCores = (s.samples[n-1].cpuSeconds - s.samples[0].cpuSeconds) / s.samples[n-1].at.Sub(s.samples[0].at).Seconds()
	}
	return r
}

func leader(ctx context.Context, p *Provider) (string, map[string]*dto.MetricFamily, error) {
	pods := &corev1.PodList{}
	if err := p.Client.List(ctx, pods, client.InNamespace(p.Controller.Namespace), client.MatchingLabelsSelector{Selector: p.Controller.Selector}); err != nil {
		return "", nil, err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			continue
		}
		f, err := scrape(ctx, p, pod.Name)
		if err == nil && gauge(f, "leader_election_master_status") == 1 {
			return pod.Name, f, nil
		}
	}
	return "", nil, fmt.Errorf("no leading controller pod in %s matching %s", p.Controller.Namespace, p.Controller.Selector)
}

func eventuallyLeader(ctx context.Context, p *Provider, timeout time.Duration) (string, map[string]*dto.MetricFamily) {
	GinkgoHelper()
	deadline := time.Now().Add(timeout)
	for {
		pod, f, err := leader(ctx, p)
		if err == nil {
			return pod, f
		}
		if time.Now().After(deadline) {
			Fail(err.Error())
		}
		time.Sleep(2 * time.Second)
	}
}

func scrape(ctx context.Context, p *Provider, pod string) (map[string]*dto.MetricFamily, error) {
	data, err := p.KubeClient.CoreV1().Pods(p.Controller.Namespace).ProxyGet("http", pod, p.Controller.MetricsPort, "/metrics", nil).DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	return parser.TextToMetricFamilies(bytes.NewReader(data))
}

func buildCommit(f map[string]*dto.MetricFamily) string {
	if c := os.Getenv("KARPENTER_PERF_COMMIT"); c != "" {
		return c
	}
	for _, m := range f["karpenter_build_info"].GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "commit" {
				return l.GetValue()
			}
		}
	}
	return ""
}

func gauge(f map[string]*dto.MetricFamily, name string) float64 {
	if mf, ok := f[name]; ok && len(mf.GetMetric()) > 0 {
		return mf.GetMetric()[0].GetGauge().GetValue()
	}
	return 0
}

func counter(f map[string]*dto.MetricFamily, name string) float64 {
	if mf, ok := f[name]; ok && len(mf.GetMetric()) > 0 {
		return mf.GetMetric()[0].GetCounter().GetValue()
	}
	return 0
}

func p95(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[int(math.Ceil(0.95*float64(len(s))))-1]
}

func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		m = max(m, x)
	}
	return m
}
