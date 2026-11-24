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

// Question: How fast do N pending pods become Ready, and how many nodes are left once demand drops?
//
// The baseline every change touches: plain deployments of two shapes on one NodePool, scaled out, then 30% scaled in
// so consolidation removes the extra nodes. Variants in this group change the workload or the consolidation policy.
var Pods = perf.Scenario{
	Name:     "pods",
	Question: "How fast do N pending pods become Ready, and how many nodes are left once demand drops?",
	Summary:  "Provisions then consolidates N pods of two shapes (900m/3100Mi and 3500m/28Gi) on one NodePool, the baseline every change touches.",
	Vectors:  map[perf.Vector]int{perf.VectorPods: 1000},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "N pending pods until all are healthy: provisioning throughput (batching, scheduler Solve, NodeClaim launch)."},
		{Name: "consolidation", Summary: "Scale to 70% until nodes stop being disrupted: disruption evaluation and SimulateScheduling cost."},
	},
	Run: func(r *perf.Run) {
		half := r.Size(perf.VectorPods) / 2
		small := r.Deployment("small", half, "900m", "3100Mi")
		large := r.Deployment("large", half, "3500m", "28Gi")

		r.Create(r.NodeClass, r.NodePool)
		r.AwaitNodePoolsReady(5 * time.Minute)
		scaleOut := r.StartPhase("scaleOut")
		r.Create(small, large)
		scaleOut.AwaitScaleOut(r.Replicas(), 15*time.Minute)

		consolidation := r.StartPhase("consolidation")
		from := r.Replicas()
		r.Scale(half*7/10, small, large)
		consolidation.AwaitConsolidation(from, r.Replicas(), 40*time.Minute)
	},
}
