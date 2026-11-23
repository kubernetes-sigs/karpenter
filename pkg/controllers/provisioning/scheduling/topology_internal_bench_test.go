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

package scheduling

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
)

func BenchmarkBuildDomainGroups(b *testing.B) {
	for _, nodePools := range []int{1, 5, 20, 40} {
		b.Run(fmt.Sprintf("vector=nodepools/np=%d", nodePools), func(b *testing.B) {
			benchmarkBuildDomainGroups(b, nodePools, 400)
		})
	}
	for _, instanceTypes := range []int{50, 100, 200, 400} {
		b.Run(fmt.Sprintf("vector=instancetypes/it=%d", instanceTypes), func(b *testing.B) {
			benchmarkBuildDomainGroups(b, 40, instanceTypes)
		})
	}
}

func benchmarkBuildDomainGroups(b *testing.B, nodePoolCount, instanceTypeCount int) {
	instanceTypes := fake.InstanceTypes(instanceTypeCount)
	nodePools := benchDomainGroupNodePools(nodePoolCount)
	itsByNodePool := map[string][]*cloudprovider.InstanceType{}
	for _, nodePool := range nodePools {
		itsByNodePool[nodePool.Name] = instanceTypes
	}

	b.ReportAllocs()
	for b.Loop() {
		if domainGroups := buildDomainGroups(nodePools, itsByNodePool); len(domainGroups) == 0 {
			b.Fatal("expected domain groups to be built")
		}
	}
}

func benchDomainGroupNodePools(count int) []*v1.NodePool {
	nodePools := make([]*v1.NodePool, 0, count)
	for i := range count {
		nodePools = append(nodePools, &v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("bench-np-%d", i)},
			Spec: v1.NodePoolSpec{
				Template: v1.NodeClaimTemplate{
					ObjectMeta: v1.ObjectMeta{Labels: map[string]string{
						"bench.example.com/pool": fmt.Sprintf("pool-%d", i),
						"bench.example.com/tier": fmt.Sprintf("tier-%d", i%3),
					}},
					Spec: v1.NodeClaimTemplateSpec{
						Requirements: []v1.NodeSelectorRequirementWithMinValues{
							{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{fmt.Sprintf("test-zone-%d", i%3+1)}},
							{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand, v1.CapacityTypeSpot}},
							{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
							{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux)}},
							{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpExists},
							{Key: "bench.example.com/generation", Operator: corev1.NodeSelectorOpIn, Values: []string{"gen-1", "gen-2", "gen-3"}, MinValues: new(2)},
						},
						Taints: []corev1.Taint{{
							Key:    fmt.Sprintf("bench.example.com/pool-%d", i),
							Value:  "true",
							Effect: corev1.TaintEffectNoSchedule,
						}},
					},
				},
			},
		})
	}
	return nodePools
}
