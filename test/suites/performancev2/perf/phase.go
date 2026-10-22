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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	consolidationQuiet = 3 * time.Minute
	settlePoll         = 5 * time.Second
)

// Phase is one measured window. Start it before the action under test and end it with an Await method.
type Phase struct {
	r            *Run
	id           string
	gates        []Gate
	src          *source
	start        time.Time
	initialNodes int
}

func (r *Run) StartPhase(name string) *Phase {
	GinkgoHelper()
	id, gates := r.phaseID(name)
	return &Phase{r: r, id: id, gates: gates, src: startSource(r.ctx, r.Provider), start: time.Now(), initialNodes: len(r.nodes())}
}

func (p *Phase) AwaitScaleOut(pods int, timeout time.Duration) *Result {
	GinkgoHelper()
	p.r.awaitHealthyPods(pods, timeout)
	return p.end(&Result{Kind: "scale-out", Pods: pods, PodsNetChange: pods, Rounds: 1}, time.Now())
}

func (p *Phase) AwaitConsolidation(fromPods, toPods int, timeout time.Duration) *Result {
	GinkgoHelper()
	p.r.awaitHealthyPods(toPods, timeout)
	start, last, rounds, inRound, prev := time.Now(), time.Now(), 0, false, len(p.r.nodes())
	for time.Since(start) < timeout && time.Since(last) < consolidationQuiet {
		n := len(p.r.nodes())
		if p.r.disrupting() > 0 || n != prev {
			if !inRound {
				rounds++
			}
			inRound, last = true, time.Now()
		} else {
			inRound = false
		}
		prev = n
		time.Sleep(settlePoll)
	}
	return p.end(&Result{Kind: "consolidation", Pods: toPods, PodsNetChange: toPods - fromPods, Rounds: rounds}, last)
}

func (p *Phase) end(rep *Result, lastActivity time.Time) *Result {
	GinkgoHelper()
	rep.Total, rep.Active = time.Since(p.start), lastActivity.Sub(p.start)
	res, hists, counters, err := p.src.stop(p.r.ctx)
	Expect(err).ToNot(HaveOccurred())
	Expect(res.Samples).To(BeNumerically(">", 1), "collected too few metrics samples to gate on")
	rep.PodReady = p.r.podReadyQuantiles(p.start)
	nodes := p.r.nodes()
	rep.Resources, rep.LatencyStats, rep.Counters, rep.Commit = res, hists, counters, p.src.commit
	rep.Nodes, rep.NodesNetChange = len(nodes), len(nodes)-p.initialNodes
	rep.ReservedCPUUtil, rep.ReservedMemoryUtil = p.r.reserved()
	for k, v := range counters {
		if strings.HasPrefix(k, "karpenter_pods_drained_total") {
			rep.PodsDisrupted += v
		}
	}
	return p.r.finish(p.id, p.gates, rep)
}

// Restart deletes the controller pods and measures how long the new leader takes to report
// karpenter_cluster_state_synced, plus the CPU and heap it spent getting there.
func (r *Run) Restart(name string, timeout time.Duration) *Result {
	GinkgoHelper()
	id, gates := r.phaseID(name)
	pods := &corev1.PodList{}
	Expect(r.Provider.Client.List(r.ctx, pods, client.InNamespace(r.Provider.Controller.Namespace), client.MatchingLabelsSelector{Selector: r.Provider.Controller.Selector})).To(Succeed())
	start := time.Now()
	for i := range pods.Items {
		Expect(client.IgnoreNotFound(r.Provider.Client.Delete(r.ctx, &pods.Items[i]))).To(Succeed())
	}
	old := map[string]bool{}
	for _, p := range pods.Items {
		old[p.Name] = true
	}
	rep := &Result{Kind: "startup"}
	Eventually(func(g Gomega) {
		pod, f, err := leader(r.ctx, r.Provider)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(old[pod]).To(BeFalse(), "old controller pod %s still leading", pod)
		g.Expect(gauge(f, "karpenter_cluster_state_synced")).To(Equal(1.0))
		rep.StartupSeconds = time.Since(start).Seconds()
		rep.Commit = buildCommit(f)
		rep.StartupCPUSecond = counter(f, "process_cpu_seconds_total")
		rep.Resources = ResourceStats{P95HeapMB: gauge(f, "go_gc_heap_live_bytes") / (1 << 20), MaxHeapMB: gauge(f, "go_gc_heap_live_bytes") / (1 << 20), Samples: 1}
	}).WithTimeout(timeout).WithPolling(time.Second).Should(Succeed())
	rep.Total = time.Since(start)
	rep.Active = rep.Total
	rep.Nodes = len(r.nodes())
	return r.finish(id, gates, rep)
}
