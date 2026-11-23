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
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ReplacementTracker exposes replacementTracker to the perf_test specs.
type ReplacementTracker struct{ t *replacementTracker }

func NewReplacementTracker(original []string, want int) ReplacementTracker {
	return ReplacementTracker{newReplacementTracker(original, want)}
}

func (r ReplacementTracker) Start(now time.Time) { r.t.start(now) }
func (r ReplacementTracker) Node(now time.Time, n *corev1.Node, deleted bool) {
	r.t.node(now, n, deleted)
}
func (r ReplacementTracker) Pod(now time.Time, p *corev1.Pod, deleted bool) { r.t.pod(now, p, deleted) }
func (r ReplacementTracker) Remaining() int                                 { return r.t.remaining() }

func (r ReplacementTracker) Stop(now time.Time) *Result {
	rep := &Result{}
	r.t.stop(now, rep)
	return rep
}

func Resized(p *Provider, s *Scenario, sizes map[Vector]int) bool { return p.resized(s, sizes) }
