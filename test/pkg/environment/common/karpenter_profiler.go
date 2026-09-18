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
	"strings"
	"time"

	"github.com/google/pprof/profile"
	. "github.com/onsi/ginkgo/v2"
	"github.com/samber/lo"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const CPUProfileSeconds = 20

// KarpenterProfiler collects pprof heap and CPU profiles from the Karpenter pod
// for debugging artifacts. It captures the profile at peak heap usage and the
// highest CPU sample. These profiles are intended for offline analysis with
// `go tool pprof`, not for regression assertions (use KarpenterMetricsPoller for that).
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

// StartKarpenterProfiler begins collecting pprof profiles from the Karpenter pod
// in the background. Heap profiles are captured every 30 seconds (instant).
// CPU profiles are captured every 60 seconds (blocks for CPUProfileSeconds).
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

// Stop stops the profiler and returns the pprof artifacts:
// (heapProfileData, cpuProfileData)
func (kp *KarpenterProfiler) Stop() ([]byte, []byte) {
	kp.cancel()
	<-kp.done
	GinkgoWriter.Printf("KarpenterProfiler: Stopped after %d polls, peakHeap=%.2f MB, peakCPU=%.2f ms, lastError=%s\n",
		kp.pollCount, kp.peakMemoryMB, float64(kp.peakCPUNanos)/1e6, kp.lastError)
	return kp.peakMemoryProfile, kp.peakCPUProfile
}

func (kp *KarpenterProfiler) run(ctx context.Context) {
	defer close(kp.done)

	localPort := rand.IntnRange(10000, 49151)
	pfCtx, pfCancel := context.WithCancel(ctx)
	defer pfCancel()

	if err := kp.establishPortForward(pfCtx, localPort); err != nil {
		kp.lastError = fmt.Sprintf("initial port-forward: %v", err)
		GinkgoWriter.Printf("KarpenterProfiler: failed to establish initial port-forward: %v\n", err)
		return
	}
	GinkgoWriter.Printf("KarpenterProfiler: port-forward established on port %d\n", localPort)

	heapTicker := time.NewTicker(30 * time.Second)
	defer heapTicker.Stop()

	// Capture an initial heap profile immediately
	kp.captureHeap(localPort)

	for {
		select {
		case <-ctx.Done():
			return
		case <-heapTicker.C:
			kp.pollCount++
			kp.captureHeap(localPort)
			if kp.pollCount%2 == 0 {
				kp.captureCPU(localPort)
			}
		}
	}
}

func (kp *KarpenterProfiler) establishPortForward(ctx context.Context, localPort int) error {
	findCtx, findCancel := context.WithTimeout(ctx, 10*time.Second)
	defer findCancel()

	pod, err := kp.env.FindActiveKarpenterPod(findCtx)
	if err != nil || pod == nil {
		return fmt.Errorf("finding karpenter pod: %w", err)
	}

	if err := kp.env.PortForwardPod(ctx, pod, 8080, localPort); err != nil {
		return fmt.Errorf("port-forward to %s: %w", pod.Name, err)
	}
	return nil
}

func (kp *KarpenterProfiler) captureHeap(localPort int) {
	if memMB, memData := kp.fetchHeapProfile(localPort); memMB > kp.peakMemoryMB {
		GinkgoWriter.Printf("KarpenterProfiler: [heap] new peak memory: %.2f MB (previous: %.2f MB)\n", memMB, kp.peakMemoryMB)
		kp.peakMemoryMB = memMB
		kp.peakMemoryProfile = memData
	}
}

func (kp *KarpenterProfiler) captureCPU(localPort int) {
	if cpuNanos, cpuData := kp.fetchCPUProfile(localPort); cpuNanos > kp.peakCPUNanos {
		GinkgoWriter.Printf("KarpenterProfiler: [cpu] new peak CPU: %.2f ms (previous: %.2f ms)\n", float64(cpuNanos)/1e6, float64(kp.peakCPUNanos)/1e6)
		kp.peakCPUNanos = cpuNanos
		kp.peakCPUProfile = cpuData
	}
}

var profilerClient = &http.Client{Timeout: 30 * time.Second}

func (kp *KarpenterProfiler) fetchHeapProfile(port int) (float64, []byte) {
	resp, err := profilerClient.Get(fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/heap", port)) //nolint:gosec
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
	// CPU profile blocks for CPUProfileSeconds, so use a longer timeout
	cpuClient := &http.Client{Timeout: time.Duration(CPUProfileSeconds+15) * time.Second}
	resp, err := cpuClient.Get(fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/profile?seconds=%d", port, CPUProfileSeconds)) //nolint:gosec
	if err != nil {
		kp.lastError = fmt.Sprintf("pprof cpu error: %v", err)
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

// PortForwardPod creates a port-forward to a pod without using Ginkgo assertions.
// Safe to call from background goroutines. The port-forward is torn down when ctx is canceled.
// Callers must cancel ctx to clean up the port-forward goroutine, even if this function returns an error.
func (env *Environment) PortForwardPod(ctx context.Context, pod *corev1.Pod, podPort, localPort int) error {
	roundTripper, upgrader, err := spdy.RoundTripperFor(env.Config)
	if err != nil {
		return fmt.Errorf("creating round tripper: %w", err)
	}

	serverURL := env.KubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("portforward").URL()

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, serverURL)

	ready := make(chan struct{})
	errCh := make(chan error, 1)
	stop := make(chan struct{})

	go func() {
		<-ctx.Done()
		close(stop)
	}()

	go func() {
		fw, err := portforward.New(dialer, []string{fmt.Sprintf("%d:%d", localPort, podPort)}, stop, ready, io.Discard, io.Discard)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- fw.ForwardPorts()
	}()

	select {
	case <-ready:
		return nil
	case err := <-errCh:
		return fmt.Errorf("port-forward failed: %w", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FindActiveKarpenterPod finds the active Karpenter pod without using Ginkgo assertions.
// Safe to call from background goroutines. Returns nil, err if the pod cannot be found.
func (env *Environment) FindActiveKarpenterPod(ctx context.Context) (*corev1.Pod, error) {
	lease := &coordinationv1.Lease{}
	if err := env.Client.Get(ctx, types.NamespacedName{Name: "karpenter-leader-election", Namespace: "kube-system"}, lease); err != nil {
		return nil, err
	}

	holderArr := strings.Split(lo.FromPtr(lease.Spec.HolderIdentity), "_")
	if len(holderArr) == 0 || holderArr[0] == "" {
		return nil, fmt.Errorf("lease holder identity is empty")
	}

	pod := &corev1.Pod{}
	if err := env.Client.Get(ctx, types.NamespacedName{Name: holderArr[0], Namespace: "kube-system"}, pod); err != nil {
		return nil, err
	}
	return pod, nil
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
