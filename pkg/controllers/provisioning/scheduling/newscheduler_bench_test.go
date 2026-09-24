//go:build test_performance

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

package scheduling_test

import (
	"fmt"
	"testing"
)

func BenchmarkNewScheduler(b *testing.B) {
	run := func(b *testing.B, s Scenario) {
		f := buildScenario(b, s)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = newSchedulerFromScenario(f)
		}
	}

	for _, n := range []int{0, 100, 500, 1000} {
		s := Scenario{Nodes: n, PodsPerNode: 10, NodePools: 1, DaemonSets: 0}
		b.Run(fmt.Sprintf("vector=nodes/n=%d", n), func(b *testing.B) { run(b, s) })
	}
	for _, np := range []int{1, 5, 20, 50} {
		s := Scenario{Nodes: 100, PodsPerNode: 10, NodePools: np, DaemonSets: 5}
		b.Run(fmt.Sprintf("vector=nodepools/np=%d", np), func(b *testing.B) { run(b, s) })
	}
	for _, ds := range []int{0, 1, 5, 10, 20} {
		s := Scenario{Nodes: 100, PodsPerNode: 10, NodePools: 1, DaemonSets: ds}
		b.Run(fmt.Sprintf("vector=daemonsets/ds=%d", ds), func(b *testing.B) { run(b, s) })
	}
}
