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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

// Question: What does it cost when many NodePools each need a node at the same time?
//
// Tenants pin workloads to their own NodePool, so many pools provision at once. perfv1's Many NodePools Targeted:
// one single-pod deployment pinned to each of 150 of 500 diverse pools, then half zeroed.
var NodePoolsTargeted = perf.Scenario{
	Name:     "nodepoolsTargeted",
	Group:    "nodepools",
	Question: "What does it cost when many NodePools each need a node at the same time?",
	Summary:  "N diverse NodePools, with one single-pod 50m/128Mi deployment pinned (karpenter.sh/nodepool selector) to each of the first P: many pools provision at once, one node each.",
	Vectors:  map[perf.Vector]int{perf.VectorNodePools: 500, perf.VectorPods: 150},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "P pinned pods, about one node per targeted NodePool: per-NodePool provisioning and cost tracking population."},
		{Name: "consolidation", Summary: "Zero half the deployments until their nodes are gone: emptiness across many NodePools."},
	},
	Run: func(r *perf.Run) {
		targets := r.Size(perf.VectorPods)
		pools := buildDiverseNodePools(r, r.Size(perf.VectorNodePools))
		r.Create(r.NodeClass)
		r.CreateParallel(objects(pools))
		r.AwaitNodePoolsReady(15 * time.Minute)
		deps := make([]*appsv1.Deployment, 0, targets)
		for i := range targets {
			deps = append(deps, r.Deployment(fmt.Sprintf("t%03d", i), 1, "50m", "128Mi",
				test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: pools[i%len(pools)].Name}),
				test.WithTolerations([]corev1.Toleration{diverseToleration})))
		}

		scaleOut := r.StartPhase("scaleOut")
		r.CreateParallel(objects(deps))
		scaleOut.AwaitScaleOut(r.Replicas(), 15*time.Minute)

		consolidation := r.StartPhase("consolidation")
		for _, d := range deps[targets/2:] {
			r.Scale(0, d)
		}
		consolidation.AwaitConsolidation(targets, r.Replicas(), 20*time.Minute)
	},
}
