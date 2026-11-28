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

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

// Question: Does Balanced score moves per NodePool when NodePools use different instance families?
//
// NodePools with different instance families and densities must be scored on their own. perfv1's Balanced
// Heterogeneous: a dense and a sparse workload on two family-restricted NodePools, with the same score check.
var PodsBalancedHeterogeneous = perf.Scenario{
	Name:     "podsBalancedHeterogeneous",
	Group:    "pods",
	Question: "Does Balanced score moves per NodePool when NodePools use different instance families?",
	Summary:  "Two Balanced NodePools restricted to the first two Catalog instance families (c and m on KWOK), a dense workload (300 per 400, 500m/1Gi) on the first and a sparse one (100 per 400, 2500m/8Gi) on the second, scaled to 60%: Balanced scoring per NodePool.",
	Vectors:  map[perf.Vector]int{perf.VectorPods: 400},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "Both workloads until all pods are healthy."},
		{Name: "consolidation", Summary: "Scale both to 60% until nodes stop being disrupted; every scored move must belong to one of the two NodePools."},
	},
	Requires: func(p *perf.Provider) error {
		if p.Catalog.FamilyKey == "" || len(p.Catalog.Families) < 2 {
			return fmt.Errorf("needs Catalog.FamilyKey and two Catalog.Families")
		}
		return nil
	},
	Run: func(r *perf.Run) {
		balancedPool(r)
		cat := r.Provider.Catalog
		pool := func(family string) *v1.NodePool {
			np := r.NodePool.DeepCopy()
			np.Name = fmt.Sprintf("%s-%s", r.NodePool.Name, family)
			test.ReplaceRequirements(np, v1.NodeSelectorRequirementWithMinValues{Key: cat.FamilyKey, Operator: corev1.NodeSelectorOpIn, Values: []string{family}})
			return np
		}
		c, m := pool(cat.Families[0]), pool(cat.Families[1])
		n := r.Size(perf.VectorPods)
		denseN, sparseN := n*3/4, n-n*3/4
		dense := r.Deployment("dense", denseN, "500m", "1Gi", test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: c.Name}))
		sparse := r.Deployment("sparse", sparseN, "2500m", "8Gi", test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: m.Name}))

		r.Create(r.NodeClass, c, m)
		r.AwaitNodePoolsReady(5 * time.Minute)
		scaleOut := r.StartPhase("scaleOut")
		r.Create(dense, sparse)
		scaleOut.AwaitScaleOut(r.Replicas(), 15*time.Minute)

		consolidation := r.StartPhase("consolidation")
		from := r.Replicas()
		r.Scale(denseN*3/5, dense)
		r.Scale(sparseN*3/5, sparse)
		pools := expectBalancedDecisions(consolidation.AwaitConsolidation(from, r.Replicas(), 25*time.Minute))
		Expect(pools.Difference(sets.New(c.Name, m.Name)).UnsortedList()).To(BeEmpty(), "Balanced scored a move against a NodePool this scenario didn't create")
	},
}
