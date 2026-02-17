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
	"sync"
	"time"

	"github.com/google/pprof/profile"
	. "github.com/onsi/ginkgo/v2"
	"k8s.io/apimachinery/pkg/util/rand"
)

const pollInterval = 10 * time.Second

// MemoryTracker polls pprof for Karpenter memory usage and captures profile at peak
type MemoryTracker struct {
	env             *Environment
	mu              sync.Mutex
	peakMemoryMB    float64
	peakProfileData []byte
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	pollCount       int
	lastError       string
}

// StartMemoryTracker begins tracking Karpenter memory usage in the background
func StartMemoryTracker(env *Environment) *MemoryTracker {
	ctx, cancel := context.WithCancel(env.Context)
	mt := &MemoryTracker{
		env:    env,
		cancel: cancel,
	}
	mt.wg.Add(1)
	go mt.poll(ctx)
	return mt
}

// Stop stops the memory tracker and returns peak memory (MB) and the pprof profile data at peak
func (mt *MemoryTracker) Stop() (float64, []byte) {
	mt.cancel()
	mt.wg.Wait()
	mt.mu.Lock()
	defer mt.mu.Unlock()
	GinkgoWriter.Printf("[MemoryTracker] Stopped after %d polls, peak=%.2f MB, lastError=%s\n", mt.pollCount, mt.peakMemoryMB, mt.lastError)
	return mt.peakMemoryMB, mt.peakProfileData
}

func (mt *MemoryTracker) poll(ctx context.Context) {
	defer mt.wg.Done()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mt.mu.Lock()
			mt.pollCount++
			mt.mu.Unlock()

			memMB, profileData := mt.captureHeapProfile()
			if memMB <= 0 {
				continue
			}

			mt.mu.Lock()
			if memMB > mt.peakMemoryMB {
				mt.peakMemoryMB = memMB
				mt.peakProfileData = profileData
				GinkgoWriter.Printf("[MemoryTracker] New peak: %.2f MB\n", memMB)
			}
			mt.mu.Unlock()
		}
	}
}

func (mt *MemoryTracker) captureHeapProfile() (float64, []byte) {
	defer func() {
		if r := recover(); r != nil {
			mt.mu.Lock()
			mt.lastError = fmt.Sprintf("captureHeapProfile panic: %v", r)
			mt.mu.Unlock()
		}
	}()

	pod := mt.env.ExpectActiveKarpenterPod()
	if pod == nil {
		return 0, nil
	}

	ctx, cancel := context.WithTimeout(mt.env.Context, 10*time.Second)
	defer cancel()

	localPort := rand.IntnRange(1024, 49151)
	mt.env.ExpectPodPortForwarded(ctx, pod, 8080, localPort)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/heap", localPort))
	if err != nil {
		mt.mu.Lock()
		mt.lastError = fmt.Sprintf("pprof http error: %v", err)
		mt.mu.Unlock()
		return 0, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		mt.mu.Lock()
		mt.lastError = fmt.Sprintf("pprof status: %d", resp.StatusCode)
		mt.mu.Unlock()
		return 0, nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil
	}

	// Parse profile to get heap inuse bytes
	memBytes := parseHeapInuse(data)
	if memBytes <= 0 {
		mt.mu.Lock()
		mt.lastError = "failed to parse heap profile"
		mt.mu.Unlock()
		return 0, nil
	}

	return float64(memBytes) / (1024 * 1024), data
}

func parseHeapInuse(data []byte) int64 {
	p, err := profile.ParseData(data)
	if err != nil {
		return 0
	}

	// Find inuse_space sample type index
	var inuseIdx = -1
	for i, st := range p.SampleType {
		if st.Type == "inuse_space" {
			inuseIdx = i
			break
		}
	}
	if inuseIdx < 0 {
		return 0
	}

	// Sum all inuse_space values
	var total int64
	for _, s := range p.Sample {
		if inuseIdx < len(s.Value) {
			total += s.Value[inuseIdx]
		}
	}
	return total
}
