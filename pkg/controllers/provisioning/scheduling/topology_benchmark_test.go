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
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"
)

// benchTopologyFixtures returns nodePools and instanceTypes fixtures scaled by NodePool count. Each NodePool carries a
// distinct taint and a couple of custom labels to exercise the per-NodePool tracking in the domain groups. The CPU
// regression which caused PR #2671 to be reverted (#2954) scaled with the number of NodePools, which the standard
// scheduling benchmarks do not cover (they use a single NodePool).
func benchTopologyFixtures(nodePoolCount int) ([]*v1.NodePool, map[string][]*cloudprovider.InstanceType) {
	its := fake.InstanceTypes(400)
	nodePools := make([]*v1.NodePool, 0, nodePoolCount)
	instanceTypes := map[string][]*cloudprovider.InstanceType{}
	for i := range nodePoolCount {
		np := test.NodePool(v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("nodepool-%d", i)},
			Spec: v1.NodePoolSpec{
				Template: v1.NodeClaimTemplate{
					ObjectMeta: v1.ObjectMeta{
						Labels: map[string]string{
							"team":       fmt.Sprintf("team-%d", i%5),
							"pool-index": fmt.Sprintf("%d", i),
						},
					},
					Spec: v1.NodeClaimTemplateSpec{
						Taints: []corev1.Taint{{Key: fmt.Sprintf("dedicated-%d", i), Value: "true", Effect: corev1.TaintEffectNoSchedule}},
					},
				},
			},
		})
		nodePools = append(nodePools, np)
		instanceTypes[np.Name] = its
	}
	return nodePools, instanceTypes
}

// BenchmarkBuildDomainGroups measures domain group construction, which runs on every scheduling loop for both
// provisioning and consolidation.
func BenchmarkBuildDomainGroups(b *testing.B) {
	for _, nodePoolCount := range []int{1, 10, 50, 100} {
		b.Run(fmt.Sprintf("nodepools-%d", nodePoolCount), func(b *testing.B) {
			nodePools, instanceTypes := benchTopologyFixtures(nodePoolCount)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				buildDomainGroups(nodePools, instanceTypes)
			}
		})
	}
}

// BenchmarkNewTopologyGroup measures topology group construction for a pod with a zone topology spread constraint and
// a nodeSelector, which exercises the per-pod domain filtering path.
func BenchmarkNewTopologyGroup(b *testing.B) {
	for _, nodePoolCount := range []int{1, 10, 50, 100} {
		b.Run(fmt.Sprintf("nodepools-%d", nodePoolCount), func(b *testing.B) {
			nodePools, instanceTypes := benchTopologyFixtures(nodePoolCount)
			domainGroups := buildDomainGroups(nodePools, instanceTypes)
			pod := test.Pod(test.PodOptions{
				ObjectMeta:   metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				NodeSelector: map[string]string{"team": "team-0"},
			})
			honor := corev1.NodeInclusionPolicyHonor
			selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				NewTopologyGroup(TopologyTypeSpread, corev1.LabelTopologyZone, pod, sets.New("default"), selector, 1, nil, &honor, &honor, domainGroups[corev1.LabelTopologyZone])
			}
		})
	}
}
