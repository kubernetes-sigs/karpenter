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

	"k8s.io/apimachinery/pkg/types"
)

// These benchmarks establish the BEFORE baseline for the write path that a future copy-on-write refactor of
// updateForPod/cleanupForPod, Nominate, and MarkForDeletion/UnmarkForDeletion must not regress unacceptably.
// UpdatePod/DeletePod today mutate an existing *StateNode in place; after the refactor they will instead clone
// and swap. Re-running these same benchmarks post-refactor (comparing via benchstat) is how that trade-off gets
// measured, not assumed.
//
// "Churn" here means pod bind/unbind events per iteration, calibrated to the rates discussed for medium (~5-10
// pod events per 5 minutes) and high (~1000 pod events per 5 minutes) churn clusters -- the absolute rate doesn't
// matter for a per-op ns/op benchmark, only that both ends of the realistic range are covered.
//
// Run with:
//
//	KUBEBUILDER_ASSETS=<path> go test -tags=test_performance -run=XXX -bench=. ./pkg/controllers/state/... -benchtime=20x

// "MedChurn"/"HighChurn" model one churn round as a batch of eventsPerRound pod bind+unbind pairs (~5-10 vs
// ~1000, per the target rates -- "per 5 minutes" only matters for translating ns/op into a real-world rate,
// not for the benchmark itself). Each b.N iteration executes one full round, so ns/op is directly "cost of one
// churn round at this intensity" -- this also surfaces any cost that isn't purely linear per-event (lock
// contention, GC pressure from the churn's allocations), which a single-event benchmark would hide.
func BenchmarkUpdatePod_MediumCluster_MedChurn(b *testing.B)  { benchmarkPodChurn(b, 400, 8) }
func BenchmarkUpdatePod_MediumCluster_HighChurn(b *testing.B) { benchmarkPodChurn(b, 400, 1000) }

// benchmarkPodChurn measures the cost of one churn round -- eventsPerRound bind-then-unbind pod events -- against
// a cluster of numNodes nodes, each pre-populated with 20 pods (a realistic per-node pod count).
//
// Exported (not lowercase-package-private in spirit, just not `_test`-file-local) so the 5000-node variants in
// pod_churn_5k_benchmark_test.go (build tag test_performance_5000) can reuse it.
func benchmarkPodChurn(b *testing.B, numNodes, eventsPerRound int) {
	cluster, nodes := setupBenchCluster(b, numNodes, 20)
	ctx := benchContext()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for e := 0; e < eventsPerRound; e++ {
			node := nodes[(i*eventsPerRound+e)%len(nodes)]
			pod := randomPod(node.Name, i*eventsPerRound+e)
			if err := cluster.UpdatePod(ctx, pod); err != nil {
				b.Fatalf("binding pod, %s", err)
			}
			cluster.DeletePod(types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
		}
	}
}

// These isolate the other two paths that will also become copy-on-write: MarkForDeletion/UnmarkForDeletion
// (markedForDeletion) and NominateNodeForPod (nominatedUntil). Both are expected to stay cheap post-refactor
// since neither mutates the map-shaped fields that require a real clone -- this benchmark exists to prove that
// expectation rather than assume it.

func BenchmarkMarkForDeletion_400(b *testing.B) { benchmarkMarkForDeletion(b, 400) }

func benchmarkMarkForDeletion(b *testing.B, numNodes int) {
	cluster, nodes := setupBenchCluster(b, numNodes, 0)
	providerIDs := make([]string, len(nodes))
	for i, n := range nodes {
		providerIDs[i] = n.Spec.ProviderID
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := providerIDs[i%len(providerIDs)]
		cluster.MarkForDeletion(id)
		cluster.UnmarkForDeletion(id)
	}
}

func BenchmarkNominateNodeForPod_400(b *testing.B) { benchmarkNominateNodeForPod(b, 400) }

func benchmarkNominateNodeForPod(b *testing.B, numNodes int) {
	cluster, nodes := setupBenchCluster(b, numNodes, 0)
	providerIDs := make([]string, len(nodes))
	for i, n := range nodes {
		providerIDs[i] = n.Spec.ProviderID
	}
	ctx := benchContext()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cluster.NominateNodeForPod(ctx, providerIDs[i%len(providerIDs)])
	}
}
