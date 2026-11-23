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

// Question: How fast do pods with hard spread and anti-affinity constraints become Ready?
//
// Pods with hostname anti-affinity, zonal spread and hostname spread, the constraints that make scheduling track
// topology domains. Variants change the mix or the size.
var Topology = perf.Scenario{
	Name:     "topology",
	Question: "How fast do pods with hard spread and anti-affinity constraints become Ready?",
	Summary:  "N pods with hard scheduling constraints: 10% hostname anti-affinity, 45% zonal topology spread, 45% hostname topology spread. Exercises topology domain tracking, the code path pods and nodepools don't touch.",
	Vectors:  map[perf.Vector]int{perf.VectorPods: 1000},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "Constrained pods until all are healthy: topology groups, domain counting and anti-affinity search (pkg/controllers/provisioning/scheduling/topology*.go)."},
	},
	Run: func(r *perf.Run) {
		n := r.Size(perf.VectorPods)
		own := ownPool(r)
		anti := r.Deployment("anti", n/10, "900m", "3100Mi", own, test.WithPodAntiAffinityHostname())
		zone := r.Deployment("zone", n*45/100, "900m", "3100Mi", own, zoneSpread())
		host := r.Deployment("host", n-n/10-n*45/100, "900m", "3100Mi", own, test.WithHostnameSpread())

		r.Create(r.NodeClass, r.NodePool)
		r.AwaitNodePoolsReady(5 * time.Minute)
		scaleOut := r.StartPhase("scaleOut")
		r.Create(anti, zone, host)
		scaleOut.AwaitScaleOut(r.Replicas(), 20*time.Minute)
	},
}
