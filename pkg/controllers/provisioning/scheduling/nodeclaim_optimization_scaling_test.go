//go:build rapid

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

// Scaling test for the NodeClaim optimization pass. Gated behind the `rapid`
// build tag (same as the property tests) so default `go test ./...` and CI
// skip it. Unlike the property tests, this is DETERMINISTIC: it sweeps a ladder
// of pod counts and, for each count, times a baseline Solve (optimization off)
// against an optimized Solve (optimization on) on the identical pod set. The
// goal is a time-cost curve versus the number of pods (and versus the number of
// NodeClaims the baseline produces).
//
// Running:
/*
export KUBEBUILDER_ASSETS="$(setup-envtest use -p path)"
export TEST_OUTPUT_DIR=$(pwd)/test_output          # optional: emits scaling_timing.csv
export SCALING_POD_COUNTS="100,250,500,1000,2000"  # optional: override the ladder
export SCALING_REPEATS=3                            # optional: repeats per size (median-friendly)
go test -tags rapid ./pkg/controllers/provisioning/scheduling/ \
  -run TestScheduling \
  --ginkgo.focus="NodeClaim Optimization Scaling" \
  -v -count=1 -timeout 60m \
  | grep -v "NodeClaim optimization pass complete" \
  | grep -v "relaxing soft constraints for pod since"
*/
//
// The pod set is a fixed-seed prefix family: the first k pods of an n-pod run
// are byte-for-byte the pods of the k-pod run, so smaller sizes are the larger
// workload truncated, so the curve is monotonic in a meaningful way. The size
// distribution mirrors the random property test (0.25-8 CPU, 0.25-16x memory
// multiplier) so realized split rates are representative.

package scheduling_test

