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

// Question: Does Balanced keep making only worthwhile moves while demand goes up and down?
//
// Demand that rises and falls makes nodes come and go between consolidation passes. perfv1's Balanced Churn Chain:
// 400 pods scaled to 200, 400 and 200 again, with the same score check.
var PodsBalancedChurn = perf.Scenario{
	Name:     "podsBalancedChurn",
	Group:    "pods",
	Question: "Does Balanced keep making only worthwhile moves while demand goes up and down?",
	Summary:  "N 900m/3100Mi pods under Balanced, then halved, restored and halved again inside one phase: Balanced scoring while the workload keeps moving.",
	Vectors:  map[perf.Vector]int{perf.VectorPods: 400},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "N pending pods until all are healthy."},
		{Name: "consolidation", Summary: "Three rounds (N/2, N, N/2), each settled for twice consolidateAfter, then until nodes stop being disrupted."},
	},
	Run: func(r *perf.Run) {
		balancedPool(r)
		n := r.Size(perf.VectorPods)
		app := r.Deployment("app", n, "900m", "3100Mi", ownPool(r))

		r.Create(r.NodeClass, r.NodePool)
		r.AwaitNodePoolsReady(5 * time.Minute)
		scaleOut := r.StartPhase("scaleOut")
		r.Create(app)
		scaleOut.AwaitScaleOut(r.Replicas(), 15*time.Minute)

		consolidation := r.StartPhase("consolidation")
		settle := 2 * time.Minute
		if d := r.NodePool.Spec.Disruption.ConsolidateAfter.Duration; d != nil {
			settle = 2 * *d
		}
		for _, replicas := range []int{n / 2, n, n / 2} {
			r.Scale(replicas, app)
			r.AwaitHealthyPods(r.Replicas(), 10*time.Minute)
			time.Sleep(settle)
		}
		expectBalancedDecisions(consolidation.AwaitConsolidation(n, r.Replicas(), 20*time.Minute))
	},
}
