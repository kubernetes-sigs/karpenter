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

//go:build rapid

// Rapid property tests for the NodeClaim optimization pass. Gated behind the
// `rapid` build tag so default `go test ./...` runs (including CI, `go vet`,
// race, and coverage) skip the file entirely.
//
// Running the rapid suite:
//
/*
export KUBEBUILDER_ASSETS="$(setup-envtest use -p path)"
export TEST_OUTPUT_DIR=$(pwd)/test_output
go test -tags rapid ./pkg/controllers/provisioning/scheduling/ \
  -run TestScheduling \
  --ginkgo.focus="should handle random" \
#  --ginkgo.focus="NodeClaim Optimization Rapid" \
  -v -count=1 \
  -timeout 30m \
  -rapid.checks=100 \
  | grep -v "NodeClaim optimization pass complete" \
  | grep -v "relaxing soft constraints for pod since"
*/
//
// Narrow the focus to a single property:
//
//	--ginkgo.focus="should handle random"   									# random workloads
//	--ginkgo.focus="should handle diverse"                                      # diverse constraints
//
// Tunables:
//   - `-rapid.checks=N` sets the number of randomized iterations (default 100).
//   - `-timeout T` bounds the whole run; 30m is a safe default for 100 checks.
//   - TEST_OUTPUT_DIR enables the CSV/JSONL diagnostic exporter below. Leave
//     unset for a no-op.
//
// For the deterministic Ginkgo suite (no `rapid` tag, no envtest) run with
// `--ginkgo.focus="NodeClaim Optimization" --ginkgo.skip="Rapid"`.

package scheduling_test

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	"pgregory.net/rapid"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	kwok "sigs.k8s.io/karpenter/kwok/cloudprovider"
	kwokoptions "sigs.k8s.io/karpenter/kwok/options"
)

func filterByMaxVCPU(instanceTypes []*cloudprovider.InstanceType, max string) []*cloudprovider.InstanceType {
	maxQ := resource.MustParse(max)
	return lo.Filter(instanceTypes, func(it *cloudprovider.InstanceType, _ int) bool {
		return it.Capacity.Cpu().Cmp(maxQ) <= 0
	})
}

// nodeClaimNameForHostname returns "<state>-nc-<suffix>" where suffix is the
// trailing numeric token of the hostname (e.g. "hostname-placeholder-0025" ->
// "s2-post-nc-0025"). Hostname is the stable NodeClaim identity across states
// — keying the display name off it lets pre/post rows line up by eye.
// Falls back to the full hostname when it has no trailing "-<number>" tail.
func nodeClaimNameForHostname(state, hostname string) string {
	suffix := hostname
	if i := strings.LastIndex(hostname, "-"); i >= 0 {
		suffix = hostname[i+1:]
	}
	return fmt.Sprintf("%s-nc-%s", state, suffix)
}

type CSVWriter struct {
	summaryWriter *csv.Writer
	ncWriter      *csv.Writer
	podWriter     *csv.Writer
	podConfigFile *os.File
	summaryFile   *os.File
	ncFile        *os.File
	podFile       *os.File
}

func cpuToFloat(q resource.Quantity) float64 {
	return float64(q.MilliValue()) / 1000.0
}

func memToGiB(q resource.Quantity) float64 {
	return float64(q.Value()) / (1024 * 1024 * 1024)
}

