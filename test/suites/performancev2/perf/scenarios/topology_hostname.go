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

	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

// Question: How fast do hostname-spread pods schedule and consolidate as the cluster grows?
//
// Hostname spread adds a topology domain with every node, so its cost grows with the cluster. perfv1's Host Name
// Spreading Reg and XL (kwok runs pods=2000): half spread 900m/3100Mi, half large 3500m/28Gi, scaled to 70%.
var TopologyHostname = perf.Scenario{
	Name:     "topologyHostname",
	Group:    "topology",
	Question: "How fast do hostname-spread pods schedule and consolidate as the cluster grows?",
	Summary:  "N pods, half with hostname topology spread (900m/3100Mi) and half large (3500m/28Gi), scaled to 70%: hostname domains grow with every node, so spread cost grows with the cluster.",
	Vectors:  map[perf.Vector]int{perf.VectorPods: 1000},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "N pending pods until all are healthy: hostname domain counting in Solve as nodes are added."},
		{Name: "consolidation", Summary: "Scale to 70% until nodes stop being disrupted: SimulateScheduling with hostname spread across every remaining node."},
	},
	Run: func(r *perf.Run) {
		half := r.Size(perf.VectorPods) / 2
		spread := r.Deployment("spread", half, "900m", "3100Mi", ownPool(r), test.WithHostnameSpread())
		large := r.Deployment("large", half, "3500m", "28Gi", ownPool(r))

		r.Create(r.NodeClass, r.NodePool)
		r.AwaitNodePoolsReady(5 * time.Minute)
		scaleOut := r.StartPhase("scaleOut")
		r.Create(spread, large)
		scaleOut.AwaitScaleOut(r.Replicas(), 25*time.Minute)

		consolidation := r.StartPhase("consolidation")
		from := r.Replicas()
		r.Scale(half*7/10, spread, large)
		consolidation.AwaitConsolidation(from, r.Replicas(), 30*time.Minute)
	},
}
