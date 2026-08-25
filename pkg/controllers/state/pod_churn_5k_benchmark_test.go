//go:build test_performance_5000

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

import "testing"

// 5000-node variants of pod_churn_benchmark_test.go's benchmarks, split into their own build-tagged file because
// they're slow (multiple minutes for the full set) -- routine `make benchmark-cow` runs should stay fast, so
// these only run via the separate `make benchmark-cow-5k` target. See pod_churn_benchmark_test.go for the shared
// benchmarkPodChurn/benchmarkMarkForDeletion/benchmarkNominateNodeForPod helpers (build tag
// test_performance || test_performance_5000, so they're available here too).
//
// Run with:
//
//	KUBEBUILDER_ASSETS=<path> go test -tags=test_performance_5000 -run=XXX -bench=. ./pkg/controllers/state/...

func BenchmarkUpdatePod_LargeCluster_MedChurn(b *testing.B)  { benchmarkPodChurn(b, 5000, 8) }
func BenchmarkUpdatePod_LargeCluster_HighChurn(b *testing.B) { benchmarkPodChurn(b, 5000, 1000) }

func BenchmarkMarkForDeletion_5000(b *testing.B) { benchmarkMarkForDeletion(b, 5000) }

func BenchmarkNominateNodeForPod_5000(b *testing.B) { benchmarkNominateNodeForPod(b, 5000) }
