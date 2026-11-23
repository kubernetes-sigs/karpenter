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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	consolidationQuiet = 3 * time.Minute
	settlePoll         = 5 * time.Second
)

// Phase is one measured window. Start it before the action under test and end it with an Await method. The Await
// methods of a skipped phase (see Off) return nil without waiting.
type Phase struct {
	r            *Run
	id           string
	gates        []Gate
	src          *source
	prof         *profiler
	start        time.Time
	initialNodes []string
	skip         bool
	timedOut     string
}

func (r *Run) StartPhase(name string) *Phase {
	GinkgoHelper()
	id, gates, skip := r.phaseID(name)
	if skip {
		return &Phase{r: r, id: id, skip: true}
	}
	src := startSource(r.ctx, r.Provider)
	return &Phase{r: r, id: id, gates: gates, src: src, prof: startProfiler(r.ctx, r.Provider, src.pod), start: time.Now(), initialNodes: nodeNames(r.nodes())}
}

// AwaitScaleOut waits for the pods to be healthy. Its total time runs from the first pod's creation to the last
// pod's Ready condition, both API server timestamps, so it doesn't depend on how often the suite polls.
func (p *Phase) AwaitScaleOut(pods int, timeout time.Duration) *Result {
	GinkgoHelper()
	if p.skip {
		return nil
	}
	p.timeOut(p.r.awaitHealthyPods(pods, p.r.timeout(timeout)))
	rep := &Result{Kind: "scale-out", Pods: pods, PodsNetChange: pods, Rounds: 1}
	if first, last, ok := p.r.podSpan(p.start); ok {
		rep.Total = last.Sub(first)
		rep.Active = rep.Total
	}
	return p.end(rep, time.Now())
}

// AwaitConsolidation waits for the pods, then for consolidation to settle: no node changes or disruption decisions
// for consolidationQuiet, and a single-node consolidation pass (the last method) that finished after the last
// change, so a long evaluation isn't mistaken for having nothing to do.
func (p *Phase) AwaitConsolidation(fromPods, toPods int, timeout time.Duration) *Result {
	GinkgoHelper()
	if p.skip {
		return nil
	}
	timeout = p.r.timeout(timeout)
	p.timeOut(p.r.awaitHealthyPods(toPods, timeout))
	start, last, rounds, inRound, prev := time.Now(), time.Now(), 0, false, len(p.r.nodes())
	f := p.src.latest()
	decisions, passesAtLast := disruptionDecisions(f), singleNodePasses(f)
	settled := func() bool { return time.Since(last) >= consolidationQuiet && singleNodePasses(f) > passesAtLast }
	for !settled() && time.Since(start) < timeout {
		n := len(p.r.nodes())
		f = p.src.latest()
		if d := disruptionDecisions(f); p.r.disrupting() > 0 || n != prev || d != decisions {
			if !inRound {
				rounds++
			}
			inRound, last, decisions, passesAtLast = true, time.Now(), d, singleNodePasses(f)
		} else {
			inRound = false
		}
		prev = n
		time.Sleep(settlePoll)
	}
	rep := &Result{Kind: "consolidation", Pods: toPods, PodsNetChange: toPods - fromPods, Rounds: rounds, ConsolidationTimedOut: !settled()}
	if rep.ConsolidationTimedOut && p.timedOut == "" {
		p.timedOut = fmt.Sprintf("consolidation didn't settle within %s (%d rounds, last change %s before the timeout)", timeout, rounds, time.Since(last).Round(time.Second))
	}
	return p.end(rep, last)
}

// AwaitDrift ends the phase once every node from its start is replaced, nothing is disrupting, and the scenario has
// the given healthy pods.
func (p *Phase) AwaitDrift(pods int, timeout time.Duration) *Result {
	GinkgoHelper()
	if p.skip {
		return nil
	}
	timeout = p.r.timeout(timeout)
	rounds, inRound := 0, false
	Eventually(func(g Gomega) {
		nodes := p.r.nodes()
		disrupting := p.r.disrupting()
		switch {
		case disrupting > 0 && !inRound:
			rounds, inRound = rounds+1, true
		case disrupting == 0:
			inRound = false
		}
		remaining := sets.New(nodeNames(nodes)...).Intersection(sets.New(p.initialNodes...))
		g.Expect(remaining.UnsortedList()).To(BeEmpty(), "nodes from before drift")
		g.Expect(disrupting).To(BeZero(), "nodes being disrupted")
		g.Expect(p.r.healthyPods()).To(Equal(pods), "healthy %s pods", p.r.Scenario.Name)
	}).WithTimeout(timeout).WithPolling(settlePoll).Should(Succeed())
	return p.end(&Result{Kind: "drift", Pods: pods, Rounds: rounds}, time.Now())
}

func nodeNames(nodes []corev1.Node) []string {
	out := make([]string, 0, len(nodes))
	for i := range nodes {
		out = append(out, nodes[i].Name)
	}
	return out
}

func (p *Phase) end(rep *Result, lastActivity time.Time) *Result {
	GinkgoHelper()
	if rep.Total == 0 {
		rep.Total, rep.Active = time.Since(p.start), lastActivity.Sub(p.start)
	}
	res, hists, counters, gauges, err := p.src.stop(p.r.ctx)
	Expect(err).ToNot(HaveOccurred())
	heap, cpu := p.prof.stop(p.r.ctx)
	Expect(res.Samples).To(BeNumerically(">", 1), "collected too few metrics samples to gate on")
	rep.PodReady = p.r.podReadyQuantiles(p.start)
	nodes := p.r.nodes()
	rep.Resources, rep.LatencyStats, rep.Counters, rep.Gauges, rep.Commit = res, hists, counters, gauges, p.src.commit
	rep.Nodes, rep.NodesNetChange = len(nodes), len(nodes)-len(p.initialNodes)
	rep.ReservedCPUUtil, rep.ReservedMemoryUtil = p.r.reserved()
	rep.Timeline = timeline(p.start, p.r.scenarioPods(), nodes, p.r.nodeClaims())
	keepMixPrices(rep.Gauges, rep.Timeline.NodeMix)
	for k, v := range counters {
		if strings.HasPrefix(k, "karpenter_pods_drained_total") {
			rep.PodsDisrupted += v
		}
	}
	rep.TimedOut = p.timedOut
	rep = p.r.finish(p.id, p.gates, rep)
	writeProfiles(rep, heap, cpu)
	if p.timedOut != "" {
		Fail(p.r.timeoutAlarm(p.id, p.timedOut))
	}
	return rep
}

// timeOut records the first wait that ran out of time. The phase still ends normally, so its report and profiles are
// written as evidence, and then the spec fails.
func (p *Phase) timeOut(err error) {
	if err != nil && p.timedOut == "" {
		p.timedOut = err.Error()
	}
}

// Restart deletes the controller pods and measures how long the new leader takes to report
// karpenter_cluster_state_synced, plus the CPU and heap it spent getting there.
func (r *Run) Restart(name string, timeout time.Duration) *Result {
	GinkgoHelper()
	id, gates, skip := r.phaseID(name)
	if skip {
		return nil
	}
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
	}).WithTimeout(r.timeout(timeout)).WithPolling(time.Second).Should(Succeed())
	rep.Total = time.Since(start)
	rep.Active = rep.Total
	rep.Nodes = len(r.nodes())
	return r.finish(id, gates, rep)
}
