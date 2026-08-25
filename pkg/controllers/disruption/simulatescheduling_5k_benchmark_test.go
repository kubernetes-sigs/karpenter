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

package disruption_test

import "testing"

// 5000-node variants of simulatescheduling_benchmark_test.go's benchmarks, split into their own build-tagged
// file. These are the slowest benchmarks in the whole copy-on-write suite (envtest-backed, ~830s for the full
// disruption package run) -- excluded from the routine `make benchmark-cow` target and only runnable via the
// separate `make benchmark-cow-5k` target. See simulatescheduling_benchmark_test.go (build tag
// test_performance || test_performance_5000) for setupSimulateSchedulingBenchFixture and the shared
// benchmarkSimulateScheduling/benchmarkClusterDeepCopyNodes/benchmarkClusterNodesIterate helpers, which are
// available here too since that file's tag includes test_performance_5000.
//
// Run with:
//
//	KUBEBUILDER_ASSETS=<path> go test -tags=test_performance_5000 -run=XXX -bench=. ./pkg/controllers/disruption/...

func BenchmarkSimulateScheduling_5000(b *testing.B) { benchmarkSimulateScheduling(b, 5000) }

func BenchmarkClusterDeepCopyNodes_5000(b *testing.B) { benchmarkClusterDeepCopyNodes(b, 5000) }

func BenchmarkClusterNodesIterate_5000(b *testing.B) { benchmarkClusterNodesIterate(b, 5000) }
