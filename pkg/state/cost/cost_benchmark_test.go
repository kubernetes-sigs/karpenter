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

package cost_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/state/cost"
)

// freshCloudProvider returns a new instance type list on every call, like a provider that resolves offerings per
// NodePool, so a list the cost tracker keeps can't be shared with the provider's cache.
type freshCloudProvider struct {
	*fake.CloudProvider
}

func (freshCloudProvider) GetInstanceTypes(context.Context, *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	return fake.InstanceTypesAssorted(), nil
}

type costBenchmark struct {
	nodePools  []*v1.NodePool
	nodeClaims []*v1.NodeClaim
	kubeClient client.Client
}

// newCostBenchmark creates wide-open NodePools with nodeClaimsPerNodePool NodeClaims each, spread over
// instanceTypesPerNodePool instance types.
func newCostBenchmark(nodePools, nodeClaimsPerNodePool, instanceTypesPerNodePool int) *costBenchmark {
	instanceTypes := fake.InstanceTypesAssorted()
	bm := &costBenchmark{}
	builder := fakecr.NewClientBuilder()
	for i := range nodePools {
		np := &v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("nodepool-%d", i)}}
		builder = builder.WithObjects(np)
		bm.nodePools = append(bm.nodePools, np)
		for j := range nodeClaimsPerNodePool {
			it := instanceTypes[(i*instanceTypesPerNodePool+j%instanceTypesPerNodePool)%len(instanceTypes)]
			bm.nodeClaims = append(bm.nodeClaims, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s-%d", np.Name, j),
				Labels: map[string]string{
					v1.NodePoolLabelKey:            np.Name,
					corev1.LabelInstanceTypeStable: it.Name,
					v1.CapacityTypeLabelKey:        it.Offerings[0].CapacityType(),
					corev1.LabelTopologyZone:       it.Offerings[0].Zone(),
				},
			}})
		}
	}
	bm.kubeClient = builder.Build()
	return bm
}

func (bm *costBenchmark) track(ctx context.Context, b *testing.B, clusterCost *cost.ClusterCost) {
	for _, nodeClaim := range bm.nodeClaims {
		if err := clusterCost.UpdateNodeClaim(ctx, nodeClaim); err != nil {
			b.Fatal(err)
		}
	}
}

func heapInUse() float64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return float64(stats.HeapAlloc)
}

// BenchmarkClusterCostRetained reports the heap the cost tracker keeps after tracking every NodeClaim and one pricing
// refresh per NodePool.
func BenchmarkClusterCostRetained(b *testing.B) {
	for _, tc := range []struct{ nodeClaims, instanceTypes int }{{1, 1}, {40, 4}, {40, 40}} {
		b.Run(fmt.Sprintf("nodepools=25/nodeclaims=%d/instancetypes=%d", tc.nodeClaims, tc.instanceTypes), func(b *testing.B) {
			ctx := context.Background()
			bm := newCostBenchmark(25, tc.nodeClaims, tc.instanceTypes)
			provider := freshCloudProvider{fake.NewCloudProvider()}
			var retained float64
			b.ReportAllocs()
			for b.Loop() {
				before := heapInUse()
				clusterCost := cost.NewClusterCost(ctx, provider, bm.kubeClient)
				bm.track(ctx, b, clusterCost)
				for _, np := range bm.nodePools {
					clusterCost.UpdateOfferings(ctx, np, fake.InstanceTypesAssorted())
				}
				retained += heapInUse() - before
				runtime.KeepAlive(clusterCost)
			}
			b.ReportMetric(retained/float64(b.N), "retained-B/op")
		})
	}
}

// BenchmarkClusterCostUpdateOfferings measures a pricing refresh of every NodePool.
func BenchmarkClusterCostUpdateOfferings(b *testing.B) {
	ctx := context.Background()
	bm := newCostBenchmark(25, 40, 40)
	provider := fake.NewCloudProvider()
	provider.InstanceTypes = fake.InstanceTypesAssorted()
	clusterCost := cost.NewClusterCost(ctx, provider, bm.kubeClient)
	bm.track(ctx, b, clusterCost)
	b.ReportAllocs()
	for b.Loop() {
		for _, np := range bm.nodePools {
			clusterCost.UpdateOfferings(ctx, np, provider.InstanceTypes)
		}
	}
}