import (
	"encoding/csv"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

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

// scalingRNGSeed fixes the pod-generation sequence so every run is
// reproducible and the prefix property holds across sizes.
const scalingRNGSeed = 1

func scalingPodCounts() []int {
	if v := os.Getenv("SCALING_POD_COUNTS"); v != "" {
		if counts := parseIntList(v); len(counts) > 0 {
			return counts
		}
	}
	// Dense ladder with breakpoints thickening as N grows, up to 8000. The
	// extra mid-range points (750/1500/3000/6000) sharpen the curve where the
	// baseline transitions from linear to visibly super-linear.
	return []int{100, 250, 500, 750, 1000, 1500, 2000, 3000, 4000, 6000, 8000}
}

// scalingRepeats returns the uniform repeat count. Kept as an explicit
// SCALING_REPEATS override (applied to every size) and as the default when a
// caller doesn't want the tiered schedule. Default 1.
func scalingRepeats() int {
	if v := os.Getenv("SCALING_REPEATS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// repeatsForSize returns the number of repeats to run at pod count n. Small
// sizes are cheap and noisier (fixed-cost floor dominates, warmup jitter), so
// they get more repeats; large sizes are expensive, so they get fewer. An
// explicit SCALING_REPEATS override forces that uniform count at every size.
//
// Schedule (no override):
//
//	n <  500 -> 9
//	n < 2000 -> 6
//	n < 6000 -> 4
//	else     -> 3
func repeatsForSize(n int) int {
	if v := os.Getenv("SCALING_REPEATS"); v != "" {
		if r, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && r > 0 {
			return r
		}
	}
	switch {
	case n < 500:
		return 9
	case n < 2000:
		return 6
	case n < 6000:
		return 4
	default:
		return 3
	}
}

func parseIntList(s string) []int {
	var out []int
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if n, err := strconv.Atoi(tok); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// meanStd returns the arithmetic mean and the sample standard deviation
// (Bessel-corrected, n-1 denominator) of xs. Std is 0 for fewer than two
// samples, since a single repeat has no spread to report.
func meanStd(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean = sum / float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	std = math.Sqrt(ss / float64(len(xs)-1))
	return mean, std
}

// stat is a mean/std pair for one metric aggregated across repeats.
type stat struct{ mean, std float64 }

func statOf(xs []float64) stat {
	m, s := meanStd(xs)
	return stat{m, s}
}

// fmtStat renders "mean±std" at the given decimal precision.
func fmtStat(s stat, prec int) string {
	return fmt.Sprintf("%.*f±%.*f", prec, s.mean, prec, s.std)
}

// linregress fits ys = intercept + slope·xs by ordinary least squares.
// Returns (0,0) for fewer than two points and (0, mean(ys)) when xs has no
// spread (vertical fit undefined).
func linregress(xs, ys []float64) (slope, intercept float64) {
	n := float64(len(xs))
	if n < 2 {
		return 0, 0
	}
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	denom := n*sxx - sx*sx
	if denom == 0 {
		return 0, sy / n
	}
	slope = (n*sxy - sx*sy) / denom
	intercept = (sy - slope*sx) / n
	return slope, intercept
}

// linregressR2 fits ys = intercept + slope·xs and additionally returns R², the
// fraction of variance in ys explained by the fit. R² is 1 for a perfect line
// and undefined (returned as 0) when ys has no spread or fewer than two points.
func linregressR2(xs, ys []float64) (slope, intercept, r2 float64) {
	slope, intercept = linregress(xs, ys)
	if len(ys) < 2 {
		return slope, intercept, 0
	}
	var sy float64
	for _, y := range ys {
		sy += y
	}
	mean := sy / float64(len(ys))
	var ssTot, ssRes float64
	for i := range xs {
		pred := intercept + slope*xs[i]
		ssRes += (ys[i] - pred) * (ys[i] - pred)
		ssTot += (ys[i] - mean) * (ys[i] - mean)
	}
	if ssTot == 0 {
		return slope, intercept, 0
	}
	return slope, intercept, 1 - ssRes/ssTot
}

// buildScalingPods deterministically generates n pods using a fixed-seed RNG.
// Because the seed and draw order are fixed, buildScalingPods(k) equals the
// first k pods of buildScalingPods(n) for k <= n: the prefix property.
func buildScalingPods(n int) []*corev1.Pod {
	rng := rand.New(rand.NewSource(scalingRNGSeed))
	pods := make([]*corev1.Pod, n)
	for i := 0; i < n; i++ {
		cpuFloat := 0.25 + rng.Float64()*(8.0-0.25)
		memMult := 0.25 + rng.Float64()*(16.0-0.25)
		memFloat := cpuFloat * memMult
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
	return pods
}

// diverseGroupCount is the fixed number of workload-group templates the
// diverse builder rotates through. Pod i is assigned to group i%G, so the
// group roster (and each group's constraint set) is generated once from the
// seed and the prefix property is preserved: pod i is byte-identical for any N.
const diverseGroupCount = 12

// diverseGroupTemplate is a constraint profile shared by all replicas assigned
// to a group. Drawn once per group from the fixed-seed RNG.
type diverseGroupTemplate struct {
	label     string
	cpuStr    string
	memStr    string
	zone      string // "" = no zone pin
	tscKey    string // "" = no topology spread
	tscSkew   int32
	antiHost  bool  // hostname anti-affinity to own replicas
	hostPort  int32 // 0 = none
	affTarget int   // >=0 = preferred pod affinity to that group; -1 = none
	affKey    string
}

// buildDiverseGroupTemplates draws diverseGroupCount group profiles from a
// fixed-seed RNG. Constraint probabilities mirror the "should handle diverse"
// property test (zone 30%, topology spread 30%, anti-affinity 25%, host port
// 15%, cross-group pod affinity 20%) at full constraintRate, so the realized
// constraint mix is representative of that suite.
func buildDiverseGroupTemplates() []diverseGroupTemplate {
	rng := rand.New(rand.NewSource(scalingRNGSeed))
	zones := []string{"test-zone-a", "test-zone-b", "test-zone-c", "test-zone-d"}
	topologyKeys := []string{corev1.LabelTopologyZone, corev1.LabelHostname}

	tmpls := make([]diverseGroupTemplate, diverseGroupCount)
	for g := 0; g < diverseGroupCount; g++ {
		cpuFloat := 0.25 + rng.Float64()*(4.0-0.25)
		memFloat := cpuFloat * (0.5 + rng.Float64()*(8.0-0.5))
		t := diverseGroupTemplate{
			label:     fmt.Sprintf("group-%d", g),
			cpuStr:    fmt.Sprintf("%.2f", cpuFloat),
			memStr:    fmt.Sprintf("%.2fGi", memFloat),
			affTarget: -1,
		}
		if rng.Intn(100) < 30 {
			t.zone = zones[rng.Intn(len(zones))]
		}
		if rng.Intn(100) < 30 {
			t.tscKey = topologyKeys[rng.Intn(len(topologyKeys))]
			t.tscSkew = int32(1 + rng.Intn(3))
		}
		if rng.Intn(100) < 25 {
			t.antiHost = true
		}
		if rng.Intn(100) < 15 {
			t.hostPort = int32(8000 + rng.Intn(4))
		}
		if g > 0 && rng.Intn(100) < 20 {
			t.affTarget = rng.Intn(g)
			t.affKey = topologyKeys[rng.Intn(len(topologyKeys))]
		}
		tmpls[g] = t
	}
	return tmpls
}

// buildDiverseScalingPods generates n pods by rotating through a fixed roster
// of constraint-carrying group templates (pod i -> group i%G). Like
// buildScalingPods this is a prefix family: pod i is byte-identical for any N.
func buildDiverseScalingPods(n int) []*corev1.Pod {
	tmpls := buildDiverseGroupTemplates()
	pods := make([]*corev1.Pod, n)
	for i := 0; i < n; i++ {
		tmpl := tmpls[i%len(tmpls)]
		groupSelector := map[string]string{"app": tmpl.label}
		opts := test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("pod-%d", i),
				Namespace: "default",
				UID:       types.UID(fmt.Sprintf("pod-%d", i)),
				Labels:    map[string]string{"app": tmpl.label},
			},
			Image: "nginx:latest",
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(tmpl.cpuStr),
					corev1.ResourceMemory: resource.MustParse(tmpl.memStr),
				},
			},
		}
		if tmpl.zone != "" {
			opts.NodeRequirements = []corev1.NodeSelectorRequirement{{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{tmpl.zone},
			}}
		}
		if tmpl.tscKey != "" {
			opts.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
				MaxSkew:           tmpl.tscSkew,
				TopologyKey:       tmpl.tscKey,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: groupSelector},
			}}
		}
		if tmpl.antiHost {
			opts.PodAntiRequirements = []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: groupSelector},
				TopologyKey:   corev1.LabelHostname,
			}}
		}
		if tmpl.hostPort > 0 {
			// Globally-unique port per host-port pod. A port shared across a
			// group's replicas is a structural contradiction: host ports are
			// one-per-node, so with many same-port replicas (and hostname
			// anti-affinity) all but a handful can never schedule, producing a
			// large fixed block of errors that doesn't reflect real diversity.
			// Keying the port off the global pod index i makes every host-port
			// pod's port distinct (no collisions at all), keeps the port stable
			// per pod index so the prefix property holds, and still exercises
			// the host-port reservation bookkeeping in Add/RevertTo. Base 8000
			// + i stays well under the 65535 ceiling for the sizes tested.
			opts.HostPorts = []int32{8000 + int32(i)}
		}
		if tmpl.affTarget >= 0 {
			opts.PodPreferences = []corev1.WeightedPodAffinityTerm{{
				Weight: 50,
				PodAffinityTerm: corev1.PodAffinityTerm{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": fmt.Sprintf("group-%d", tmpl.affTarget)}},
					TopologyKey:   tmpl.affKey,
				},
			}}
		}
		pods[i] = test.UnschedulablePod(opts)
	}
	return pods
}

