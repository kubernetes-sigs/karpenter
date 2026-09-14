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

package performance

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
)

// The "Many Diverse NodePools" scenario is the inverse of the other performance tests: instead of scaling
// a large number of pods against a single NodePool, it registers a large number of *diverse* NodePools and
// only a handful of pods/nodes. This isolates the cost that scales with NodePool cardinality rather than
// with pod or node count, e.g. the per-NodePool bookkeeping done by cluster cost tracking, the NodePool
// counter/metrics controllers, and cluster state. A memory regression in any of those paths shows up here
// as elevated Karpenter controller memory even though the cluster itself is tiny.

const (
	manyNodePoolsCount           = 500
	manyNodePoolsPodCount        = 100
	manyNodePoolsScaleInPodCount = 40

	// The "targeted" test pins one small deployment to each of a subset of NodePools via a nodeSelector on the
	// built-in karpenter.sh/nodepool label (i.e. node affinity to a specific NodePool). That lands pods on that
	// many distinct NodePools, driving real per-NodePool provisioning and populating cluster cost tracking
	// across many NodePools at once rather than just scanning them. Node count is high here by design.
	// manyNodePoolsTargetPools is how many of the NodePools receive a workload; scale-in zeroes out half of
	// those deployments so their nodes consolidate away.
	manyNodePoolsTargetPools = 150

	// manyNodePoolsTaintKey taints a fraction of the NodePools so scheduling and disruption have to reason
	// about tolerations at high NodePool cardinality. Combined with the topology-spread constraints on the
	// workloads below, this exercises the topology/taint bookkeeping paths implicated in memory/CPU
	// regressions like kubernetes-sigs/karpenter#2779 and #2954. Workloads tolerate this key so tainted
	// NodePools remain usable.
	manyNodePoolsTaintKey = "perf.example.com/dedicated"
)

// manyNodePoolsToleration lets the test workloads schedule onto the tainted subset of NodePools.
var manyNodePoolsToleration = corev1.Toleration{Key: manyNodePoolsTaintKey, Operator: corev1.TolerationOpExists}

// Resource-usage thresholds below are base defaults; every one is overridable per-metric via the
// KARPENTER_PERF_THRESHOLDS env var (see thresholds.go). The base values were chosen from repeated local
// kind+KWOK runs of this suite: they sit roughly 1.3–1.5x above the observed P95 to absorb run-to-run
// variance (controller RSS drifts upward under repeated NodePool churn) while still tripping on the kind of
// per-NodePool memory/CPU regression this test guards against. They are intentionally generous rather than
// tight; tighten them via the override env var once a stable CI baseline exists.

