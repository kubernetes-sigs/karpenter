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

	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

// wideShapes are perfv1's 30 fixed wide-deployment shapes. Pod counts are per 1000 pods; the first 20 deployments get
// 35 and the last 10 get 30, then 25 and 20 after scale-in.
var wideShapes = []struct{ cpu, memory string }{
	{"240m", "300Mi"}, {"450m", "400Mi"}, {"650m", "700Mi"}, {"900m", "1Gi"}, {"1400m", "2Gi"},
	{"1900m", "4Gi"}, {"2400m", "8Gi"}, {"2800m", "4Gi"}, {"3300m", "16Gi"}, {"3700m", "10Gi"},
	{"2300m", "64Gi"}, {"900m", "30Gi"}, {"1300m", "32Gi"}, {"2800m", "32Gi"}, {"1800m", "30Gi"},
	{"1850m", "20Gi"}, {"3300m", "30Gi"}, {"3800m", "300Mi"}, {"225m", "500Mi"}, {"450m", "750Mi"},
	{"700m", "1Gi"}, {"900m", "2Gi"}, {"1300m", "4Gi"}, {"1800m", "8Gi"}, {"2400m", "16Gi"},
	{"2800m", "32Gi"}, {"3400m", "28Gi"}, {"3800m", "64Gi"}, {"3300m", "13Gi"}, {"2800m", "24Gi"},
}

// Question: Do pods spread across many deployments of different shapes and constraints become Ready as fast as a few uniform ones?
//
// Real clusters run dozens of deployments, each its own shape and spread rules. perfv1's Wide Deployments: 30 fixed
// shapes (225m-3800m, 300Mi-64Gi), all zonally spread, some also anti-affine or hostname-spread.
var PodsWide = perf.Scenario{
	Name:     "podsWide",
	Group:    "pods",
	Question: "Do pods spread across many deployments of different shapes and constraints become Ready as fast as a few uniform ones?",
	Summary:  "N pods over 30 deployments with fixed varied shapes (225m-3800m, 300Mi-64Gi), all zonally spread, 3 also hostname anti-affine and 3 also hostname-spread, scaled to 70%: many distinct pod shapes and topology groups at once.",
	Vectors:  map[perf.Vector]int{perf.VectorPods: 1000},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "N pending pods until all are healthy: instance type selection across many shapes, and 30 zonal topology groups."},
		{Name: "consolidation", Summary: "Scale every deployment to about 70% until nodes stop being disrupted: consolidation over a heterogeneous node mix."},
	},
	Run: func(r *perf.Run) {
		n := r.Size(perf.VectorPods)
		deps := make([]*appsv1.Deployment, len(wideShapes))
		initial, scaled := make([]int, len(wideShapes)), make([]int, len(wideShapes))
		for i, sh := range wideShapes {
			initial[i], scaled[i] = frac(n, 35, 1000), frac(n, 25, 1000)
			if i >= 20 {
				initial[i], scaled[i] = frac(n, 30, 1000), frac(n, 20, 1000)
			}
			opts := []test.DeploymentOptionModifier{ownPool(r), zoneSpread()}
			switch {
			case i < 3:
				opts = append(opts, test.WithPodAntiAffinityHostname())
			case i < 6:
				opts = append(opts, test.WithHostnameSpread())
			}
			deps[i] = r.Deployment(fmt.Sprintf("w%02d", i+1), initial[i], sh.cpu, sh.memory, opts...)
		}

		r.Create(r.NodeClass, r.NodePool)
		r.AwaitNodePoolsReady(5 * time.Minute)
		scaleOut := r.StartPhase("scaleOut")
		r.Create(objects(deps)...)
		scaleOut.AwaitScaleOut(r.Replicas(), 20*time.Minute)

		consolidation := r.StartPhase("consolidation")
		from := r.Replicas()
		for i, d := range deps {
			r.Scale(scaled[i], d)
		}
		consolidation.AwaitConsolidation(from, r.Replicas(), 30*time.Minute)
	},
}
