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
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/pprof/profile"
	. "github.com/onsi/ginkgo/v2"
)

const CPUProfileSeconds = 20

// KarpenterProfiler polls pprof for Karpenter memory and CPU usage and captures profiles at peak
type KarpenterProfiler struct {
	env               *Environment
	peakMemoryMB      float64
	peakMemoryProfile []byte
	peakCPUNanos      int64
	peakCPUProfile    []byte
	cancel            context.CancelFunc
	done              chan struct{}
	pollCount         int
	lastError         string
}

// StartKarpenterProfiler begins profiling Karpenter resource usage in the background
func StartKarpenterProfiler(env *Environment) *KarpenterProfiler {
	ctx, cancel := context.WithCancel(env.Context)
	kp := &KarpenterProfiler{
		env:    env,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go kp.run(ctx)
	return kp
}

// Stop stops the profiler and returns peak memory (MB), memory profile, peak CPU (nanoseconds), and CPU profile
func (kp *KarpenterProfiler) Stop() (float64, []byte, int64, []byte) {
	kp.cancel()
	<-kp.done
	GinkgoWriter.Printf("KarpenterProfiler: Stopped after %d polls, peakMemory=%.2f MB, peakCPU=%.2f ms, lastError=%s\n", kp.pollCount, kp.peakMemoryMB, float64(kp.peakCPUNanos)/1e6, kp.lastError)
	return kp.peakMemoryMB, kp.peakMemoryProfile, kp.peakCPUNanos, kp.peakCPUProfile
}

func (kp *KarpenterProfiler) run(ctx context.Context) {
	defer close(kp.done)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		kp.pollCount++
		kp.captureProfiles(ctx)
	}
}

func (kp *KarpenterProfiler) captureProfiles(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			kp.lastError = fmt.Sprintf("captureProfiles panic: %v", r)
		}
	}()

	pod := kp.env.ExpectActiveKarpenterPod()
	if pod == nil {
		return
	}

	portCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	localPort := 1024
	kp.env.ExpectPodPortForwarded(portCtx, pod, 8080, localPort)

	// Capture heap profile (instant)
	if memMB, memData := kp.fetchHeapProfile(localPort); memMB > kp.peakMemoryMB {
		kp.peakMemoryMB = memMB
		kp.peakMemoryProfile = memData
	}

	// Capture CPU profile (20 second sample)
	if cpuNanos, cpuData := kp.fetchCPUProfile(localPort); cpuNanos > kp.peakCPUNanos {
		kp.peakCPUNanos = cpuNanos
		kp.peakCPUProfile = cpuData
	}
}

func (kp *KarpenterProfiler) fetchHeapProfile(port int) (float64, []byte) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/heap", port))
	if err != nil {
		kp.lastError = fmt.Sprintf("pprof heap error: %v", err)
		return 0, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil
	}

	memBytes := parseProfileValue(data, "inuse_space")
	return float64(memBytes) / (1024 * 1024), data
}

func (kp *KarpenterProfiler) fetchCPUProfile(port int) (int64, []byte) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/profile?seconds=%d", port, CPUProfileSeconds))
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil
	}

	cpuNanos := parseProfileValue(data, "cpu")
	return cpuNanos, data
}

func parseProfileValue(data []byte, sampleType string) int64 {
	p, err := profile.ParseData(data)
	if err != nil {
		return 0
	}

	var idx = -1
	for i, st := range p.SampleType {
		if st.Type == sampleType {
			idx = i
			break
		}
	}
	if idx < 0 {
		return 0
	}

	var total int64
	for _, s := range p.Sample {
		if idx < len(s.Value) {
			total += s.Value[idx]
		}
	}
	return total
}