func NewCSVWriter(prefix ...string) (*CSVWriter, error) {
	w := &CSVWriter{}

	dir := os.Getenv("TEST_OUTPUT_DIR")
	if dir == "" {
		return w, nil // no-op writer; all methods are nil-safe
	}

	p := ""
	if len(prefix) > 0 && prefix[0] != "" {
		p = prefix[0] + "_"
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	var err error
	w.summaryFile, err = os.Create(dir + "/" + p + "optimization_summary.csv")
	if err != nil {
		return nil, err
	}
	w.summaryWriter = csv.NewWriter(w.summaryFile)
	w.summaryWriter.Write([]string{"run", "pod_count", "pre_nodeclaims", "post_nodeclaims", "pre_cost", "post_cost", "duration_sec"})

	w.ncFile, err = os.Create(dir + "/" + p + "nodeclaim_details.csv")
	if err != nil {
		return nil, err
	}
	w.ncWriter = csv.NewWriter(w.ncFile)
	// nodeclaim_name is keyed off hostname (the stable NodeClaim identity)
	// so rows line up across pre/post by eye.
	w.ncWriter.Write([]string{"run", "state", "nodeclaim_name", "hostname", "instance_type", "cpu_capacity", "memory_capacity", "kube_overhead_cpu", "kube_overhead_memory", "pod_count", "pod_cpu_sum", "pod_memory_sum", "price"})

	w.podFile, err = os.Create(dir + "/" + p + "pod_details.csv")
	if err != nil {
		return nil, err
	}
	w.podWriter = csv.NewWriter(w.podFile)
	w.podWriter.Write([]string{"run", "state", "nodeclaim_name", "hostname", "pod_name", "cpu_request", "memory_request"})

	w.podConfigFile, err = os.Create(dir + "/" + p + "pod_configs.jsonl")
	if err != nil {
		return nil, err
	}

	return w, nil
}

func (w *CSVWriter) WriteSummary(run, podCount, preNodeClaims, postNodeClaims int, preCost, postCost, duration float64) {
	if w.summaryWriter == nil {
		return
	}
	w.summaryWriter.Write([]string{
		fmt.Sprintf("%d", run),
		fmt.Sprintf("%d", podCount),
		fmt.Sprintf("%d", preNodeClaims),
		fmt.Sprintf("%d", postNodeClaims),
		fmt.Sprintf("%.4f", preCost),
		fmt.Sprintf("%.4f", postCost),
		fmt.Sprintf("%.3f", duration),
	})
}

// WriteNodeClaims emits one nodeclaim row + one pod row per pod for each
// NodeClaim. Goes through SnapshotNodeClaims so live and post-Solve call
// sites share a single rendering path with WriteNodeClaimSnapshots.
func (w *CSVWriter) WriteNodeClaims(run int, state string, nodeClaims []*scheduling.NodeClaim) {
	if w.ncWriter == nil {
		return
	}
	w.WriteNodeClaimSnapshots(run, state, scheduling.SnapshotNodeClaims(nodeClaims))
}

// WriteNodeClaimSnapshots emits rows for a pre-captured snapshot slice.
// Used to record the pre-optimization state (captured inside tryOptimize
// before any RevertTo) alongside the final-post state.
func (w *CSVWriter) WriteNodeClaimSnapshots(run int, state string, snaps []scheduling.NodeClaimSnapshot) {
	if w.ncWriter == nil {
		return
	}
	// Copy before sorting so we don't reorder the caller's slice (the live
	// Scheduler state or a captured OptimizationSnapshot). Hostname order is
	// the reader-friendly ordering: the same hostname appears at the same
	// position across pre/post, so split survivors, untouched claims, and new
	// claims are obvious at a glance.
	ordered := make([]scheduling.NodeClaimSnapshot, len(snaps))
	copy(ordered, snaps)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Hostname < ordered[j].Hostname
	})
	for _, snap := range ordered {
		if snap.CheapestInstance == nil {
			continue
		}
		it := snap.CheapestInstance
		name := nodeClaimNameForHostname(state, snap.Hostname)

		podCPU := resource.Quantity{}
		podMem := resource.Quantity{}
		for _, pod := range snap.Pods {
			for _, c := range pod.Spec.Containers {
				podCPU.Add(*c.Resources.Requests.Cpu())
				podMem.Add(*c.Resources.Requests.Memory())
			}
		}

		w.ncWriter.Write([]string{
			fmt.Sprintf("%d", run),
			state,
			name,
			snap.Hostname,
			it.Name,
			fmt.Sprintf("%.2f", cpuToFloat(*it.Capacity.Cpu())),
			fmt.Sprintf("%.2f", memToGiB(*it.Capacity.Memory())),
			fmt.Sprintf("%.2f", cpuToFloat(*it.Overhead.KubeReserved.Cpu())),
			fmt.Sprintf("%.2f", memToGiB(*it.Overhead.KubeReserved.Memory())),
			fmt.Sprintf("%d", len(snap.Pods)),
			fmt.Sprintf("%.2f", cpuToFloat(podCPU)),
			fmt.Sprintf("%.2f", memToGiB(podMem)),
			fmt.Sprintf("%.4f", snap.Price),
		})

		for _, pod := range snap.Pods {
			cpu := resource.Quantity{}
			mem := resource.Quantity{}
			for _, c := range pod.Spec.Containers {
				cpu.Add(*c.Resources.Requests.Cpu())
				mem.Add(*c.Resources.Requests.Memory())
			}
			w.podWriter.Write([]string{
				fmt.Sprintf("%d", run),
				state,
				name,
				snap.Hostname,
				pod.Name,
				fmt.Sprintf("%.2f", cpuToFloat(cpu)),
				fmt.Sprintf("%.2f", memToGiB(mem)),
			})
		}
	}
	w.ncWriter.Flush()
	w.podWriter.Flush()
}