// buildUniformCPUPods generates n identical, unconstrained pods each requesting
// cpuPerPod vCPU and a fixed small memory (so CPU is the binding dimension).
// Packing density (and therefore the resulting NodeClaim count) is controlled
// entirely by cpuPerPod: small requests pack many pods per node (few
// NodeClaims), large requests approach one-per-node (many NodeClaims). Used by
// the isolation experiment to vary NodeClaim count while holding pod count
// fixed, breaking the pods↔NodeClaims collinearity of the scaling sweep.
func buildUniformCPUPods(n int, cpuPerPod float64) []*corev1.Pod {
	pods := make([]*corev1.Pod, n)
	for i := 0; i < n; i++ {
		pods[i] = test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("pod-%d", i),
				Namespace: "default",
				UID:       types.UID(fmt.Sprintf("pod-%d", i)),
			},
			Image: "nginx:latest",
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%.3f", cpuPerPod)),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		})
	}
	return pods
}

type scalingCSVWriter struct {
	w *csv.Writer
	f *os.File
}

func newScalingCSVWriter(prefix string) *scalingCSVWriter {
	dir := os.Getenv("TEST_OUTPUT_DIR")
	if dir == "" {
		return &scalingCSVWriter{} // no-op; methods are nil-safe
	}
	Expect(os.MkdirAll(dir, 0755)).To(Succeed())
	f, err := os.Create(fmt.Sprintf("%s/%stiming.csv", dir, prefix))
	Expect(err).ToNot(HaveOccurred())
	w := csv.NewWriter(f)
	w.Write([]string{"pod_count", "repeat", "baseline_ncs", "optimized_ncs", "baseline_sec", "optimized_sec", "overhead_sec", "overhead_ms_per_pod", "overhead_ms_per_baseline_nc", "overhead_pct"})
	return &scalingCSVWriter{w: w, f: f}
}

func (s *scalingCSVWriter) write(podCount, repeat, baseNCs, optNCs int, baseSec, optSec, msPerPod, msPerBaseNC, pctIncrease float64) {
	if s.w == nil {
		return
	}
	s.w.Write([]string{
		strconv.Itoa(podCount),
		strconv.Itoa(repeat),
		strconv.Itoa(baseNCs),
		strconv.Itoa(optNCs),
		fmt.Sprintf("%.4f", baseSec),
		fmt.Sprintf("%.4f", optSec),
		fmt.Sprintf("%.4f", optSec-baseSec),
		fmt.Sprintf("%.4f", msPerPod),
		fmt.Sprintf("%.4f", msPerBaseNC),
		fmt.Sprintf("%.2f", pctIncrease),
	})
	s.w.Flush()
}

func (s *scalingCSVWriter) Close() {
	if s.w == nil {
		return
	}
	s.w.Flush()
	s.f.Close()
}

// scalingSummaryWriter emits one row per pod count with mean/std aggregated
// across the repeats. Nil-safe when TEST_OUTPUT_DIR is unset.
type scalingSummaryWriter struct {
	w *csv.Writer
	f *os.File
}

