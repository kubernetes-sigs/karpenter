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
	"time"

	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

// Question: Does the Balanced consolidation policy only make moves worth their disruption?
//
// Balanced scores each consolidation move by savings against disruption and takes it only above 1/BalancedK. The
// pods workload under Balanced; the karpenter_consolidation_score histogram must respect the threshold (an assertion,
// not a bound). perfv1's Balanced Basic.
var PodsBalanced = perf.Scenario{
	Name:     "podsBalanced",
	Group:    "pods",
	Question: "Does the Balanced consolidation policy only make moves worth their disruption?",
	Summary:  "The pods workload (half 900m/3100Mi, half 3500m/28Gi, scaled to 70%) under consolidationPolicy Balanced, whose every scored move must respect the 1/BalancedK threshold.",
	Vectors:  map[perf.Vector]int{perf.VectorPods: 1000},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "N pending pods until all are healthy."},
		{Name: "consolidation", Summary: "Scale to 70% until nodes stop being disrupted: Balanced move scoring on top of consolidation."},
	},
	Run: func(r *perf.Run) {
		balancedPool(r)
		half := r.Size(perf.VectorPods) / 2
		small := r.Deployment("small", half, "900m", "3100Mi", ownPool(r))
		large := r.Deployment("large", half, "3500m", "28Gi", ownPool(r))

		r.Create(r.NodeClass, r.NodePool)
		r.AwaitNodePoolsReady(5 * time.Minute)
		scaleOut := r.StartPhase("scaleOut")
		r.Create(small, large)
		scaleOut.AwaitScaleOut(r.Replicas(), 15*time.Minute)

		consolidation := r.StartPhase("consolidation")
		from := r.Replicas()
		r.Scale(half*7/10, small, large)
		expectBalancedDecisions(consolidation.AwaitConsolidation(from, r.Replicas(), 20*time.Minute))
	},
}