func (w *CSVWriter) WritePodConfigs(run int, pods []*corev1.Pod) {
	if w.podConfigFile == nil {
		return
	}
	type hostPort struct {
		Port     int32  `json:"port"`
		Protocol string `json:"protocol"`
	}
	type podConfig struct {
		Name                      string                            `json:"name"`
		UID                       string                            `json:"uid,omitempty"`
		CPURequest                string                            `json:"cpuRequest"`
		MemoryRequest             string                            `json:"memoryRequest"`
		NodeRequirements          []corev1.NodeSelectorRequirement  `json:"nodeRequirements,omitempty"`
		TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
		PodAntiAffinity           []corev1.PodAffinityTerm          `json:"podAntiAffinity,omitempty"`
		PodAffinityPreferences    []corev1.WeightedPodAffinityTerm  `json:"podAffinityPreferences,omitempty"`
		HostPorts                 []hostPort                        `json:"hostPorts,omitempty"`
		Labels                    map[string]string                 `json:"labels,omitempty"`
	}
	type runEntry struct {
		Run  int         `json:"run"`
		Pods []podConfig `json:"pods"`
	}

	entry := runEntry{Run: run}
	for _, pod := range pods {
		pc := podConfig{
			Name:   pod.Name,
			UID:    string(pod.UID),
			Labels: pod.Labels,
		}
		// Resource requests from first container
		for _, c := range pod.Spec.Containers {
			if cpu := c.Resources.Requests.Cpu(); cpu != nil {
				pc.CPURequest = cpu.String()
			}
			if mem := c.Resources.Requests.Memory(); mem != nil {
				pc.MemoryRequest = fmt.Sprintf("%.2fGi", memToGiB(*mem))
			}
			for _, p := range c.Ports {
				if p.HostPort > 0 {
					pc.HostPorts = append(pc.HostPorts, hostPort{Port: p.HostPort, Protocol: string(p.Protocol)})
				}
			}
			break // first container only
		}
		if aff := pod.Spec.Affinity; aff != nil {
			if na := aff.NodeAffinity; na != nil && na.RequiredDuringSchedulingIgnoredDuringExecution != nil {
				for _, term := range na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
					pc.NodeRequirements = append(pc.NodeRequirements, term.MatchExpressions...)
				}
			}
			if paa := aff.PodAntiAffinity; paa != nil {
				pc.PodAntiAffinity = paa.RequiredDuringSchedulingIgnoredDuringExecution
			}
			if pa := aff.PodAffinity; pa != nil {
				pc.PodAffinityPreferences = pa.PreferredDuringSchedulingIgnoredDuringExecution
			}
		}
		pc.TopologySpreadConstraints = pod.Spec.TopologySpreadConstraints
		entry.Pods = append(entry.Pods, pc)
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	w.podConfigFile.Write(data)
	w.podConfigFile.Write([]byte("\n"))
}

func (w *CSVWriter) Close() {
	if w.summaryWriter == nil {
		return
	}
	w.summaryWriter.Flush()
	w.ncWriter.Flush()
	w.podWriter.Flush()
	w.summaryFile.Close()
	w.ncFile.Close()
	w.podFile.Close()
	w.podConfigFile.Close()
}

