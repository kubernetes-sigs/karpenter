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

package scenarios

import (
	"fmt"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

const (
	diverseTaint = "perf.karpenter.sh/dedicated"
	diversePool  = "perf.example.com/diverse-pool" // template labels can't use the restricted karpenter.sh domain
)

var diverseToleration = corev1.Toleration{Key: diverseTaint, Operator: corev1.TolerationOpExists}

// diverseNodePools builds n NodePools that no two collapse to the same shape: a rotating capacity type, weight and
// template label everywhere, plus whichever of zone, architecture and instance size ceiling the provider's Catalog
// names. Every third pool carries a taint the scenario's pods tolerate.
func buildDiverseNodePools(r *perf.Run, n int) []*v1.NodePool {
	cat := r.Provider.Catalog
	capacityTypes := [][]string{{v1.CapacityTypeSpot}, {v1.CapacityTypeOnDemand}, {v1.CapacityTypeSpot, v1.CapacityTypeOnDemand}}
	pools := make([]*v1.NodePool, 0, n)
	for i := range n {
		np := r.NodePool.DeepCopy()
		np.Name = fmt.Sprintf("%s-%d", r.NodePool.Name, i)
		np.Spec.Weight = new(int32(1 + i%50)) //nolint:gosec // i%50 fits
		np.Spec.Template.Labels = lo.Assign(np.Spec.Template.Labels, map[string]string{diversePool: r.Scenario.Name, "perf.example.com/pool-index": fmt.Sprint(i)})
		reqs := []v1.NodeSelectorRequirementWithMinValues{{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: capacityTypes[i%len(capacityTypes)]}}
		if len(cat.Zones) > 0 {
			reqs = append(reqs, v1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: cat.Zones[i%len(cat.Zones)]})
		}
		if len(cat.Arches) > 0 {
			reqs = append(reqs, v1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{cat.Arches[i%len(cat.Arches)]}})
		}
		if cat.SizeKey != "" && len(cat.SizeCeilings) > 0 {
			reqs = append(reqs, v1.NodeSelectorRequirementWithMinValues{Key: cat.SizeKey, Operator: corev1.NodeSelectorOpLt, Values: []string{cat.SizeCeilings[i%len(cat.SizeCeilings)]}})
		}
		test.ReplaceRequirements(np, reqs...)
		if i%3 == 0 {
			np.Spec.Template.Spec.Taints = append(np.Spec.Template.Spec.Taints, corev1.Taint{Key: diverseTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule})
		}
		pools = append(pools, np)
	}
	return pools
}

// Question: What does each NodePool cost when no two NodePools look alike?
//
// Identical NodePools share work; real ones differ in capacity type, weight, zone, arch, size and taints. perfv1's
// Many Diverse NodePools: 500 such pools and a few pods that can use any of them.
var NodePoolsDiverse = perf.Scenario{
	Name:     "nodepoolsDiverse",
	Group:    "nodepools",
	Question: "What does each NodePool cost when no two NodePools look alike?",
	Summary:  "N diverse NodePools (capacity type, weight, a third tainted, plus the zone, arch and size ceiling in the provider's Catalog) and a few zonally spread 50m/128Mi pods that can use any of them: per-NodePool cost in cluster state, cost tracking and scheduling, where nodepools has one usable pool.",
	Vectors:  map[perf.Vector]int{perf.VectorNodePools: 500, perf.VectorPods: 100},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "Pods across the diverse NodePools: NodePool selection and topology/taint bookkeeping over every pool."},
		{Name: "consolidation", Summary: "Scale to 40% until nodes stop being disrupted: disruption and cost recompute across every NodePool."},
	},
	Run: func(r *perf.Run) {
		pods := r.Size(perf.VectorPods)
		r.Create(r.NodeClass)
		r.CreateParallel(objects(buildDiverseNodePools(r, r.Size(perf.VectorNodePools))))
		r.AwaitNodePoolsReady(15 * time.Minute)
		app := r.Deployment("app", pods, "50m", "128Mi", test.WithNodeSelector(map[string]string{diversePool: r.Scenario.Name}),
			test.WithTolerations([]corev1.Toleration{diverseToleration}), zoneSpread())

		scaleOut := r.StartPhase("scaleOut")
		r.Create(app)
		scaleOut.AwaitScaleOut(r.Replicas(), 10*time.Minute)

		consolidation := r.StartPhase("consolidation")
		from := r.Replicas()
		r.Scale(pods*2/5, app)
		consolidation.AwaitConsolidation(from, r.Replicas(), 15*time.Minute)
	},
}
