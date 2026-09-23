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

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/clock"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
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

	// Domain-filtering sweep (#2227): every pod carries a zone topology spread constraint with Honor inclusion
	// policies and selects a subset of the NodePools by label and toleration, so building each pod's topology group
	// evaluates NodePool requirement/taint compatibility instead of skipping the filters. NewTopology is the unit
	// that runs once per scheduling loop: it builds the domain groups from every NodePool and instance type and then
	// constructs each pod's topology group, so domain tracking and per-pod domain filtering are measured together.
	// The reverted attempts at domain filtering regressed along the NodePool axis (#2779 memory, #2954 CPU) and the
	// single-NodePool suites didn't catch it, which is why this sweep extends to 100.
	for _, np := range []int{1, 10, 50, 100} {
		b.Run(fmt.Sprintf("vector=filteringnodepools/np=%d", np), func(b *testing.B) {
			ctx := benchCtx()
			pods := benchFilteringSpreadPods(1000)

			cp := fake.NewCloudProvider()
			instanceTypes := fake.InstanceTypes(400)
			cp.InstanceTypes = instanceTypes

			client := fakecr.NewFakeClient()
			clk := &clock.RealClock{}
			cl := state.NewCluster(clk, client, cp)

			nodePools := benchFilteringNodePools(np)
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

// benchFilteringNodePools builds NodePools dedicated to one of five teams: a team label, a matching dedicated taint,
// and a team-dependent zone subset, so a pod selecting its team's NodePools is compatible with a fifth of them and
// the counted domains differ per team.
func benchFilteringNodePools(count int) []*v1.NodePool {
	zoneSets := [][]string{
		{"test-zone-1"},
		{"test-zone-2", "test-zone-3"},
		{"test-zone-1", "test-zone-2", "test-zone-3"},
	}
	nps := make([]*v1.NodePool, count)
	for i := range nps {
		team := fmt.Sprintf("team-%d", i%5)
		nps[i] = test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Template: v1.NodeClaimTemplate{
					ObjectMeta: v1.ObjectMeta{
						Labels: map[string]string{"team": team},
					},
					Spec: v1.NodeClaimTemplateSpec{
						Taints: []corev1.Taint{{
							Key:    "dedicated",
							Value:  team,
							Effect: corev1.TaintEffectNoSchedule,
						}},
						Requirements: []v1.NodeSelectorRequirementWithMinValues{{
							Key:      corev1.LabelTopologyZone,
							Operator: corev1.NodeSelectorOpIn,
							Values:   zoneSets[i%len(zoneSets)],
						}},
					},
				},
			},
		})
	}
	return nps
}

// benchFilteringSpreadPods builds pods which select their team's NodePools by label and toleration, each with a zone
// topology spread constraint whose Honor policies make domain filtering evaluate NodePool compatibility.
func benchFilteringSpreadPods(count int) []*corev1.Pod {
	pods := make([]*corev1.Pod, count)
	for i := range pods {
		team := fmt.Sprintf("team-%d", i%5)
		pods[i] = test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": team},
				UID:    uuid.NewUUID(), // set the UUID so the cached data is properly stored in the scheduler
			},
			NodeSelector: map[string]string{"team": team},
			Tolerations: []corev1.Toleration{{
				Key:      "dedicated",
				Operator: corev1.TolerationOpEqual,
				Value:    team,
				Effect:   corev1.TaintEffectNoSchedule,
			}},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				TopologyKey:        corev1.LabelTopologyZone,
				WhenUnsatisfiable:  corev1.DoNotSchedule,
				MaxSkew:            1,
				LabelSelector:      &metav1.LabelSelector{MatchLabels: map[string]string{"app": team}},
				NodeTaintsPolicy:   lo.ToPtr(corev1.NodeInclusionPolicyHonor),
				NodeAffinityPolicy: lo.ToPtr(corev1.NodeInclusionPolicyHonor),
			}},
		})
	}
	return pods
}
