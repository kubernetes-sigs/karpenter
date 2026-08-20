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

// 5000-node variants of snapshot_benchmark_test.go's benchmarks, split out so they don't run as part of the
// routine `make benchmark-cow` target -- see pod_churn_5k_benchmark_test.go for the rationale. Unlike the pod
// churn 5000-node benchmarks, these use the fast fake-client fixture (not envtest), so they're not actually slow
// in isolation, but are kept alongside the other _5000 variants for a consistent "large-scale" opt-in tag.
//
// Run with:
//
//	go test -tags=test_performance_5000 -run=XXX -bench=. ./pkg/controllers/state/...

func BenchmarkSnapshot_5000(b *testing.B) { benchmarkSnapshot(b, 5000) }

func BenchmarkPointerSliceCopy_5000(b *testing.B) { benchmarkPointerSliceCopy(b, 5000) }