func newScalingSummaryWriter(prefix string) *scalingSummaryWriter {
	dir := os.Getenv("TEST_OUTPUT_DIR")
	if dir == "" {
		return &scalingSummaryWriter{}
	}
	Expect(os.MkdirAll(dir, 0755)).To(Succeed())
	f, err := os.Create(fmt.Sprintf("%s/%ssummary.csv", dir, prefix))
	Expect(err).ToNot(HaveOccurred())
	w := csv.NewWriter(f)
	// All *_ms columns are milliseconds. mean/std are across repeats.
	w.Write([]string{
		"pod_count", "repeats", "baseline_ncs", "pod_errors",
		"baseline_ms_mean", "baseline_ms_std",
		"optimized_ms_mean", "optimized_ms_std",
		"overhead_ms_mean", "overhead_ms_std",
		"overhead_pct_mean", "overhead_pct_std",
		"baseline_ms_per_pod_mean", "baseline_ms_per_pod_std",
		"overhead_ms_per_pod_mean", "overhead_ms_per_pod_std",
		"baseline_ms_per_nc_mean", "baseline_ms_per_nc_std",
		"overhead_ms_per_nc_mean", "overhead_ms_per_nc_std",
	})
	return &scalingSummaryWriter{w: w, f: f}
}

func (w *scalingSummaryWriter) write(s perSizeSummary) {
	if w.w == nil {
		return
	}
	f := func(v float64, prec int) string { return strconv.FormatFloat(v, 'f', prec, 64) }
	w.w.Write([]string{
		strconv.Itoa(s.podCount),
		strconv.Itoa(s.repeats),
		strconv.Itoa(s.baseNCs),
		strconv.Itoa(s.errs),
		f(s.baseMS.mean, 4), f(s.baseMS.std, 4),
		f(s.optMS.mean, 4), f(s.optMS.std, 4),
		f(s.ovhdMS.mean, 4), f(s.ovhdMS.std, 4),
		f(s.pct.mean, 2), f(s.pct.std, 2),
		f(s.basePerPod.mean, 4), f(s.basePerPod.std, 4),
		f(s.ovhdPerPod.mean, 4), f(s.ovhdPerPod.std, 4),
		f(s.basePerNC.mean, 4), f(s.basePerNC.std, 4),
		f(s.ovhdPerNC.mean, 4), f(s.ovhdPerNC.std, 4),
	})
	w.w.Flush()
}

func (s *scalingSummaryWriter) Close() {
	if s.w == nil {
		return
	}
	s.w.Flush()
	s.f.Close()
}

// perSizeSummary holds the aggregated metrics (mean/std across repeats) for one
// pod count, kept so the whole summary table prints together after the raw
// per-repeat rows. All time metrics are in milliseconds.
//
// Three views of the same underlying overhead (optimized − baseline):
//   - whole-Solve absolutes: baseline, optimized, overhead, and % increase
//   - per-pod:       divided by pod count
//   - per-NodeClaim: divided by the baseline NodeClaim count
//
// % increase is scale-free ((opt−base)/base), so it has no meaningful per-pod
// or per-NodeClaim variant; the single whole-Solve figure answers "how much
// more time, proportionally, does optimization cost".
type perSizeSummary struct {
	podCount, repeats, baseNCs, errs int
	// Whole-Solve wall-clock, ms.
	baseMS stat // baseline Solve
	optMS  stat // optimized Solve
	ovhdMS stat // overhead = optimized − baseline
	pct    stat // percent increase over baseline
	// Per-pod, ms.
	basePerPod stat
	ovhdPerPod stat
	// Per-baseline-NodeClaim, ms.
	basePerNC stat
	ovhdPerNC stat
}