// diverseNodePools builds `count` NodePools that all reference the shared nodeClass but each pin a different
// slice of the instance-type catalog, so no two NodePools collapse to the same shape. Diversity is layered:
//   - Portable across providers: a rotating capacity-type requirement, a unique scheduling weight, and a
//     unique node-template label.
//   - KWOK only: because KWOK exposes a rich, deterministic instance-type catalog we additionally pin each
//     NodePool to a rotating zone, architecture, and instance-size ceiling. On real cloud providers these
//     dimensions vary by account/region, so we keep to the portable capacity-type dimension there.
//
// Pods created by the test carry no node requirements, so they remain schedulable against any NodePool the
// scheduler selects (every NodePool retains at least one viable, affordable offering).
func diverseNodePools(count int) []*v1.NodePool {
	capacityTypes := [][]string{
		{v1.CapacityTypeSpot},
		{v1.CapacityTypeOnDemand},
		{v1.CapacityTypeSpot, v1.CapacityTypeOnDemand},
	}
	zones := [][]string{
		{"test-zone-a"},
		{"test-zone-b"},
		{"test-zone-c"},
		{"test-zone-d"},
		{"test-zone-a", "test-zone-b"},
	}
	arches := [][]string{
		{v1.ArchitectureAmd64},
		{v1.ArchitectureArm64},
	}
	// Upper bound on instance-size (cpu count for the generic KWOK catalog); keeps nodes small and varies
	// the offering set each NodePool tracks.
	sizeCeilings := []string{"8", "16", "32"}

	nodePools := make([]*v1.NodePool, 0, count)
	for i := 0; i < count; i++ {
		np := env.DefaultNodePool(nodeClass)
		np.Spec.Limits = v1.Limits{}
		np.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
		np.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("30s")
		np.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
		// Unique weight and node-template label so that no two NodePools are identical.
		np.Spec.Weight = lo.ToPtr(int32(1 + i%50))
		np.Spec.Template.Labels = lo.Assign(np.Spec.Template.Labels, map[string]string{
			"diverse-nodepool-index": fmt.Sprintf("%d", i),
		})

		reqs := []v1.NodeSelectorRequirementWithMinValues{{
			Key:      v1.CapacityTypeLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   capacityTypes[i%len(capacityTypes)],
		}}
		if env.IsDefaultNodeClassKWOK() {
			reqs = append(reqs,
				v1.NodeSelectorRequirementWithMinValues{
					Key:      corev1.LabelTopologyZone,
					Operator: corev1.NodeSelectorOpIn,
					Values:   zones[i%len(zones)],
				},
				v1.NodeSelectorRequirementWithMinValues{
					Key:      corev1.LabelArchStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   arches[i%len(arches)],
				},
				v1.NodeSelectorRequirementWithMinValues{
					Key:      v1alpha1.InstanceSizeLabelKey,
					Operator: corev1.NodeSelectorOpLt,
					Values:   []string{sizeCeilings[i%len(sizeCeilings)]},
				},
			)
		}
		test.ReplaceRequirements(np, reqs...)

		// Taint every third NodePool so the scheduler and disruption loops must account for tolerations across
		// a large, diverse NodePool set. Test workloads carry manyNodePoolsToleration so these stay schedulable.
		if i%3 == 0 {
			np.Spec.Template.Spec.Taints = append(np.Spec.Template.Spec.Taints, corev1.Taint{
				Key:    manyNodePoolsTaintKey,
				Value:  "true",
				Effect: corev1.TaintEffectNoSchedule,
			})
		}
		nodePools = append(nodePools, np)
	}
	return nodePools
}

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Many Diverse NodePools", func() {
		It("should stay within resource bounds with many diverse NodePools and few pods/nodes", func() {
			// ========== PHASE 1: SCALE-OUT ==========
			By(fmt.Sprintf("Creating %d diverse NodePools", manyNodePoolsCount))
			nodePools := diverseNodePools(manyNodePoolsCount)
			objs := make([]client.Object, 0, len(nodePools)+1)
			objs = append(objs, nodeClass)
			for _, np := range nodePools {
				objs = append(objs, np)
			}
			env.ExpectCreated(objs...)

			// A deliberately small, resource-light deployment: enough pods to spin up a few NodeClaims (so the
			// cost-tracking path is actually exercised) while keeping the node count small. The pressure in this
			// scenario comes from NodePool cardinality, not from the pod/node count. A zone topology-spread
			// constraint plus a toleration for the tainted NodePools make the scheduler exercise its
			// topology/taint bookkeeping across the whole NodePool set.
			By(fmt.Sprintf("Creating a small deployment of %d pods", manyNodePoolsPodCount))
			deployment := test.Deployment(test.CreateDeploymentOptions("many-nodepools-app", manyNodePoolsPodCount, "50m", "128Mi",
				test.WithTolerations([]corev1.Toleration{manyNodePoolsToleration}),
				test.WithTopologySpreadConstraints([]corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "many-nodepools-app"}},
				}}),
			))
			env.ExpectCreated(deployment)

			By(fmt.Sprintf("Monitoring scale-out with %d NodePools", manyNodePoolsCount))
			scaleOutReport, err := ReportScaleOutWithOutput(env, "Many Diverse NodePools Performance Test", manyNodePoolsPodCount, 10*time.Minute, "many_nodepools_scale_out")
			Expect(err).ToNot(HaveOccurred(), "Scale-out should execute successfully")

			By("Validating scale-out performance with many NodePools")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(manyNodePoolsPodCount), "Should have all pods scheduled")

			// The core guard: controller memory must stay bounded even with a high cardinality of NodePools.
			// This is the regression class that motivated the memory optimizations in cluster cost tracking.
			Expect(scaleOutReport.KarpenterP95MemoryMB).To(BeNumerically("<", MemoryThreshold("manyNodePools/scaleOut", 300)),
				"Karpenter controller P95 memory should stay bounded with many NodePools")
			Expect(scaleOutReport.KarpenterAvgCPUCores).To(BeNumerically("<", CPUThreshold("manyNodePools/scaleOut", 0.80)),
				"Karpenter controller avg CPU should stay bounded with many NodePools")
			Expect(scaleOutReport.KarpenterP95CPUCores).To(BeNumerically("<", CPUThreshold("manyNodePools/scaleOutP95", 0.80)),
				"Karpenter controller P95 CPU should stay bounded with many NodePools")
			Expect(scaleOutReport.TotalTime).To(BeNumerically("<", TotalTimeThreshold("manyNodePools/scaleOut", 5*time.Minute)),
				"Scale-out should complete quickly given the small pod/node count")

			// ========== PHASE 2: SCALE-IN / CONSOLIDATION ==========
			// Scaling the deployment down exercises the disruption/consolidation controllers, which recompute
			// cluster cost across every NodePool. With this many NodePools that is the memory-sensitive path we
			// want to guard. The absolute node count is small, so consolidation may not remove nodes; the value
			// here is the controller's memory/CPU behavior while evaluating disruption at high NodePool cardinality.
			By("Scaling the deployment down to trigger consolidation")
			initialNodes := scaleOutReport.TotalNodes
			deployment.Spec.Replicas = lo.ToPtr(int32(manyNodePoolsScaleInPodCount))
			env.ExpectUpdated(deployment)

			By("Monitoring scale-in / consolidation with many NodePools")
			consolidationReport, err := ReportConsolidationWithOutput(env, "Many Diverse NodePools Consolidation Test", manyNodePoolsPodCount, manyNodePoolsScaleInPodCount, initialNodes, 15*time.Minute, "many_nodepools_consolidation")
			Expect(err).ToNot(HaveOccurred(), "Consolidation should execute successfully")

			By("Validating scale-in performance with many NodePools")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(manyNodePoolsScaleInPodCount), "Should have the scaled-in pod count")
			Expect(consolidationReport.PodsNetChange).To(Equal(manyNodePoolsScaleInPodCount-manyNodePoolsPodCount), "Should reflect the net pod reduction")

			Expect(consolidationReport.KarpenterP95MemoryMB).To(BeNumerically("<", MemoryThreshold("manyNodePools/consolidation", 300)),
				"Karpenter controller P95 memory should stay bounded during consolidation with many NodePools")
			Expect(consolidationReport.KarpenterAvgCPUCores).To(BeNumerically("<", CPUThreshold("manyNodePools/consolidation", 0.30)),
				"Karpenter controller avg CPU should stay bounded during consolidation with many NodePools")
			Expect(consolidationReport.KarpenterP95CPUCores).To(BeNumerically("<", CPUThreshold("manyNodePools/consolidationP95", 1.00)),
				"Karpenter controller P95 CPU should stay bounded during consolidation with many NodePools")
			Expect(consolidationReport.TotalTime).To(BeNumerically("<", TotalTimeThreshold("manyNodePools/consolidation", 15*time.Minute)),
				"Consolidation should complete within the timeout")
		})

		It("should stay within resource bounds when pods target many NodePools", func() {
			// This variant complements the "few nodes" case above: instead of letting pods collapse onto the
			// single cheapest NodePool, each deployment is pinned to a specific NodePool via a nodeSelector on
			// the built-in karpenter.sh/nodepool label (node affinity to that NodePool). Pods therefore land on
			// many distinct NodePools, driving real per-NodePool provisioning and populating cluster cost
			// tracking across many NodePools at once. Node count is high here by design.
			scaleInPools := manyNodePoolsTargetPools / 2

			// ========== PHASE 1: SCALE-OUT (one pinned deployment per targeted NodePool) ==========
			By(fmt.Sprintf("Creating %d diverse NodePools", manyNodePoolsCount))
			nodePools := diverseNodePools(manyNodePoolsCount)
			objs := make([]client.Object, 0, len(nodePools)+1)
			objs = append(objs, nodeClass)
			for _, np := range nodePools {
				objs = append(objs, np)
			}
			env.ExpectCreated(objs...)

			By(fmt.Sprintf("Creating one pinned deployment for each of %d targeted NodePools", manyNodePoolsTargetPools))
			deployments := make([]*appsv1.Deployment, manyNodePoolsTargetPools)
			for i := 0; i < manyNodePoolsTargetPools; i++ {
				deployments[i] = test.Deployment(test.CreateDeploymentOptions(fmt.Sprintf("mnp-target-%03d", i), 1, "50m", "128Mi",
					test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: nodePools[i].Name}),
					test.WithTolerations([]corev1.Toleration{manyNodePoolsToleration}),
				))
				env.ExpectCreated(deployments[i])
			}

			By(fmt.Sprintf("Monitoring scale-out across %d targeted NodePools", manyNodePoolsTargetPools))
			scaleOutReport, err := ReportScaleOutWithOutput(env, "Many NodePools Targeted Performance Test", manyNodePoolsTargetPools, 15*time.Minute, "many_nodepools_targeted_scale_out")
			Expect(err).ToNot(HaveOccurred(), "Scale-out should execute successfully")

			By("Validating targeted scale-out performance")
			Expect(scaleOutReport.TestType).To(Equal("scale-out"), "Should be detected as scale-out test")
			Expect(scaleOutReport.TotalPods).To(Equal(manyNodePoolsTargetPools), "Should have all pods scheduled")
			// Sanity: pinning to distinct NodePools should provision roughly one node per targeted NodePool.
			Expect(scaleOutReport.TotalNodes).To(BeNumerically(">", manyNodePoolsTargetPools/2),
				"Pinned pods should provision nodes across many distinct NodePools")

			Expect(scaleOutReport.KarpenterP95MemoryMB).To(BeNumerically("<", MemoryThreshold("manyNodePoolsTargeted/scaleOut", 500)),
				"Karpenter controller P95 memory should stay bounded when targeting many NodePools")
			Expect(scaleOutReport.KarpenterAvgCPUCores).To(BeNumerically("<", CPUThreshold("manyNodePoolsTargeted/scaleOut", 1.50)),
				"Karpenter controller avg CPU should stay bounded when targeting many NodePools")
			Expect(scaleOutReport.KarpenterP95CPUCores).To(BeNumerically("<", CPUThreshold("manyNodePoolsTargeted/scaleOutP95", 2.00)),
				"Karpenter controller P95 CPU should stay bounded when targeting many NodePools")

			// ========== PHASE 2: SCALE-IN / CONSOLIDATION ==========
			// Zero out half the deployments so their NodePools empty and those nodes consolidate away.
			By(fmt.Sprintf("Zeroing out %d deployments to trigger consolidation", manyNodePoolsTargetPools-scaleInPools))
			initialNodes := scaleOutReport.TotalNodes
			for i := scaleInPools; i < manyNodePoolsTargetPools; i++ {
				deployments[i].Spec.Replicas = lo.ToPtr(int32(0))
				env.ExpectUpdated(deployments[i])
			}

			By("Monitoring targeted scale-in / consolidation")
			consolidationReport, err := ReportConsolidationWithOutput(env, "Many NodePools Targeted Consolidation Test", manyNodePoolsTargetPools, scaleInPools, initialNodes, 20*time.Minute, "many_nodepools_targeted_consolidation")
			Expect(err).ToNot(HaveOccurred(), "Consolidation should execute successfully")

			By("Validating targeted scale-in performance")
			Expect(consolidationReport.TestType).To(Equal("consolidation"), "Should be detected as consolidation test")
			Expect(consolidationReport.TotalPods).To(Equal(scaleInPools), "Should have the scaled-in pod count")
			Expect(consolidationReport.PodsNetChange).To(Equal(scaleInPools-manyNodePoolsTargetPools), "Should reflect the net pod reduction")

			Expect(consolidationReport.KarpenterP95MemoryMB).To(BeNumerically("<", MemoryThreshold("manyNodePoolsTargeted/consolidation", 500)),
				"Karpenter controller P95 memory should stay bounded during targeted consolidation")
			Expect(consolidationReport.KarpenterAvgCPUCores).To(BeNumerically("<", CPUThreshold("manyNodePoolsTargeted/consolidation", 2.00)),
				"Karpenter controller avg CPU should stay bounded during targeted consolidation")
			Expect(consolidationReport.KarpenterP95CPUCores).To(BeNumerically("<", CPUThreshold("manyNodePoolsTargeted/consolidationP95", 2.50)),
				"Karpenter controller P95 CPU should stay bounded during targeted consolidation")
			Expect(consolidationReport.TotalTime).To(BeNumerically("<", TotalTimeThreshold("manyNodePoolsTargeted/consolidation", 20*time.Minute)),
				"Consolidation should complete within the timeout")
		})
	})
})
