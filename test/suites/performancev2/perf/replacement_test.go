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

package perf_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

func node(name string, disrupted bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if disrupted {
		n.Spec.Taints = []corev1.Taint{{Key: v1.DisruptedTaintKey, Effect: corev1.TaintEffectNoSchedule}}
	}
	return n
}

func pod(name string, ready bool) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if ready {
		p.Status.Conditions[0].Status = corev1.ConditionTrue
	}
	return p
}

var _ = Describe("Replacement tracking", func() {
	t0 := time.Unix(1_700_000_000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

	It("times each replaced node from its first disrupted event to its delete event", func() {
		t := perf.NewReplacementTracker([]string{"a", "b"}, 0)
		t.Node(at(0), node("a", false), false)
		t.Start(at(0))
		t.Node(at(1500), node("a", true), false)
		t.Node(at(2000), node("a", true), false)
		t.Node(at(4000), node("b", false), true)
		Expect(t.Remaining()).To(Equal(1))
		t.Node(at(9500), node("a", true), true)
		Expect(t.Remaining()).To(BeZero())
		r := t.Stop(at(10000)).Replacement
		// a: disrupted at 1.5 s, gone at 9.5 s. b: deleted with no disrupted event first, so it counts from the delete.
		Expect(r.Count).To(Equal(2))
		Expect([]float64{r.P50, r.Max}).To(Equal([]float64{0, 8}))
	})

	It("ignores nodes it isn't replacing", func() {
		t := perf.NewReplacementTracker([]string{"a"}, 0)
		t.Start(at(0))
		t.Node(at(1000), node("new", true), false)
		t.Node(at(2000), node("new", true), true)
		Expect(t.Remaining()).To(Equal(1))
		Expect(t.Stop(at(3000)).Replacement.Count).To(BeZero())
	})

	It("integrates missing healthy pods between events, from Start", func() {
		t := perf.NewReplacementTracker([]string{"a"}, 2)
		t.Pod(at(0), pod("p1", true), false)
		t.Pod(at(0), pod("p2", true), false)
		t.Start(at(1000))
		t.Pod(at(3000), pod("p1", false), false) // 1 missing from 3 s
		t.Pod(at(4000), pod("p1", false), true)  // still 1 missing
		t.Pod(at(5000), pod("p2", false), false) // 2 missing from 5 s
		t.Pod(at(6000), pod("p3", true), false)  // 1 missing from 6 s
		t.Pod(at(7000), pod("p4", true), false)  // 0 missing from 7 s
		r := t.Stop(at(10000))
		// 2 s x 1 + 2 x 1 s + 1 x 1 s
		Expect(r.UnavailablePodSeconds).To(Equal(5.0))
		Expect(r.PeakUnavailablePods).To(Equal(2))
	})

	It("doesn't charge the time before Start", func() {
		t := perf.NewReplacementTracker([]string{"a"}, 3)
		t.Pod(at(0), pod("p1", true), false)
		t.Start(at(60000))
		Expect(t.Stop(at(61000)).UnavailablePodSeconds).To(Equal(2.0))
	})
})
