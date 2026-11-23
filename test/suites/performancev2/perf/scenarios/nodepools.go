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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

const tenantTaint = "perf.karpenter.sh/tenant"

// Question: What does each NodePool I add cost in scheduling latency, controller memory and startup time?
//
// Platform teams give each tenant its own NodePool. Here every NodePool but one is closed to the pods by a taint, so
// each scheduling pass pays for N-1 NodePools it can't use. Variants make the NodePools differ or all receive pods.
var NodePools = perf.Scenario{
	Name:     "nodepools",
	Question: "What does each NodePool I add cost in scheduling latency, controller memory and startup time?",
	Summary:  "N NodePools where all but one carry a tenant NoSchedule taint the pods don't tolerate, so every pass rules out N-1 NodePools (the multi-tenant platform pattern).",
	Vectors:  map[perf.Vector]int{perf.VectorNodePools: 1000, perf.VectorPods: 100},
	Sweep:    map[perf.Vector][]int{perf.VectorNodePools: {1, 10, 100, 1000, 10000}, perf.VectorPods: {100, 1000, 5000}},
	Phases: []perf.PhaseSpec{
		{Name: "scheduling", Summary: "Pods onto the one open NodePool: per-NodePool cost in NewScheduler and Solve (templates, instance type filtering, topology domain groups). Find the function with BenchmarkNewScheduler/vector=nodepools."},
		{Name: "startup", Summary: "Restart the controller with the NodePools and nodes present: startup cost (cluster state sync, offerings, cost tracking, NodeOverlay store)."},
	},
	Run: func(r *perf.Run) {
		n, pods := r.Size(perf.VectorNodePools), r.Size(perf.VectorPods)
		r.Create(r.NodeClass)
		pools := make([]client.Object, 0, n)
		for i := range n {
			np := r.NodePool.DeepCopy()
			np.Name = fmt.Sprintf("%s-%d", r.NodePool.Name, i)
			if i > 0 {
				np.Spec.Template.Spec.Taints = append(np.Spec.Template.Spec.Taints, corev1.Taint{
					Key: tenantTaint, Value: fmt.Sprint(i), Effect: corev1.TaintEffectNoSchedule,
				})
			}
			pools = append(pools, np)
		}
		r.CreateParallel(pools)
		r.AwaitNodePoolsReady(15 * time.Minute)
		app := r.Deployment("app", pods, "1", "2Gi")

		scheduling := r.StartPhase("scheduling")
		r.Create(app)
		scheduling.AwaitScaleOut(r.Replicas(), time.Hour)

		r.Restart("startup", 20*time.Minute)
	},
}
