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
	"os"
	"path/filepath"
	"sync"

	"github.com/google/pprof/profile"
	. "github.com/onsi/ginkgo/v2"
)

const cpuProfileSeconds = 20

// profiler takes pprof profiles through the API server proxy: a heap profile after the phase's last metrics
// scrape, and with KARPENTER_PERF_CPU_PROFILE=true the busiest of back-to-back CPU profiles during the phase.
// CPU profiling raised cpu_cores 15-21% on kind, so it is opt-in and its runs shouldn't be compared to
// unprofiled bounds. Both need the controller's ENABLE_PROFILING; without it the endpoint 404s and nothing is saved.
type profiler struct {
	p      *Provider
	pod    string
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	cpu     []byte
	cpuPeak int64
}

func startProfiler(ctx context.Context, p *Provider, pod string) *profiler {
	ctx, cancel := context.WithCancel(ctx)
	pr := &profiler{p: p, pod: pod, cancel: cancel, done: make(chan struct{})}
	if os.Getenv("OUTPUT_DIR") == "" || os.Getenv("KARPENTER_PERF_CPU_PROFILE") != "true" {
		close(pr.done)
		return pr
	}
	go pr.run(ctx)
	return pr
}

func (pr *profiler) run(ctx context.Context) {
	defer close(pr.done)
	for ctx.Err() == nil {
		data, err := pr.get(ctx, "/debug/pprof/profile", map[string]string{"seconds": fmt.Sprint(cpuProfileSeconds)})
		if err != nil {
			return
		}
		if v := profileTotal(data, "cpu"); v > pr.cpuPeak {
			pr.mu.Lock()
			pr.cpu, pr.cpuPeak = data, v
			pr.mu.Unlock()
		}
	}
}

func (pr *profiler) get(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	return pr.p.KubeClient.CoreV1().Pods(pr.p.Controller.Namespace).ProxyGet("http", pr.pod, pr.p.Controller.MetricsPort, path, params).DoRaw(ctx)
}

// stop returns a heap profile taken now and the busiest CPU profile, either nil if profiling is unavailable.
func (pr *profiler) stop(ctx context.Context) (heap, cpu []byte) {
	if pr == nil {
		return nil, nil
	}
	pr.cancel()
	<-pr.done
	if os.Getenv("OUTPUT_DIR") != "" {
		heap, _ = pr.get(ctx, "/debug/pprof/heap", nil)
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return heap, pr.cpu
}

func profileTotal(data []byte, sampleType string) int64 {
	prof, err := profile.Parse(bytes.NewReader(data))
	if err != nil {
		return 0
	}
	idx := -1
	for i, st := range prof.SampleType {
		if st.Type == sampleType {
			idx = i
		}
	}
	var total int64
	for _, s := range prof.Sample {
		if idx >= 0 && idx < len(s.Value) {
			total += s.Value[idx]
		}
	}
	return total
}

// writeProfiles saves the phase's profiles next to its report, named for the e2e workflow's artifact globs.
func writeProfiles(rep *Result, heap, cpu []byte) {
	dir := os.Getenv("OUTPUT_DIR")
	for kind, data := range map[string][]byte{"memory": heap, "cpu": cpu} {
		if dir == "" || len(data) == 0 {
			continue
		}
		name := filepath.Base(fmt.Sprintf("karpenter_%s_profile_%s.pb.gz", kind, reportBase(rep)))
		if err := os.WriteFile(filepath.Join(filepath.Clean(dir), name), data, 0o600); err != nil {
			GinkgoWriter.Printf("perf %s: writing %s profile: %v\n", rep.ID, kind, err)
		}
	}
}
