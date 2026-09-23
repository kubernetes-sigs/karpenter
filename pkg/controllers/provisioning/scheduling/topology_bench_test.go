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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/clock"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	corescheduling "sigs.k8s.io/karpenter/pkg/scheduling"
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

	// Domain-filtering sweeps (#2227): every pod carries a zone topology spread constraint with Honor inclusion
	// policies and selects a subset of the NodePools by label and toleration, so building each pod's topology group
	// evaluates NodePool requirement/taint compatibility instead of skipping the filters. NewTopology is the unit
	// that runs once per scheduling loop: it builds the domain groups from every NodePool and instance type and then
	// constructs each pod's topology group, so domain tracking and per-pod domain filtering are measured together.
	// The reverted attempts at domain filtering regressed along the NodePool axis (#2779 memory, #2954 CPU) and the
	// single-NodePool suites didn't catch it; each sweep below scales one axis of that blind spot while holding the
	// baseline scenario fixed.
	base := filteringScenario{nodePools: 50, taintGroups: 5, instanceTypes: 400, zones: 3, pods: 1000}
	for _, np := range []int{1, 10, 50, 100, 200} {
		s := base
		s.nodePools = np
		b.Run(fmt.Sprintf("vector=filteringnodepools/np=%d", np), func(b *testing.B) { benchmarkFilteringTopology(b, s) })
	}
	for _, it := range []int{100, 400, 1000} {
		s := base
		s.instanceTypes = it
		b.Run(fmt.Sprintf("vector=filteringinstancetypes/it=%d", it), func(b *testing.B) { benchmarkFilteringTopology(b, s) })
	}
	for _, z := range []int{3, 10, 50} {
		s := base
		s.zones = z
		b.Run(fmt.Sprintf("vector=filteringzones/z=%d", z), func(b *testing.B) { benchmarkFilteringTopology(b, s) })
	}
	for _, tg := range []int{1, 5, 20} {
		s := base
		s.taintGroups = tg
		b.Run(fmt.Sprintf("vector=filteringtaintgroups/tg=%d", tg), func(b *testing.B) { benchmarkFilteringTopology(b, s) })
	}
}

// filteringScenario is one knob per scaling vector of the domain-filtering benchmarks; a sweep scales one field and
// holds the rest fixed. taintGroups is the number of distinct team identities: each NodePool carries its team's
// label and dedicated taint, and each pod selects and tolerates exactly one team.
type filteringScenario struct {
	nodePools     int
	taintGroups   int
	instanceTypes int
	zones         int
	pods          int
}

func benchmarkFilteringTopology(b *testing.B, s filteringScenario) {
	ctx := benchCtx()
	pods := benchFilteringSpreadPods(s)

	cp := fake.NewCloudProvider()
	instanceTypes := benchZonedInstanceTypes(s)
	cp.InstanceTypes = instanceTypes

	client := fakecr.NewFakeClient()
	clk := &clock.RealClock{}
	cl := state.NewCluster(clk, client, cp)

	nodePools := benchFilteringNodePools(s)
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
}

func benchZone(z int) string {
	return fmt.Sprintf("test-zone-%d", z)
}

// benchZonedInstanceTypes builds instance types whose offerings span every zone in the scenario, so the domain
// universe scales with the zones vector.
func benchZonedInstanceTypes(s filteringScenario) []*cloudprovider.InstanceType {
	its := make([]*cloudprovider.InstanceType, s.instanceTypes)
	for i := range its {
		resources := corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", i%64+1)),
			corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", (i%64+1)*2)),
			corev1.ResourcePods:   resource.MustParse(fmt.Sprintf("%d", (i%64+1)*10)),
		}
		offerings := make([]cloudprovider.Offering, 0, s.zones)
		for z := range s.zones {
			offerings = append(offerings, cloudprovider.Offering{
				Available: true,
				Requirements: corescheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
					corev1.LabelTopologyZone: benchZone(z),
				}),
				Price: fake.PriceFromResources(resources),
			})
		}
		its[i] = fake.NewInstanceType(fmt.Sprintf("fake-it-%d", i), fake.WithResources(resources), fake.WithOfferings(offerings...))
	}
	return its
}

// benchFilteringNodePools builds NodePools dedicated to one of the scenario's teams: a team label, a matching
// dedicated taint, and a rotating zone subset (one zone, the trailing two thirds, or all zones), so a pod selecting
// its team's NodePools is compatible with 1/taintGroups of them and the counted domains differ per team.
func benchFilteringNodePools(s filteringScenario) []*v1.NodePool {
	nps := make([]*v1.NodePool, s.nodePools)
	for i := range nps {
		var zones []string
		switch i % 3 {
		case 0:
			zones = []string{benchZone(i % s.zones)}
		case 1:
			for z := s.zones / 3; z < s.zones; z++ {
				zones = append(zones, benchZone(z))
			}
		case 2:
			for z := range s.zones {
				zones = append(zones, benchZone(z))
			}
		}
		team := fmt.Sprintf("team-%d", i%s.taintGroups)
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
							Values:   zones,
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
func benchFilteringSpreadPods(s filteringScenario) []*corev1.Pod {
	pods := make([]*corev1.Pod, s.pods)
	for i := range pods {
		team := fmt.Sprintf("team-%d", i%s.taintGroups)
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
