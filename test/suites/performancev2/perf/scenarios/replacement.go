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
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

// Question: How long does rolling N nodes take, and how much capacity do I lose meanwhile?
//
// A NodePool template, AMI or Kubernetes version change replaces the whole fleet. A slow rollout delays the fix;
// pods down during it are lost capacity. Here a NodePool template annotation drifts every node under half
// hostname-spread, half large pods (perfv1's Drift Performance). Variants change what triggers the replacement or
// what must survive it.
var Replacement = perf.Scenario{
	Name:     "replacement",
	Question: "How long does rolling N nodes take, and how much capacity do I lose meanwhile?",
	Summary:  "N pods, half hostname-spread (900m/3100Mi) and half large (3500m/28Gi), then a NodePool template change drifts every node: drift detection, replacement launch and drain throughput.",
	Vectors:  map[perf.Vector]int{perf.VectorPods: 600},
	Phases: []perf.PhaseSpec{
		{Name: "initial", Summary: "N pending pods until all are healthy, the fleet the drift replaces."},
		{Name: "drift", Summary: "Annotate the NodePool template until every original node is replaced and the pods are healthy again: drift detection (pkg/controllers/nodeclaim/disruption), replacement NodeClaims and eviction (pkg/controllers/disruption, node/termination)."},
	},
	Run: func(r *perf.Run) {
		n := r.Size(perf.VectorPods)
		spread := r.Deployment("spread", n/2, "900m", "3100Mi", ownPool(r), test.WithHostnameSpread())
		large := r.Deployment("large", n-n/2, "3500m", "28Gi", ownPool(r))

		r.Create(r.NodeClass, r.NodePool)
		r.AwaitNodePoolsReady(5 * time.Minute)
		initial := r.StartPhase("initial")
		r.Create(spread, large)
		initial.AwaitScaleOut(r.Replicas(), 20*time.Minute)

		r.StartPhase("drift").AwaitReplacement(r.Replicas(), 45*time.Minute, func() { driftNodePool(r) })
	},
}

// Question: Do pods marked karpenter.sh/do-not-disrupt survive consolidation and a fleet rollout?
//
// Batch jobs and singletons opt out of voluntary disruption. Consolidation and drift must route around their nodes
// while still doing their work. perfv1's Do Not Disrupt checked consolidation; this also drifts the fleet.
var ReplacementDoNotDisrupt = perf.Scenario{
	Name:     "replacementDoNotDisrupt",
	Group:    "replacement",
	Question: "Do pods marked karpenter.sh/do-not-disrupt survive consolidation and a fleet rollout?",
	Summary:  "N pods: 5/11 hostname-spread (900m/3100Mi), 5/11 large (3500m/28Gi) and 1/11 with karpenter.sh/do-not-disrupt (900m/450Mi). Halving the first two consolidates around the protected nodes, then a NodePool template change drifts every other node.",
	Vectors:  map[perf.Vector]int{perf.VectorPods: 1100},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "N pending pods until all are healthy."},
		{Name: "consolidation", Summary: "Halve the unprotected deployments until nodes stop being disrupted: candidate filtering on do-not-disrupt pods (pkg/controllers/disruption). Nodes must drop, and every node hosting a protected pod must survive."},
		{Name: "drift", Summary: "Drift the NodePool: every node without a protected pod is replaced, every node with one survives, and no protected pod restarts."},
	},
	Run: func(r *perf.Run) {
		n := r.Size(perf.VectorPods)
		protectedN := frac(n, 1, 11)
		each := (n - protectedN) / 2
		spread := r.Deployment("spread", each, "900m", "3100Mi", ownPool(r), test.WithHostnameSpread())
		large := r.Deployment("large", each, "3500m", "28Gi", ownPool(r))
		protected := r.Deployment("protected", protectedN, "900m", "450Mi", ownPool(r), test.WithDoNotDisrupt())

		r.Create(r.NodeClass, r.NodePool)
		r.AwaitNodePoolsReady(5 * time.Minute)
		scaleOut := r.StartPhase("scaleOut")
		r.Create(spread, large, protected)
		scaleOut.AwaitScaleOut(r.Replicas(), 15*time.Minute)

		protectedPods, protectedNodes := sets.New[string](), sets.New[string]()
		for _, p := range r.Pods() {
			if p.Labels["app"] == protected.Name {
				protectedPods.Insert(string(p.UID))
				protectedNodes.Insert(p.Spec.NodeName)
			}
		}
		before := len(r.Nodes())
		consolidation := r.StartPhase("consolidation")
		from := r.Replicas()
		r.Scale(each/2, spread, large)
		if consolidation.AwaitConsolidation(from, r.Replicas(), 25*time.Minute) == nil {
			return
		}
		after := sets.New[string]()
		for _, node := range r.Nodes() {
			after.Insert(node.Name)
		}
		Expect(protectedNodes.Difference(after).UnsortedList()).To(BeEmpty(), "nodes hosting do-not-disrupt pods were consolidated")
		Expect(after.Len()).To(BeNumerically("<", before), "consolidation removed no nodes, so it never had to route around the protected ones")

		r.StartPhase("drift").AwaitReplacement(r.Replicas(), 45*time.Minute, func() { driftNodePool(r) }, protectedNodes.UnsortedList()...)
		for _, p := range r.Pods() {
			if p.Labels["app"] == protected.Name {
				Expect(protectedPods.Has(string(p.UID))).To(BeTrue(), "do-not-disrupt pod %s was replaced during the rollout", p.Name)
			}
		}
	},
}

// driftNodePool changes the run NodePool's template, which drifts every node it owns.
func driftNodePool(r *perf.Run) {
	r.Patch(r.NodePool, func() {
		if r.NodePool.Spec.Template.Annotations == nil {
			r.NodePool.Spec.Template.Annotations = map[string]string{}
		}
		r.NodePool.Spec.Template.Annotations["perf.karpenter.sh/drift"] = fmt.Sprint(time.Now().UnixNano())
	})
}