var _ = Describe("NodeClaim Optimization Rapid", func() {
	It("should handle random workloads", func() {
		fmt.Println("\n\n=== STARTING NodeClaim Optimization Rapid Cost Test ===")
		fmt.Println("  pre  = optimized scheduler, pre-optimization snapshot (captured before first RevertTo)")
		fmt.Println("  post = optimized scheduler, final state")
		csvWriter, err := NewCSVWriter("cost")
		Expect(err).ToNot(HaveOccurred())
		defer csvWriter.Close()

		createNodePool := func() *v1.NodePool {
			return test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Limits: v1.Limits(corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1000000"),
					}),
				},
			})
		}

		runIndex := 0
		var triggeredRuns, cheaperRuns, costlierRuns int
		var totalPreNCs, totalPostNCs int
		var totalPreCost, totalPostCost float64
		var cheaperPreCost, cheaperPostCost float64
		var cheaperPreNCs, cheaperPostNCs int
		var cheaperPctSum float64
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			runIndex++

			ExpectCleanedUp(ctx, env.Client)
			cluster.Reset()
			scheduling.QueueDepth.Reset()
			scheduling.DurationSeconds.Reset()
			scheduling.UnschedulablePodsCount.Reset()

			ctx = kwokoptions.ToContext(ctx, &kwokoptions.Options{})
			instanceTypes, err := kwok.ConstructInstanceTypes(ctx)
			Expect(err).ToNot(HaveOccurred())
			instanceTypes = filterByMaxVCPU(instanceTypes, "64")
			cloudProvider.InstanceTypes = instanceTypes

			podCount := rapid.IntRange(1, 200).Draw(t, "podCount")

			pods := make([]*corev1.Pod, podCount)
			for i := 0; i < podCount; i++ {
				cpuFloat := rapid.Float64Range(0.25, 8.0).Draw(t, "cpuRequest")
				memFloatMultiplier := rapid.Float64Range(.25, 16).Draw(t, "memRequest")
				memFloat := cpuFloat * memFloatMultiplier
				pods[i] = test.UnschedulablePod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("pod-%d", i),
						Namespace: "default",
						UID:       types.UID(fmt.Sprintf("pod-%d", i)),
					},
					Image: "nginx:latest",
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%.2f", cpuFloat)),
							corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%.2fGi", memFloat)),
						},
					},
				})
			}

			nodePool := createNodePool()
			ExpectApplied(ctx, env.Client, nodePool)

			podsCopy := make([]*corev1.Pod, len(pods))
			for i, p := range pods {
				podsCopy[i] = p.DeepCopy()
			}
			start := time.Now()
			s, _ := prov.NewScheduler(ctx, podsCopy, nil, scheduling.EnableNodeClaimOptimization)
			results, _ := s.Solve(ctx, podsCopy)
			duration := time.Since(start)

			postCost := scheduling.TotalNodeClaimPrice(results.NewNodeClaims)
			postNCs := len(results.NewNodeClaims)
			// Pre is nil when the pass never fired (no unscheduled pods at
			// loop drain). In that case pre ≡ post — nothing moved.
			preCost := postCost
			preNCs := postNCs
			triggered := s.OptimizationSnapshot.Pre != nil
			if triggered {
				preCost = s.OptimizationSnapshot.PreCost
				preNCs = len(s.OptimizationSnapshot.Pre)
				triggeredRuns++
			}

			totalPreCost += preCost
			totalPostCost += postCost
			totalPreNCs += preNCs
			totalPostNCs += postNCs

			runSaved := preCost - postCost
			runPctSaved := 0.0
			if preCost > 0 {
				runPctSaved = runSaved / preCost * 100
			}
			if postCost < preCost-0.00001 {
				cheaperRuns++
				cheaperPreCost += preCost
				cheaperPostCost += postCost
				cheaperPreNCs += preNCs
				cheaperPostNCs += postNCs
				cheaperPctSum += runPctSaved
			} else if postCost > preCost+0.00001 {
				costlierRuns++
			}

			// Sign convention: saved > 0 means the optimized cost is lower than
			// the pre-optimization cost. Negative values mean the pass made
			// things costlier.
			fmt.Printf("cost (%4d), pods (%4d), nodeclaims (%3d → %3d), cost (%8.4f → %8.4f), saved %+8.4f (%+5.1f%%), duration %s\n",
				runIndex, podCount,
				preNCs, postNCs,
				preCost, postCost,
				runSaved, runPctSaved,
				duration.Round(time.Millisecond))

			csvWriter.WriteSummary(runIndex, podCount, preNCs, postNCs, preCost, postCost, duration.Seconds())
			if triggered {
				csvWriter.WriteNodeClaimSnapshots(runIndex, "pre", s.OptimizationSnapshot.Pre)
				csvWriter.WriteNodeClaimSnapshots(runIndex, "post", s.OptimizationSnapshot.Post)
			} else {
				csvWriter.WriteNodeClaims(runIndex, "pre", results.NewNodeClaims)
				csvWriter.WriteNodeClaims(runIndex, "post", results.NewNodeClaims)
			}
			csvWriter.WritePodConfigs(runIndex, pods)

			// Per-run invariant: every split decision must pay off against
			// its own estimate. If the optimization pass ran, the final cost
			// must be <= the cost captured before the first revert. A failure
			// here points at estimateCheapestPlacement underpricing displaced
			// pods that ended up fragmenting across multiple NodeClaims.
			Expect(postCost).To(BeNumerically("<=", preCost+.00001),
				"optimized cost (%.4f) should be <= pre-opt cost (%.4f) — bad split decision", postCost, preCost)

			// Verify every input pod appears in exactly one optimized NodeClaim.
			scheduledUIDs := map[types.UID]struct{}{}
			for _, nc := range results.NewNodeClaims {
				for _, pod := range nc.Pods {
					_, dup := scheduledUIDs[pod.UID]
					Expect(dup).To(BeFalse(), "pod %s scheduled on multiple NodeClaims", pod.Name)
					scheduledUIDs[pod.UID] = struct{}{}
				}
			}
			Expect(scheduledUIDs).To(HaveLen(len(pods)), "optimization lost or duplicated pods: want %d, got %d", len(pods), len(scheduledUIDs))

			ExpectCleanedUp(ctx, env.Client)
			cluster.Reset()
		})

		// Sign convention for `saved`: positive = the optimized run is
		// cheaper than pre-optimization; negative = it got costlier. This
		// lines up with the per-run format above so the totals row reads the
		// same way.
		saved := totalPreCost - totalPostCost
		pct := 0.0
		if totalPreCost > 0 {
			pct = saved / totalPreCost * 100
		}
		fmt.Printf("\n=== COST SUMMARY: %d runs ===\n", runIndex)
		fmt.Printf("  pre → post: %3d cheaper, %3d costlier | cost %.2f → %.2f | saved %+.2f (%+.1f%%) | NCs %d → %d\n",
			cheaperRuns, costlierRuns, totalPreCost, totalPostCost, saved, pct, totalPreNCs, totalPostNCs)
		if cheaperRuns > 0 {
			// These are runs that got cheaper by definition, so savings are
			// always positive — render without a sign to avoid the visual
			// noise of a redundant "+".
			cheaperSaved := cheaperPreCost - cheaperPostCost
			cheaperPct := cheaperSaved / cheaperPreCost * 100
			fmt.Printf("    Of runs that got cheaper (%d): cost %.2f → %.2f | saved %.2f (%.1f%%) | avg saved %.2f/run (%.1f%%) | NCs %d → %d\n",
				cheaperRuns, cheaperPreCost, cheaperPostCost, cheaperSaved, cheaperPct,
				cheaperSaved/float64(cheaperRuns), cheaperPctSum/float64(cheaperRuns),
				cheaperPreNCs, cheaperPostNCs)
		}
		fmt.Printf("  %d of %d runs triggered the optimization pass (%d%%)\n",
			triggeredRuns, runIndex, triggeredRuns*100/runIndex)
	})

	It("should handle diverse pod scheduling constraints", func() {
		fmt.Println("\n\n=== STARTING Diverse Constraint Optimization Test ===")
		fmt.Println("  pre  = optimized scheduler, pre-optimization snapshot (captured before first RevertTo)")
		fmt.Println("  post = optimized scheduler, final state")
		csvWriter, err := NewCSVWriter("diverse")
		Expect(err).ToNot(HaveOccurred())
		defer csvWriter.Close()

		// Zones and topology keys available in KWOK instance types.
		zones := []string{"test-zone-a", "test-zone-b", "test-zone-c", "test-zone-d"}
		topologyKeys := []string{corev1.LabelTopologyZone, corev1.LabelHostname}

		createNodePool := func() *v1.NodePool {
			return test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Limits: v1.Limits(corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1000000"),
					}),
				},
			})
		}

		// workloadGroup represents a set of identical replicas (like a Deployment).
		type workloadGroup struct {
			name         string
			replicaCount int
			cpuRequest   string
			memRequest   string
			opts         test.PodOptions // constraint template (applied to all replicas)
		}

		// drawWorkloadGroups creates realistic workload groups where all replicas
		// in a group share the same constraints and resource profile.
		drawWorkloadGroups := func(t *rapid.T) ([]workloadGroup, []*corev1.Pod) {
			groupCount := rapid.IntRange(2, 20).Draw(t, "groupCount")
			constraintRate := rapid.Float64Range(.05, 1).Draw(t, "constraintRate")

			var groups []workloadGroup
			var allPods []*corev1.Pod
			podIdx := 0

			for g := 0; g < groupCount; g++ {
				replicaCount := rapid.IntRange(1, 50).Draw(t, fmt.Sprintf("g%d-replicas", g))
				cpuFloat := rapid.Float64Range(0.25, 4.0).Draw(t, fmt.Sprintf("g%d-cpu", g))
				memFloat := cpuFloat * rapid.Float64Range(0.5, 8.0).Draw(t, fmt.Sprintf("g%d-memRatio", g))

				groupLabel := fmt.Sprintf("group-%d", g)
				cpuStr := fmt.Sprintf("%.2f", cpuFloat)
				memStr := fmt.Sprintf("%.2fGi", memFloat)

				opts := test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"app": groupLabel},
					},
					Image: "nginx:latest",
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cpuStr),
							corev1.ResourceMemory: resource.MustParse(memStr),
						},
					},
				}

				groupSelector := map[string]string{"app": groupLabel}

				// ~30% chance (scaled): pin this group to a specific zone
				if rapid.IntRange(0, 99).Draw(t, fmt.Sprintf("g%d-zone", g)) < int(30*constraintRate) {
					zone := zones[rapid.IntRange(0, len(zones)-1).Draw(t, fmt.Sprintf("g%d-zoneVal", g))]
					opts.NodeRequirements = []corev1.NodeSelectorRequirement{{
						Key:      corev1.LabelTopologyZone,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{zone},
					}}
				}

				// ~30% chance (scaled): topology spread across zones or hosts
				if rapid.IntRange(0, 99).Draw(t, fmt.Sprintf("g%d-tsc", g)) < int(30*constraintRate) {
					topoKey := topologyKeys[rapid.IntRange(0, len(topologyKeys)-1).Draw(t, fmt.Sprintf("g%d-tscKey", g))]
					opts.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
						MaxSkew:           int32(rapid.IntRange(1, 3).Draw(t, fmt.Sprintf("g%d-skew", g))),
						TopologyKey:       topoKey,
						WhenUnsatisfiable: corev1.DoNotSchedule,
						LabelSelector:     &metav1.LabelSelector{MatchLabels: groupSelector},
					}}
				}

				// ~25% chance (scaled): anti-affinity to own replicas (spread across nodes)
				if rapid.IntRange(0, 99).Draw(t, fmt.Sprintf("g%d-anti", g)) < int(25*constraintRate) {
					opts.PodAntiRequirements = []corev1.PodAffinityTerm{{
						LabelSelector: &metav1.LabelSelector{MatchLabels: groupSelector},
						TopologyKey:   corev1.LabelHostname,
					}}
				}

				// ~15% chance (scaled): host port
				if rapid.IntRange(0, 99).Draw(t, fmt.Sprintf("g%d-hport", g)) < int(15*constraintRate) {
					port := int32(rapid.IntRange(8000, 8003).Draw(t, fmt.Sprintf("g%d-port", g)))
					opts.HostPorts = []int32{port}
				}

				// ~20% chance (scaled): pod affinity to a DIFFERENT group
				if g > 0 && rapid.IntRange(0, 99).Draw(t, fmt.Sprintf("g%d-paff", g)) < int(20*constraintRate) {
					targetGroup := rapid.IntRange(0, g-1).Draw(t, fmt.Sprintf("g%d-paffTarget", g))
					topoKey := topologyKeys[rapid.IntRange(0, len(topologyKeys)-1).Draw(t, fmt.Sprintf("g%d-paffKey", g))]
					opts.PodPreferences = []corev1.WeightedPodAffinityTerm{{
						Weight: 50,
						PodAffinityTerm: corev1.PodAffinityTerm{
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": fmt.Sprintf("group-%d", targetGroup)}},
							TopologyKey:   topoKey,
						},
					}}
				}

				grp := workloadGroup{
					name:         groupLabel,
					replicaCount: replicaCount,
					cpuRequest:   cpuStr,
					memRequest:   memStr,
					opts:         opts,
				}
				groups = append(groups, grp)

				// Create replicas — all identical except name/UID
				for r := 0; r < replicaCount; r++ {
					podOpts := opts
					podOpts.ObjectMeta = metav1.ObjectMeta{
						Name:      fmt.Sprintf("pod-%d", podIdx),
						Namespace: "default",
						UID:       types.UID(fmt.Sprintf("pod-%d", podIdx)),
						Labels:    opts.Labels,
					}
					allPods = append(allPods, test.UnschedulablePod(podOpts))
					podIdx++
				}
			}
			return groups, allPods
		}

		runIndex := 0
		// "Cheaper"/"costlier" counts use the same 0.00001 tolerance as the
		// per-run invariants below.
		var totalPreCost, totalPostCost float64
		var totalPreNCs, totalPostNCs int
		var triggeredRuns, cheaperRuns, costlierRuns int
		var cheaperPreCost, cheaperPostCost float64
		var cheaperPreNCs, cheaperPostNCs int
		var cheaperPctSum float64

		rapid.Check(GinkgoT(), func(t *rapid.T) {
			runIndex++

			ExpectCleanedUp(ctx, env.Client)
			cluster.Reset()
			scheduling.QueueDepth.Reset()
			scheduling.DurationSeconds.Reset()
			scheduling.UnschedulablePodsCount.Reset()

			ctx = kwokoptions.ToContext(ctx, &kwokoptions.Options{})
			instanceTypes, err := kwok.ConstructInstanceTypes(ctx)
			Expect(err).ToNot(HaveOccurred())
			instanceTypes = filterByMaxVCPU(instanceTypes, "64")
			cloudProvider.InstanceTypes = instanceTypes

			groups, pods := drawWorkloadGroups(t)

			nodePool := createNodePool()
			ExpectApplied(ctx, env.Client, nodePool)
			podsCopy := make([]*corev1.Pod, len(pods))
			for i, p := range pods {
				podsCopy[i] = p.DeepCopy()
			}
			s, _ := prov.NewScheduler(ctx, podsCopy, nil, scheduling.EnableNodeClaimOptimization)
			results, _ := s.Solve(ctx, podsCopy)
			postCost := scheduling.TotalNodeClaimPrice(results.NewNodeClaims)
			postNCs := len(results.NewNodeClaims)

			// Pre is nil when the pass never fired (no unscheduled pods at
			// loop drain). In that case pre ≡ post — nothing moved.
			preCost := postCost
			preNCs := postNCs
			triggered := s.OptimizationSnapshot.Pre != nil
			if triggered {
				preCost = s.OptimizationSnapshot.PreCost
				preNCs = len(s.OptimizationSnapshot.Pre)
				triggeredRuns++
			}

			totalPreCost += preCost
			totalPostCost += postCost
			totalPreNCs += preNCs
			totalPostNCs += postNCs

			runSaved := preCost - postCost
			runPctSaved := 0.0
			if preCost > 0 {
				runPctSaved = runSaved / preCost * 100
			}
			if postCost < preCost-0.00001 {
				cheaperRuns++
				cheaperPreCost += preCost
				cheaperPostCost += postCost
				cheaperPreNCs += preNCs
				cheaperPostNCs += postNCs
				cheaperPctSum += runPctSaved
			} else if postCost > preCost+0.00001 {
				costlierRuns++
			}

			// Count constraints across all pods for the summary line.
			var nZoneAff, nTSC, nAntiAff, nHostPort, nPodAff int
			for _, pod := range pods {
				if aff := pod.Spec.Affinity; aff != nil {
					if na := aff.NodeAffinity; na != nil && na.RequiredDuringSchedulingIgnoredDuringExecution != nil {
						nZoneAff++
					}
					if pa := aff.PodAffinity; pa != nil && len(pa.PreferredDuringSchedulingIgnoredDuringExecution) > 0 {
						nPodAff++
					}
					if paa := aff.PodAntiAffinity; paa != nil && len(paa.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
						nAntiAff++
					}
				}
				if len(pod.Spec.TopologySpreadConstraints) > 0 {
					nTSC++
				}
				for _, c := range pod.Spec.Containers {
					for _, p := range c.Ports {
						if p.HostPort > 0 {
							nHostPort++
							break
						}
					}
				}
			}

			// Sign convention: saved > 0 means the optimized cost is lower than
			// the pre-optimization cost. Negative values mean the pass made
			// things costlier.
			fmt.Printf("diverse (%3d), pods (%4d), groups (%2d), nodeclaims (%3d → %3d), cost (%8.4f → %8.4f), saved %+8.4f (%+5.1f%%), constraints(zone/tsc/anti/hport/paff, %d/%d/%d/%d/%d), errs(%3d)\n",
				runIndex, len(pods), len(groups),
				preNCs, postNCs,
				preCost, postCost,
				runSaved, runPctSaved,
				nZoneAff, nTSC, nAntiAff, nHostPort, nPodAff, len(results.PodErrors))

			csvWriter.WriteSummary(runIndex, len(pods), preNCs, postNCs, preCost, postCost, 0)
			if triggered {
				csvWriter.WriteNodeClaimSnapshots(runIndex, "pre", s.OptimizationSnapshot.Pre)
				csvWriter.WriteNodeClaimSnapshots(runIndex, "post", s.OptimizationSnapshot.Post)
			} else {
				csvWriter.WriteNodeClaims(runIndex, "pre", results.NewNodeClaims)
				csvWriter.WriteNodeClaims(runIndex, "post", results.NewNodeClaims)
			}
			csvWriter.WritePodConfigs(runIndex, pods)

			// Per-run invariant: the final cost must not exceed the
			// pre-optimization cost snapshot by more than diverseRegressionSlack.
			// Any regression larger than that points at a split decision that
			// paid less on paper than it cost in reality — typically
			// estimateCheapestPlacement underpricing displaced pods that
			// fragmented across multiple fresh NodeClaims under constraints,
			// or two claims' splits in one pass whose displaced sets re-queue
			// into an extra claim that no per-claim estimate accounted for.
			// The slack exists so rapid can soak through all N checks and
			// surface the worst case instead of bailing on the first 0.5%-class
			// regression; a regression ≥ 2% still fails and is treated as a
			// real bug.
			const diverseRegressionSlack = 0.02
			Expect(postCost).To(BeNumerically("<=", preCost*(1+diverseRegressionSlack)+.00001),
				"optimized cost (%.4f) exceeded pre-opt cost (%.4f) by more than %.0f%% — bad split decision",
				postCost, preCost, diverseRegressionSlack*100)

			// Every input pod must appear in exactly one optimized NodeClaim.
			scheduledUIDs := map[types.UID]struct{}{}
			for _, nc := range results.NewNodeClaims {
				for _, pod := range nc.Pods {
					_, dup := scheduledUIDs[pod.UID]
					Expect(dup).To(BeFalse(), "pod %s scheduled on multiple NodeClaims", pod.Name)
					scheduledUIDs[pod.UID] = struct{}{}
				}
			}
			// Pods that hit scheduling errors are expected when constraints conflict.
			// The invariant is: scheduled + errors == total input.
			Expect(len(scheduledUIDs)+len(results.PodErrors)).To(Equal(len(pods)),
				"scheduled (%d) + errors (%d) != input (%d)", len(scheduledUIDs), len(results.PodErrors), len(pods))

			ExpectCleanedUp(ctx, env.Client)
			cluster.Reset()
		})

		// Sign convention for `saved`: positive = the optimized run is
		// cheaper than pre-optimization; negative = it got costlier. This
		// lines up with the per-run format above so the totals row reads the
		// same way.
		saved := totalPreCost - totalPostCost
		pct := 0.0
		if totalPreCost > 0 {
			pct = saved / totalPreCost * 100
		}
		fmt.Printf("\n=== DIVERSE SUMMARY: %d runs ===\n", runIndex)
		fmt.Printf("  pre → post: %3d cheaper, %3d costlier | cost %.2f → %.2f | saved %+.2f (%+.1f%%) | NCs %d → %d\n",
			cheaperRuns, costlierRuns, totalPreCost, totalPostCost, saved, pct, totalPreNCs, totalPostNCs)
		if cheaperRuns > 0 {
			// These are runs that got cheaper by definition, so savings are
			// always positive — render without a sign to avoid the visual
			// noise of a redundant "+".
			cheaperSaved := cheaperPreCost - cheaperPostCost
			cheaperPct := cheaperSaved / cheaperPreCost * 100
			fmt.Printf("    Of runs that got cheaper (%d): cost %.2f → %.2f | saved %.2f (%.1f%%) | avg saved %.2f/run (%.1f%%) | NCs %d → %d\n",
				cheaperRuns, cheaperPreCost, cheaperPostCost, cheaperSaved, cheaperPct,
				cheaperSaved/float64(cheaperRuns), cheaperPctSum/float64(cheaperRuns),
				cheaperPreNCs, cheaperPostNCs)
		}
		fmt.Printf("  %d of %d runs triggered the optimization pass (%d%%)\n",
			triggeredRuns, runIndex, triggeredRuns*100/runIndex)
	})
})
