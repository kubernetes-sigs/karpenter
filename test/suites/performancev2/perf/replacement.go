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
	"context"
	"math"
	"sort"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// replacementTracker turns node and pod watch events into per-node replacement latency and unavailable pod-seconds.
// A node's latency runs from the first event that shows it disrupted (karpenter.sh/disrupted taint or a deletion
// timestamp) to its delete event. Unavailable pod-seconds integrates wanted minus healthy pods between events, from
// start, once the watches have synced, to stop.
type replacementTracker struct {
	mu              sync.Mutex
	want            int
	original        map[string]bool
	marked, gone    map[string]time.Time
	healthy         map[string]bool
	started         bool
	last            time.Time
	unavailableSecs float64
	peakUnavailable int
}

func newReplacementTracker(original []string, want int) *replacementTracker {
	t := &replacementTracker{want: want, original: map[string]bool{}, marked: map[string]time.Time{}, gone: map[string]time.Time{}, healthy: map[string]bool{}}
	for _, n := range original {
		t.original[n] = true
	}
	return t
}

// start begins integrating unavailable pods at now. Events before start only set the initial state.
func (t *replacementTracker) start(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started, t.last = true, now
	t.peakUnavailable = t.missing()
}

func (t *replacementTracker) missing() int {
	n := 0
	for _, h := range t.healthy {
		if h {
			n++
		}
	}
	return max(t.want-n, 0)
}

// advance charges the time since the last event at the deficit that held over it.
func (t *replacementTracker) advance(now time.Time) {
	if !t.started {
		return
	}
	t.unavailableSecs += float64(t.missing()) * now.Sub(t.last).Seconds()
	t.last = now
}

func (t *replacementTracker) node(now time.Time, n *corev1.Node, deleted bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.original[n.Name] {
		return
	}
	_, marked := t.marked[n.Name]
	if !marked && (deleted || n.DeletionTimestamp != nil || hasTaint(n, v1.DisruptedTaintKey)) {
		t.marked[n.Name] = now
	}
	if _, done := t.gone[n.Name]; deleted && !done {
		t.gone[n.Name] = now
	}
}

func (t *replacementTracker) pod(now time.Time, p *corev1.Pod, deleted bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.advance(now)
	if deleted {
		delete(t.healthy, p.Name)
	} else {
		t.healthy[p.Name] = p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && podReady(p)
	}
	if t.started {
		t.peakUnavailable = max(t.peakUnavailable, t.missing())
	}
}

// remaining is the number of original nodes not yet deleted.
func (t *replacementTracker) remaining() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.original) - len(t.gone)
}

func (t *replacementTracker) stop(now time.Time, rep *Result) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.advance(now)
	secs := make([]float64, 0, len(t.gone))
	for n, g := range t.gone {
		secs = append(secs, g.Sub(t.marked[n]).Seconds())
	}
	rep.UnavailablePodSeconds, rep.PeakUnavailablePods = math.Round(t.unavailableSecs*10)/10, t.peakUnavailable
	if len(secs) == 0 {
		return
	}
	sort.Float64s(secs)
	at := func(q float64) float64 { return secs[int(math.Ceil(q*float64(len(secs))))-1] }
	rep.Replacement = Quantiles{Count: len(secs), P50: at(0.50), P95: at(0.95), P99: at(0.99), Max: secs[len(secs)-1]}
}

// watchReplacement feeds the tracker from node and scenario-pod informers until ctx ends.
func (r *Run) watchReplacement(ctx context.Context, t *replacementTracker) {
	GinkgoHelper()
	nodes := informers.NewSharedInformerFactoryWithOptions(r.Provider.KubeClient, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = v1.NodePoolLabelKey }))
	pods := informers.NewSharedInformerFactoryWithOptions(r.Provider.KubeClient, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = ScenarioLabel + "=" + r.Scenario.Name }))
	handler := func(on func(time.Time, any, bool)) cache.ResourceEventHandlerFuncs {
		return cache.ResourceEventHandlerFuncs{
			AddFunc:    func(o any) { on(time.Now(), o, false) },
			UpdateFunc: func(_, o any) { on(time.Now(), o, false) },
			DeleteFunc: func(o any) {
				if d, ok := o.(cache.DeletedFinalStateUnknown); ok {
					o = d.Obj
				}
				on(time.Now(), o, true)
			},
		}
	}
	_, err := nodes.Core().V1().Nodes().Informer().AddEventHandler(handler(func(now time.Time, o any, del bool) {
		if n, ok := o.(*corev1.Node); ok {
			t.node(now, n, del)
		}
	}))
	Expect(err).ToNot(HaveOccurred())
	_, err = pods.Core().V1().Pods().Informer().AddEventHandler(handler(func(now time.Time, o any, del bool) {
		if p, ok := o.(*corev1.Pod); ok {
			t.pod(now, p, del)
		}
	}))
	Expect(err).ToNot(HaveOccurred())
	nodes.Start(ctx.Done())
	pods.Start(ctx.Done())
	for typ, ok := range nodes.WaitForCacheSync(ctx.Done()) {
		Expect(ok).To(BeTrue(), "syncing %v", typ)
	}
	for typ, ok := range pods.WaitForCacheSync(ctx.Done()) {
		Expect(ok).To(BeTrue(), "syncing %v", typ)
	}
	t.start(time.Now())
}
