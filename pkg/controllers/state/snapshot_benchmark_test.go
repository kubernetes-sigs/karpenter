//go:build test_performance || test_performance_5000

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

package state_test

import (
	"testing"

	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// BenchmarkSnapshot_* measures cluster.DeepCopyNodes() as it exists TODAY -- a full O(n) deep clone. This is the
// baseline a future generation-counter + memoized-pointer-slice Snapshot() must be compared against.
//
// BenchmarkPointerSliceCopy_* measures what that future Snapshot() will do on a cache-miss: grab a []*StateNode
// of the current live pointers under a brief lock, with no cloning at all. It's written against existing,
// already-shipped APIs (cluster.Nodes(), the read-locked iterator) so it compiles and runs today, without
// depending on the not-yet-written Snapshot() method.
//
// Run with:
//
//	go test -tags=test_performance -run=XXX -bench=. ./pkg/controllers/state/... -benchtime=10x

func BenchmarkSnapshot_400(b *testing.B) { benchmarkSnapshot(b, 400) }

func benchmarkSnapshot(b *testing.B, numNodes int) {
	cluster, _ := setupBenchCluster(b, numNodes, 5)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cluster.DeepCopyNodes()
	}
}

func BenchmarkPointerSliceCopy_400(b *testing.B) { benchmarkPointerSliceCopy(b, 400) }

func benchmarkPointerSliceCopy(b *testing.B, numNodes int) {
	cluster, _ := setupBenchCluster(b, numNodes, 5)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap := make(state.StateNodes, 0, numNodes)
		for n := range cluster.Nodes() {
			snap = append(snap, n)
		}
		if len(snap) != numNodes {
			b.Fatalf("expected %d nodes, got %d", numNodes, len(snap))
		}
	}
}