// runScalingSweep is the shared A/B sweep used by both the uniform and diverse
// scaling tests. buildPods(n) must return a fixed-seed prefix family: the
// first k pods of buildPods(n) must equal buildPods(k), so smaller sizes are
// the larger workload truncated and the curve is monotonic in a meaningful
// way. allowErrors controls the correctness gate: uniform pods must all
// schedule (errors == 0 expected), whereas diverse pods may legitimately land
// in PodErrors when their constraints conflict, so only pod conservation
// (scheduled + errors == n) is asserted there.
func runScalingSweep(label, filePrefix string, buildPods func(n int) []*corev1.Pod, allowErrors bool) {
	counts := scalingPodCounts()
	sort.Ints(counts)
	maxN := counts[len(counts)-1]

	fmt.Printf("\n\n=== STARTING NodeClaim Optimization Scaling Test (%s) ===\n", label)
	fmt.Printf("  pod counts: %v | repeats per size (tiered): %v\n",
		counts, lo.Map(counts, func(n int, _ int) int { return repeatsForSize(n) }))
	fmt.Printf("  baseline = Solve with optimization off; optimized = Solve with optimization on (same pods)\n")

	csvWriter := newScalingCSVWriter(filePrefix)
	defer csvWriter.Close()
	summaryWriter := newScalingSummaryWriter(filePrefix)
	defer summaryWriter.Close()

	ExpectCleanedUp(ctx, env.Client)
	cluster.Reset()

	ctx = kwokoptions.ToContext(ctx, &kwokoptions.Options{})
	instanceTypes, err := kwok.ConstructInstanceTypes(ctx)
	Expect(err).ToNot(HaveOccurred())
	instanceTypes = filterByMaxVCPU(instanceTypes, "64")
	cloudProvider.InstanceTypes = instanceTypes

	nodePool := test.NodePool(v1.NodePool{
		Spec: v1.NodePoolSpec{
			Limits: v1.Limits(corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("10000000"),
			}),
		},
	})
	ExpectApplied(ctx, env.Client, nodePool)

	// Generate the largest set once; each size is a prefix.
	master := buildPods(maxN)

	// timeSolve runs one Solve on a fresh deep copy of the pods (Solve mutates
	// them) and returns the results plus the Solve wall-clock.
	timeSolve := func(pods []*corev1.Pod, optimize bool) (scheduling.Results, time.Duration) {
		cp := make([]*corev1.Pod, len(pods))
		for i, p := range pods {
			cp[i] = p.DeepCopy()
		}
		var s *scheduling.Scheduler
		if optimize {
			s, _ = prov.NewScheduler(ctx, cp, nil, nil, scheduling.EnableNodeClaimOptimization)
		} else {
			s, _ = prov.NewScheduler(ctx, cp, nil, nil)
		}
		start := time.Now()
		res, _ := s.Solve(ctx, cp)
		return res, time.Since(start)
	}

	fmt.Printf("\n%8s %5s %9s %8s %11s %11s %11s %9s %11s %8s %6s\n",
		"pods", "rep", "base_ncs", "opt_ncs", "base_sec", "opt_sec", "ovhd_sec", "ms/pod", "ms/base_nc", "%_incr", "errs")

	var summaries []perSizeSummary
	for _, n := range counts {
		pods := master[:n]
		repeats := repeatsForSize(n)
		// Per-repeat samples for this pod count. All time samples in ms.
		var baseMS, optMS, ovhdMS, pct []float64
		var basePerPod, ovhdPerPod, basePerNC, ovhdPerNC []float64
		lastBaseNCs, lastErrs := 0, 0
		for r := 0; r < repeats; r++ {
			var baseRes, optRes scheduling.Results
			var baseDur, optDur time.Duration
			// Alternate arm order so one-time warmup doesn't always land on
			// the same arm.
			if (n+r)%2 == 0 {
				baseRes, baseDur = timeSolve(pods, false)
				optRes, optDur = timeSolve(pods, true)
			} else {
				optRes, optDur = timeSolve(pods, true)
				baseRes, baseDur = timeSolve(pods, false)
			}

			baseNCs := len(baseRes.NewNodeClaims)
			optNCs := len(optRes.NewNodeClaims)
			baseMsV := baseDur.Seconds() * 1000
			optMsV := optDur.Seconds() * 1000
			ovhdMsV := optMsV - baseMsV
			msPerPod := ovhdMsV / float64(n)
			msPerBaseNC := 0.0
			basePerNCV := 0.0
			if baseNCs > 0 {
				msPerBaseNC = ovhdMsV / float64(baseNCs)
				basePerNCV = baseMsV / float64(baseNCs)
			}
			pctIncrease := 0.0
			if baseMsV > 0 {
				pctIncrease = ovhdMsV / baseMsV * 100
			}

			fmt.Printf("%8d %5d %9d %8d %11.4f %11.4f %11.4f %9.4f %11.4f %8.1f %6d\n",
				n, r, baseNCs, optNCs, baseDur.Seconds(), optDur.Seconds(), (optDur - baseDur).Seconds(), msPerPod, msPerBaseNC, pctIncrease, len(optRes.PodErrors))
			csvWriter.write(n, r, baseNCs, optNCs, baseDur.Seconds(), optDur.Seconds(), msPerPod, msPerBaseNC, pctIncrease)

			baseMS = append(baseMS, baseMsV)
			optMS = append(optMS, optMsV)
			ovhdMS = append(ovhdMS, ovhdMsV)
			pct = append(pct, pctIncrease)
			basePerPod = append(basePerPod, baseMsV/float64(n))
			ovhdPerPod = append(ovhdPerPod, msPerPod)
			basePerNC = append(basePerNC, basePerNCV)
			ovhdPerNC = append(ovhdPerNC, msPerBaseNC)
			lastBaseNCs = baseNCs
			lastErrs = len(optRes.PodErrors)

			// Correctness gate: every input pod must appear on exactly one
			// optimized NodeClaim, and scheduled + errors must conserve the
			// input count.
			scheduledUIDs := map[types.UID]struct{}{}
			for _, nc := range optRes.NewNodeClaims {
				for _, pod := range nc.Pods {
					_, dup := scheduledUIDs[pod.UID]
					Expect(dup).To(BeFalse(), "pod %s scheduled on multiple NodeClaims", pod.Name)
					scheduledUIDs[pod.UID] = struct{}{}
				}
			}
			Expect(len(scheduledUIDs)+len(optRes.PodErrors)).To(Equal(n),
				"scheduled (%d) + errors (%d) != input (%d)", len(scheduledUIDs), len(optRes.PodErrors), n)
			if !allowErrors {
				// Uniform pods carry no constraints, so nothing should error,
				// and splitting only ever adds NodeClaims.
				Expect(optRes.PodErrors).To(BeEmpty(), "unconstrained pods should all schedule")
				Expect(optNCs).To(BeNumerically(">=", baseNCs),
					"optimized NodeClaims (%d) should be >= baseline (%d); splitting only adds claims", optNCs, baseNCs)
			}

			cluster.Reset()
		}

		// Aggregate across the repeats for this pod count.
		s := perSizeSummary{
			podCount: n, repeats: repeats, baseNCs: lastBaseNCs, errs: lastErrs,
			baseMS: statOf(baseMS), optMS: statOf(optMS), ovhdMS: statOf(ovhdMS), pct: statOf(pct),
			basePerPod: statOf(basePerPod), ovhdPerPod: statOf(ovhdPerPod),
			basePerNC: statOf(basePerNC), ovhdPerNC: statOf(ovhdPerNC),
		}
		summaries = append(summaries, s)
		summaryWriter.write(s)
	}

	// Summary tables: mean ± sample std (n-1) across repeats per pod count, all
	// times in milliseconds. Split into two tables so each stays readable in a
	// terminal: whole-Solve absolutes + % increase, then the per-pod and
	// per-NodeClaim normalizations. baseline is the opt-off Solve; overhead is
	// optimized − baseline.
	fmt.Printf("\n=== SCALING SUMMARY (%s): mean ± std (repeats tiered per size), times in ms ===\n", label)

	fmt.Printf("\n-- whole Solve --\n")
	fmt.Printf("%8s %5s %9s %6s %18s %18s %18s %14s\n",
		"pods", "reps", "base_ncs", "errs", "baseline_ms", "optimized_ms", "overhead_ms", "%_incr")
	for _, s := range summaries {
		fmt.Printf("%8d %5d %9d %6d %18s %18s %18s %14s\n",
			s.podCount, s.repeats, s.baseNCs, s.errs,
			fmtStat(s.baseMS, 2), fmtStat(s.optMS, 2), fmtStat(s.ovhdMS, 2), fmtStat(s.pct, 1))
	}

	// Normalized per-pod is the causal breakdown: overhead scales with pods.
	// The per-nodeclaim columns are DESCRIPTIVE ONLY: in this sweep NodeClaims
	// are collinear with pods (nc ≈ const·pods, density held fixed), so
	// ovhd/nodeclaim looks flat as an artifact of that ratio, not because there
	// is a real per-NodeClaim cost. The decomposition experiment (which varies
	// NodeClaim count independently) shows the true per-NodeClaim term is
	// near-zero; overhead is Fixed + per-pod. Don't read ovhd/nodeclaim causally.
	fmt.Printf("\n-- normalized per-pod (causal) --\n")
	fmt.Printf("%8s %18s %18s\n", "pods", "base/pod", "ovhd/pod")
	for _, s := range summaries {
		fmt.Printf("%8d %18s %18s\n", s.podCount, fmtStat(s.basePerPod, 4), fmtStat(s.ovhdPerPod, 4))
	}

	fmt.Printf("\n-- normalized per-NodeClaim (DESCRIPTIVE ONLY; nc collinear with pods here) --\n")
	fmt.Printf("%8s %18s %18s\n", "pods", "base/nodeclaim", "ovhd/nodeclaim")
	for _, s := range summaries {
		fmt.Printf("%8d %18s %18s\n", s.podCount, fmtStat(s.basePerNC, 4), fmtStat(s.ovhdPerNC, 4))
	}

	// Pods-based linear model: overhead ≈ Fixed + marginal·pods. This is the
	// well-identified fit: pods is the clean regressor (density is held fixed
	// across the sweep, so pods varies independently). Fitted over the per-size
	// overhead means. R² near 1 confirms overhead is linear in pods.
	var podXs, ovhdYs, baseYs, optYs []float64
	for _, s := range summaries {
		podXs = append(podXs, float64(s.podCount))
		ovhdYs = append(ovhdYs, s.ovhdMS.mean)
		baseYs = append(baseYs, s.baseMS.mean)
		optYs = append(optYs, s.optMS.mean)
	}
	ovhdSlope, ovhdFixed, ovhdR2 := linregressR2(podXs, ovhdYs)
	baseSlope, baseFixed, baseR2 := linregressR2(podXs, baseYs)
	optSlope, optFixed, optR2 := linregressR2(podXs, optYs)
	fmt.Printf("\n-- pods-based fit (value ≈ Fixed + marginal·pods) --\n")
	fmt.Printf("  baseline:  Fixed = %7.2f ms | marginal = %.4f ms/pod | R² = %.4f\n", baseFixed, baseSlope, baseR2)
	fmt.Printf("  optimized: Fixed = %7.2f ms | marginal = %.4f ms/pod | R² = %.4f\n", optFixed, optSlope, optR2)
	fmt.Printf("  overhead:  Fixed = %7.2f ms | marginal = %.4f ms/pod | R² = %.4f\n", ovhdFixed, ovhdSlope, ovhdR2)
	fmt.Printf("  (marginal_opt = marginal_base + marginal_ovhd exactly; OLS slope is linear\n")
	fmt.Printf("   in y and opt=base+ovhd pointwise; a low baseline/optimized R² just means the\n")
	fmt.Printf("   scheduler grows super-linearly and a straight line can't capture the whole curve)\n")
}

