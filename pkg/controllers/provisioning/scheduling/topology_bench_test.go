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

	"k8s.io/utils/clock"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

func BenchmarkNewTopology(b *testing.B) {
	for _, np := range []int{1, 5, 20, 50} {
		b.Run(fmt.Sprintf("vector=nodepools/np=%d", np), func(b *testing.B) {
			ctx := benchCtx()
			pods := makeDiversePods(1000)

			cp := fake.NewCloudProvider()
			instanceTypes := fake.InstanceTypes(400)
			cp.InstanceTypes = instanceTypes

			client := fakecr.NewFakeClient()
			clk := &clock.RealClock{}
			cl := state.NewCluster(clk, client, cp)

			nodePools := benchNodePools(np)
			itsByNP := map[string][]*cloudprovider.InstanceType{}
			for _, pool := range nodePools {
				itsByNP[pool.Name] = instanceTypes
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := scheduling.NewTopology(ctx, client, cl, nil, nodePools, itsByNP, pods); err != nil {
					b.Fatalf("creating topology: %s", err)
				}
			}
		})
	}
}

// BenchmarkForEachDomain lives in topology_benchmark_test.go: constructing a TopologyDomainGroup requires the
// package-private topologyNodePool producers, so the benchmark runs from inside the package.
