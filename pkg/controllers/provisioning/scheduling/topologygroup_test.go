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

// Internal package tests for TopologyGroup primitives. These are kept
// separate from the behavioral topology tests in topology_test.go
// (which exercise Topology end-to-end via the Scheduler) and from the
// NodeClaim optimization tests (which focus on the optimization pass).
// Primitive-level state mutations — Record / Unrecord / Register —
// benefit from direct, table-driven coverage of their invariants.

package scheduling

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

// newTestTopologyGroup constructs a minimal TopologyGroup for primitive
// tests. Only the fields Record/Unrecord touch are populated.
func newTestTopologyGroup(initial map[string]int32) *TopologyGroup {
	tg := &TopologyGroup{
		domains:      map[string]int32{},
		emptyDomains: sets.New[string](),
	}
	for d, count := range initial {
		tg.domains[d] = count
		if count == 0 {
			tg.emptyDomains.Insert(d)
		}
	}
	return tg
}

func TestTopologyGroupUnrecord_DecrementsKnownDomain(t *testing.T) {
	tg := newTestTopologyGroup(map[string]int32{"a": 2})

	tg.Unrecord("a")

	if got, want := tg.domains["a"], int32(1); got != want {
		t.Fatalf("domains[a] = %d, want %d", got, want)
	}
	if tg.emptyDomains.Has("a") {
		t.Fatalf("domain a is not empty; should not appear in emptyDomains")
	}
}

func TestTopologyGroupUnrecord_ReinsertsIntoEmptyDomainsAtZero(t *testing.T) {
	tg := newTestTopologyGroup(map[string]int32{"a": 1})

	tg.Unrecord("a")

	if got, want := tg.domains["a"], int32(0); got != want {
		t.Fatalf("domains[a] = %d, want %d", got, want)
	}
	if !tg.emptyDomains.Has("a") {
		t.Fatalf("domain a reached zero; emptyDomains should contain it")
	}
}

func TestTopologyGroupUnrecord_UnknownDomainIsNoOp(t *testing.T) {
	tg := newTestTopologyGroup(map[string]int32{"a": 1})

	tg.Unrecord("does-not-exist")

	if _, ok := tg.domains["does-not-exist"]; ok {
		t.Fatalf("Unrecord inserted an unknown domain into domains map")
	}
	if tg.emptyDomains.Has("does-not-exist") {
		t.Fatalf("Unrecord inserted an unknown domain into emptyDomains")
	}
	if got, want := tg.domains["a"], int32(1); got != want {
		t.Fatalf("unrelated domain a changed: got %d, want %d", got, want)
	}
}

func TestTopologyGroupUnrecord_GuardsAgainstNegativeCount(t *testing.T) {
	tg := newTestTopologyGroup(map[string]int32{"a": 0})

	tg.Unrecord("a")

	if got, want := tg.domains["a"], int32(0); got != want {
		t.Fatalf("domains[a] = %d, want %d (must not go negative)", got, want)
	}
	if !tg.emptyDomains.Has("a") {
		t.Fatalf("zero-count domain a should still be in emptyDomains after a guarded Unrecord")
	}
}

func TestTopologyGroupUnrecord_VariadicDecrementsEach(t *testing.T) {
	tg := newTestTopologyGroup(map[string]int32{"a": 2, "b": 1, "c": 3})

	tg.Unrecord("a", "b", "c")

	if got, want := tg.domains["a"], int32(1); got != want {
		t.Fatalf("domains[a] = %d, want %d", got, want)
	}
	if got, want := tg.domains["b"], int32(0); got != want {
		t.Fatalf("domains[b] = %d, want %d", got, want)
	}
	if got, want := tg.domains["c"], int32(2); got != want {
		t.Fatalf("domains[c] = %d, want %d", got, want)
	}
	if !tg.emptyDomains.Has("b") {
		t.Fatalf("domain b reached zero; emptyDomains should contain it")
	}
	if tg.emptyDomains.Has("a") || tg.emptyDomains.Has("c") {
		t.Fatalf("only domain b should be in emptyDomains, got %v", tg.emptyDomains)
	}
}

func TestTopologyGroupUnrecord_SymmetricWithRecord(t *testing.T) {
	// Record then Unrecord the same domains — end state matches start state.
	tg := newTestTopologyGroup(map[string]int32{"a": 0, "b": 0})

	tg.Record("a", "b", "a")
	if got, want := tg.domains["a"], int32(2); got != want {
		t.Fatalf("after Record, domains[a] = %d, want %d", got, want)
	}
	if tg.emptyDomains.Has("a") || tg.emptyDomains.Has("b") {
		t.Fatalf("after Record, neither a nor b should be empty, got %v", tg.emptyDomains)
	}

	tg.Unrecord("a", "b", "a")

	if got, want := tg.domains["a"], int32(0); got != want {
		t.Fatalf("after Unrecord, domains[a] = %d, want %d", got, want)
	}
	if got, want := tg.domains["b"], int32(0); got != want {
		t.Fatalf("after Unrecord, domains[b] = %d, want %d", got, want)
	}
	if !tg.emptyDomains.Has("a") || !tg.emptyDomains.Has("b") {
		t.Fatalf("after Unrecord, both a and b should be empty, got %v", tg.emptyDomains)
	}
}