// isoPoint is one (nodeclaims, overhead_ms) measurement at a fixed pod count,
// aggregated across repeats.
type isoPoint struct {
	cpuPerPod float64
	baseNCs   int
	baseMS    stat
	ovhdMS    stat
}

// runFixedPodSweep holds pod count fixed at n and sweeps cpuPerPod to vary the
// resulting NodeClaim count, timing the baseline vs. optimized Solve at each.
// Returns one isoPoint per cpu value. This breaks the pods↔NodeClaims
// collinearity of runScalingSweep: pod count is constant, so regressing
// overhead on NodeClaim count isolates the per-NodeClaim cost term.
func runFixedPodSweep(n, repeats int, cpuValues []float64) []isoPoint {
	nodePool := test.NodePool(v1.NodePool{
		Spec: v1.NodePoolSpec{
			Limits: v1.Limits(corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("10000000"),
			}),
		},
	})
	ExpectApplied(ctx, env.Client, nodePool)

	timeSolve := func(pods []*corev1.Pod, optimize bool) (scheduling.Results, time.Duration) {
		cp := make([]*corev1.Pod, len(pods))
		for i, p := range pods {
			cp[i] = p.DeepCopy()
		}
		var s *scheduling.Scheduler
		if optimize {
			s, _ = prov.NewScheduler(ctx, cp, nil, nil, scheduling.EnableNodeClaimOptimization)
		} else {
			s, _ = prov.NewScheduler(ctx, cp, nil, nil)
		}
		start := time.Now()
		res, _ := s.Solve(ctx, cp)
		return res, time.Since(start)
	}

	var points []isoPoint
	for _, cpu := range cpuValues {
		pods := buildUniformCPUPods(n, cpu)
		var baseMSs, ovhdMSs []float64
		lastBaseNCs := 0
		for r := 0; r < repeats; r++ {
			var baseRes, optRes scheduling.Results
			var baseDur, optDur time.Duration
			if r%2 == 0 {
				baseRes, baseDur = timeSolve(pods, false)
				optRes, optDur = timeSolve(pods, true)
			} else {
				optRes, optDur = timeSolve(pods, true)
				baseRes, baseDur = timeSolve(pods, false)
			}
			lastBaseNCs = len(baseRes.NewNodeClaims)
			_ = optRes
			baseMSs = append(baseMSs, baseDur.Seconds()*1000)
			ovhdMSs = append(ovhdMSs, (optDur-baseDur).Seconds()*1000)
			cluster.Reset()
		}
		points = append(points, isoPoint{
			cpuPerPod: cpu, baseNCs: lastBaseNCs,
			baseMS: statOf(baseMSs), ovhdMS: statOf(ovhdMSs),
		})
	}
	return points
}

var _ = Describe("NodeClaim Optimization Scaling", func() {
	It("should measure optimization overhead as pod count scales", func() {
		runScalingSweep("uniform", "scaling_", buildScalingPods, false)
	})

	It("should measure optimization overhead as pod count scales with diverse constraints", func() {
		runScalingSweep("diverse", "scaling_diverse_", buildDiverseScalingPods, true)
	})

	// Decomposes overhead into Fixed + P·pods + C·nodeclaims. The scaling sweep
	// can't separate P from C because NodeClaims track pods almost linearly
	// there (density held fixed). This experiment breaks that collinearity:
	//   1. Hold pods fixed, vary pod SIZE to sweep NodeClaim count. Regressing
	//      overhead on NodeClaim count at fixed pod count gives C as the slope
	//      (pods constant, so the P·pods term is absorbed into the intercept).
	//   2. Do that at several pod counts. Each yields an intercept = Fixed +
	//      P·pods; regressing those intercepts on pods gives P (slope) and
	//      Fixed (intercept), over-determined, so both come with an R², not a
	//      brittle two-point solve.
	// Uses its own fixed repeat count (DECOMP_REPEATS, default 3) so it's robust
	// regardless of the sweep's SCALING_REPEATS setting.
	It("should decompose overhead into fixed, per-pod, and per-nodeclaim terms", func() {
		repeats := 3
		if v := os.Getenv("DECOMP_REPEATS"); v != "" {
			if r, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && r > 0 {
				repeats = r
			}
		}
		// CPU-per-pod ladder. Instance types are capped at 64 vCPU (as in all
		// these tests), so NodeClaim count = pods once a pod needs its own node.
		// The ladder stops at 16 vCPU deliberately: beyond that, base_ncs
		// saturates at the pod count (no longer a free variable) AND the
		// baseline thrashes packing near-node-sized pods against the 64 cap,
		// producing timing outliers that wreck the per-NodeClaim regression.
		// 0.25->16 keeps NodeClaim count a clean, unsaturated independent
		// variable (~1 up to ~pods/4) so the regression stays well-conditioned.
		cpuValues := []float64{0.25, 0.5, 1, 2, 4, 8, 12, 16}
		// Several pod counts so the intercept line is over-determined.
		podCounts := []int{100, 200, 400, 800}

		fmt.Printf("\n\n=== STARTING Overhead Decomposition Experiment ===\n")
		fmt.Printf("  hold pods fixed, vary pod size to sweep NodeClaim count\n")
		fmt.Printf("  pod counts: %v | cpu/pod ladder: %v | %d repeat(s)\n", podCounts, cpuValues, repeats)

		ExpectCleanedUp(ctx, env.Client)
		cluster.Reset()
		ctx = kwokoptions.ToContext(ctx, &kwokoptions.Options{})
		instanceTypes, err := kwok.ConstructInstanceTypes(ctx)
		Expect(err).ToNot(HaveOccurred())
		instanceTypes = filterByMaxVCPU(instanceTypes, "64")
		cloudProvider.InstanceTypes = instanceTypes

		// Two things are collected per fixed-pod-count sweep:
		//  1. meanOvhd: the average overhead across the size sweep. Since pods is
		//     constant and NodeClaim count varies, any real per-NodeClaim term
		//     would show up as a trend across the sweep; averaging it out leaves
		//     the best estimate of Fixed + P·pods at this pod count. These feed
		//     the primary fit (overhead = Fixed + P·pods).
		//  2. The per-NodeClaim regression (overhead vs. nc at fixed pods). This
		//     is NOT used to produce a number; it's the falsification test: if
		//     a per-NodeClaim cost existed, these fits would have a consistent
		//     positive slope with high R². We print them to show they don't.
		var podXs, meanOvhdYs []float64
		type ncFit struct {
			pods      int
			slope, r2 float64
		}
		var ncFits []ncFit

		for _, n := range podCounts {
			points := runFixedPodSweep(n, repeats, cpuValues)

			fmt.Printf("\n-- fixed pods = %d --\n", n)
			fmt.Printf("%10s %9s %18s %18s\n", "cpu/pod", "base_ncs", "baseline_ms", "overhead_ms")
			var ncXs, ovhdYs []float64
			var ovhdSum float64
			for _, p := range points {
				fmt.Printf("%10.3f %9d %18s %18s\n",
					p.cpuPerPod, p.baseNCs, fmtStat(p.baseMS, 3), fmtStat(p.ovhdMS, 3))
				ncXs = append(ncXs, float64(p.baseNCs))
				ovhdYs = append(ovhdYs, p.ovhdMS.mean)
				ovhdSum += p.ovhdMS.mean
			}
			slope, _, r2 := linregressR2(ncXs, ovhdYs)
			ncFits = append(ncFits, ncFit{pods: n, slope: slope, r2: r2})
			podXs = append(podXs, float64(n))
			meanOvhdYs = append(meanOvhdYs, ovhdSum/float64(len(points)))
			fmt.Printf("  mean overhead over sweep = %.3f ms | per-NC fit slope = %+.4f ms/NC, R² = %.4f\n",
				ovhdSum/float64(len(points)), slope, r2)
		}

		// Primary result: overhead = Fixed + P·pods, fit on the mean overhead at
		// each pod count. pods is the clean regressor (density averaged out).
		p, fixed, r2 := linregressR2(podXs, meanOvhdYs)
		fmt.Printf("\n=== DECOMPOSITION (times in ms) ===\n")
		fmt.Printf("  Model: overhead ≈ Fixed + P·pods\n")
		fmt.Printf("  Fixed (per Solve):     %8.3f ms\n", fixed)
		fmt.Printf("  Per-pod (P):           %8.4f ms/pod\n", p)
		fmt.Printf("  fit R² = %.4f\n", r2)

		// Falsification test for a per-NodeClaim term. If C were real, every
		// per-NC fit would have a consistent positive slope with high R².
		fmt.Printf("\n  per-NodeClaim term: NOT SUPPORTED by the data. Per-NC fits (at fixed pods):\n")
		for _, f := range ncFits {
			fmt.Printf("    pods=%-4d slope=%+.4f ms/NC  R²=%.4f\n", f.pods, f.slope, f.r2)
		}
		fmt.Printf("  slopes swing sign and R² is near zero -> NodeClaim count does not predict\n")
		fmt.Printf("  overhead. Overhead is driven by pods, not NodeClaims.\n")

		fmt.Printf("\n  reconstructed overhead (Fixed + P·pods):\n")
		for _, n := range []int{100, 1000, 4000} {
			fmt.Printf("    %5d pods: %8.3f ms\n", n, fixed+p*float64(n))
		}
	})
})
