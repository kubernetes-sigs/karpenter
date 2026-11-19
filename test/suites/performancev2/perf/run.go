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

package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

const ScenarioLabel = "perf.karpenter.sh/scenario"

// Scenario is a provider-agnostic workload along one vector. Summary and the phase summaries say what it
// stresses and why; what each metric means comes from the Metric, so alarms and the inventory are generated.
type Scenario struct {
	Name    string
	Summary string
	Vectors map[Vector]int
	Sweep   map[Vector][]int
	Phases  []PhaseSpec
	Run     func(*Run)
}

type PhaseSpec struct {
	Name    string
	Summary string
}

// Run is one execution of a scenario against a provider. Objects created through it are deleted
// afterwards, so providers don't need their own cleanup.
type Run struct {
	Provider  *Provider
	Scenario  *Scenario
	Sizes     map[Vector]int
	NodeClass client.Object
	NodePool  *v1.NodePool

	ctx       context.Context
	created   []client.Object
	nodePools sets.Set[string]
	sweep     bool
	off       string
	breaches  []string
}

func newRun(ctx context.Context, p *Provider, s *Scenario, cell map[Vector]int) *Run {
	GinkgoHelper()
	nc := p.NodeClass(s)
	np := p.NodePool(s, nc)
	sizes := map[Vector]int{}
	for v, def := range s.Vectors {
		n, err := p.size(s.Name, v, def)
		Expect(err).ToNot(HaveOccurred())
		sizes[v] = n
		if n, ok := cell[v]; ok {
			sizes[v] = n
		}
	}
	return &Run{Provider: p, Scenario: s, Sizes: sizes, NodeClass: nc, NodePool: np, ctx: ctx, nodePools: sets.New[string](), sweep: cell != nil}
}

// Size is the run's size along a vector the scenario declares.
func (r *Run) Size(v Vector) int {
	GinkgoHelper()
	n, ok := r.Sizes[v]
	Expect(ok).To(BeTrue(), "scenario %s doesn't declare vector %s", r.Scenario.Name, v)
	return n
}

// Deployment builds one of the scenario's deployments. The provider's Shapes can override cpu and memory.
func (r *Run) Deployment(name string, replicas int, cpu, memory string, opts ...test.DeploymentOptionModifier) *appsv1.Deployment {
	if sh, ok := r.Provider.Shapes[r.Scenario.Name+"/"+name]; ok {
		cpu, memory = lo.CoalesceOrEmpty(sh.CPU, cpu), lo.CoalesceOrEmpty(sh.Memory, memory)
	}
	opts = append(opts, test.WithLabels(map[string]string{ScenarioLabel: r.Scenario.Name}))
	return test.Deployment(test.CreateDeploymentOptions(fmt.Sprintf("%s-%s", r.Scenario.Name, name), int32(replicas), cpu, memory, opts...)) //nolint:gosec // scenario sizes are far below MaxInt32
}

func (r *Run) label(o client.Object) {
	l := o.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[ScenarioLabel] = r.Scenario.Name
	o.SetLabels(l)
}

func (r *Run) expectClean() {
	GinkgoHelper()
	pools := &v1.NodePoolList{}
	Expect(r.Provider.Client.List(r.ctx, pools, client.HasLabels{ScenarioLabel})).To(Succeed())
	Expect(pools.Items).To(BeEmpty(), "NodePools left over from an earlier perf scenario")
	pods := &corev1.PodList{}
	Expect(r.Provider.Client.List(r.ctx, pods, client.HasLabels{ScenarioLabel})).To(Succeed())
	Expect(pods.Items).To(BeEmpty(), "pods left over from an earlier perf scenario")
}

func (r *Run) Create(objs ...client.Object) {
	GinkgoHelper()
	for _, o := range objs {
		r.label(o)
		Expect(r.Provider.Client.Create(r.ctx, o)).To(Succeed())
		r.created = append(r.created, o)
		if np, ok := o.(*v1.NodePool); ok {
			r.nodePools.Insert(np.Name)
		}
	}
}

// CreateParallel creates many objects with bounded concurrency, for vectors like NodePool count.
func (r *Run) CreateParallel(objs []client.Object) {
	GinkgoHelper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, o := range objs {
		r.label(o)
	}
	work := make(chan client.Object)
	for range 32 {
		wg.Go(func() {
			for o := range work {
				if err := r.Provider.Client.Create(r.ctx, o); err != nil {
					mu.Lock()
					firstErr = err
					mu.Unlock()
				}
			}
		})
	}
	for _, o := range objs {
		work <- o
	}
	close(work)
	wg.Wait()
	Expect(firstErr).ToNot(HaveOccurred())
	r.created = append(r.created, objs...)
	for _, o := range objs {
		if np, ok := o.(*v1.NodePool); ok {
			r.nodePools.Insert(np.Name)
		}
	}
}

// AwaitNodePoolsReady waits until every NodePool the run created is Ready. Start phases after it, so they
// measure provisioning rather than how fast Karpenter marks new NodePools Ready.
func (r *Run) AwaitNodePoolsReady(timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		pools := &v1.NodePoolList{}
		g.Expect(r.Provider.Client.List(r.ctx, pools, client.MatchingLabels{ScenarioLabel: r.Scenario.Name})).To(Succeed())
		ready := 0
		for i := range pools.Items {
			if pools.Items[i].StatusConditions().Root().IsTrue() {
				ready++
			}
		}
		g.Expect(ready).To(Equal(r.nodePools.Len()), "Ready NodePools")
	}).WithTimeout(r.timeout(timeout)).WithPolling(2 * time.Second).Should(Succeed())
}

func (r *Run) timeout(d time.Duration) time.Duration {
	return time.Duration(float64(d) * r.Provider.timeoutScale())
}

func (r *Run) Scale(replicas int, deps ...*appsv1.Deployment) {
	GinkgoHelper()
	for _, d := range deps {
		base := d.DeepCopy()
		d.Spec.Replicas = new(int32(replicas)) //nolint:gosec // scenario sizes are far below MaxInt32
		Expect(r.Provider.Client.Patch(r.ctx, d, client.MergeFrom(base))).To(Succeed())
	}
}

func (r *Run) cleanup(ctx context.Context) {
	c := r.Provider.Client
	r.ctx = ctx
	for _, v := range slices.Backward(r.created) {
		_ = client.IgnoreNotFound(c.Delete(r.ctx, v, client.PropagationPolicy(metav1.DeletePropagationBackground)))
	}
	Eventually(func(g Gomega) {
		g.Expect(client.IgnoreNotFound(c.DeleteAllOf(r.ctx, &corev1.Pod{}, client.InNamespace("default"),
			client.MatchingLabels{ScenarioLabel: r.Scenario.Name}, client.GracePeriodSeconds(0)))).To(Succeed())
		g.Expect(r.scenarioPods()).To(BeEmpty())
		g.Expect(r.nodes()).To(BeEmpty())
		for _, o := range r.created {
			err := c.Get(r.ctx, client.ObjectKeyFromObject(o), o.DeepCopyObject().(client.Object))
			g.Expect(errors.IsNotFound(err)).To(BeTrue(), "%T %s still exists", o, o.GetName())
		}
	}).WithTimeout(30 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
}

func (r *Run) scenarioPods() []corev1.Pod {
	pods := &corev1.PodList{}
	Expect(r.Provider.Client.List(r.ctx, pods, client.MatchingLabels{ScenarioLabel: r.Scenario.Name})).To(Succeed())
	return pods.Items
}

func (r *Run) nodes() []corev1.Node {
	nodes := &corev1.NodeList{}
	Expect(r.Provider.Client.List(r.ctx, nodes, client.HasLabels{v1.NodePoolLabelKey})).To(Succeed())
	var out []corev1.Node
	for _, n := range nodes.Items {
		if r.nodePools.Has(n.Labels[v1.NodePoolLabelKey]) {
			out = append(out, n)
		}
	}
	return out
}

func (r *Run) healthyPods() int {
	n := 0
	for _, p := range r.scenarioPods() {
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && podReady(&p) {
			n++
		}
	}
	return n
}

func (r *Run) podReadyQuantiles(since time.Time) Quantiles {
	var secs []float64
	for _, p := range r.scenarioPods() {
		if p.CreationTimestamp.Time.Before(since.Truncate(time.Second)) || p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				secs = append(secs, c.LastTransitionTime.Sub(p.CreationTimestamp.Time).Seconds())
			}
		}
	}
	if len(secs) == 0 {
		return Quantiles{}
	}
	sort.Float64s(secs)
	at := func(q float64) float64 { return secs[int(math.Ceil(q*float64(len(secs))))-1] }
	return Quantiles{Count: len(secs), P50: at(0.50), P95: at(0.95), P99: at(0.99), Max: secs[len(secs)-1]}
}

// podSpan is the API server's view of a scale-out: the earliest creation and the latest Ready transition of the
// scenario pods created since the phase started.
func (r *Run) podSpan(since time.Time) (first, last time.Time, ok bool) {
	for _, p := range r.scenarioPods() {
		if p.CreationTimestamp.Time.Before(since.Truncate(time.Second)) || p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				if !ok || p.CreationTimestamp.Time.Before(first) {
					first = p.CreationTimestamp.Time
				}
				if !ok || c.LastTransitionTime.After(last) {
					last = c.LastTransitionTime.Time
				}
				ok = true
			}
		}
	}
	return first, last, ok
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *Run) awaitHealthyPods(n int, timeout time.Duration) {
	GinkgoHelper()
	Eventually(r.healthyPods).WithTimeout(timeout).WithPolling(2*time.Second).Should(Equal(n), "healthy %s pods", r.Scenario.Name)
}

func (r *Run) disrupting() int {
	n := 0
	for _, node := range r.nodes() {
		if node.DeletionTimestamp != nil || hasTaint(&node, v1.DisruptedTaintKey) {
			n++
		}
	}
	return n
}

func hasTaint(n *corev1.Node, key string) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == key {
			return true
		}
	}
	return false
}

func (r *Run) reserved() (cpu, memory float64) {
	var allocCPU, allocMem, reqCPU, reqMem int64
	nodes := sets.New[string]()
	for _, n := range r.nodes() {
		allocCPU += n.Status.Allocatable.Cpu().MilliValue()
		allocMem += n.Status.Allocatable.Memory().Value()
		nodes.Insert(n.Name)
	}
	pods := &corev1.PodList{}
	Expect(r.Provider.Client.List(r.ctx, pods)).To(Succeed())
	for _, p := range pods.Items {
		if !nodes.Has(p.Spec.NodeName) || p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			reqCPU += c.Resources.Requests.Cpu().MilliValue()
			reqMem += c.Resources.Requests.Memory().Value()
		}
	}
	if allocCPU > 0 {
		cpu = float64(reqCPU) / float64(allocCPU)
	}
	if allocMem > 0 {
		memory = float64(reqMem) / float64(allocMem)
	}
	return cpu, memory
}

// phaseID resolves a phase's gates. An unlisted phase is measured only; an Off phase, and every phase after it,
// is skipped.
func (r *Run) phaseID(name string) (id string, gates []Gate, skip bool) {
	GinkgoHelper()
	id = r.Scenario.Name + "/" + name
	_, declared := r.Scenario.phase(name)
	Expect(declared).To(BeTrue(), "scenario %s runs phase %q but doesn't declare it in Phases", r.Scenario.Name, name)
	gates = r.Provider.Levels[id]
	if r.off == "" && isOff(gates) {
		r.off = id
	}
	if r.off != "" {
		GinkgoWriter.Printf("perf %s: skipped, %s is off for provider %s\n", id, r.off, r.Provider.Name)
		return id, nil, true
	}
	return id, gates, false
}

func (r *Run) finish(id string, gates []Gate, rep *Result) *Result {
	GinkgoHelper()
	rep.SchemaVersion, rep.Provider, rep.ID, rep.Scenario, rep.Sizes, rep.Time = SchemaVersion, r.Provider.Name, id, r.Scenario.Name, r.Sizes, time.Now()
	_, rep.Phase, _ = strings.Cut(id, "/")
	rep.Gates = evaluate(r.Provider, gates, id, rep)
	write(rep)
	writeSummary(rep, r.repro())
	for _, sink := range r.Provider.Sinks {
		if err := sink(r.ctx, rep); err != nil {
			GinkgoWriter.Printf("perf %s: sink failed: %v\n", id, err)
		}
	}
	if !r.sweep {
		assertGates(r, id, rep.Gates)
	}
	return rep
}

// repro is the command that reruns this spec: the perf environment of this run, and the flags its scenarios need.
func (r *Run) repro() string {
	var env []string
	for _, k := range append([]string{"KARPENTER_PERF_PROVIDER", "KARPENTER_PERF_SIZES", "KARPENTER_PERF_THRESHOLDS", "KARPENTER_PERF_SWEEP", "KARPENTER_PERF_SWEEP_GRID"}, r.Provider.ReproEnv...) {
		if v := os.Getenv(k); v != "" {
			env = append(env, fmt.Sprintf("%s='%s'", k, v))
		}
	}
	env = append(env, "OUTPUT_DIR=$(mktemp -d)")
	return fmt.Sprintf("%s go test -count 1 -timeout 3h ./test/suites/performancev2/... --ginkgo.timeout=3h --ginkgo.focus='%s'", strings.Join(env, " "), r.Scenario.Name)
}

func reportBase(rep *Result) string {
	return strings.NewReplacer("/", "_").Replace(fmt.Sprintf("%s_%s_%s", rep.Provider, rep.ID, sizesLabel(rep.Sizes, "-", "_")))
}

func write(rep *Result) {
	GinkgoWriter.Printf("perf %s [%s]: total=%s active=%s pods=%d nodes=%d pod_ready_p95/p99/max=%.0f/%.0f/%.0fs heap_p95=%.1fMB cpu_avg=%.3f startup=%.1fs samples=%d\n",
		rep.ID, sizesLabel(rep.Sizes, "=", " "), rep.Total.Round(time.Second), rep.Active.Round(time.Second), rep.Pods, rep.Nodes,
		rep.PodReady.P95, rep.PodReady.P99, rep.PodReady.Max, rep.Resources.P95HeapMB, rep.Resources.AvgCPUCores, rep.StartupSeconds, rep.Resources.Samples)
	dir := os.Getenv("OUTPUT_DIR")
	if dir == "" {
		return
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	Expect(err).ToNot(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(filepath.Clean(dir), filepath.Base(reportBase(rep)+"_performance_report.json")), b, 0o600)).To(Succeed())
}

// writeSummary appends the phase's gates as a markdown table to $GITHUB_STEP_SUMMARY and $OUTPUT_DIR/perf_summary.md,
// so a run's results show without opening its logs.
func writeSummary(rep *Result, repro string) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n### %s (%s, %s)\n\ntotal %s, pod_ready p95 %.0fs, heap p95 %.1f MB, cpu %.3f cores\n\n", rep.ID, rep.Provider, sizesLabel(rep.Sizes, "=", " "),
		rep.Total.Round(time.Second), rep.PodReady.P95, rep.Resources.P95HeapMB, rep.Resources.AvgCPUCores)
	if len(rep.Gates) > 0 {
		b.WriteString("| metric | value | bound | tier | result |\n|---|---|---|---|---|\n")
		for _, g := range rep.Gates {
			op, result := ">", "ok"
			if g.Upper {
				op = "<"
			}
			if !g.Passed {
				result = map[Tier]string{TierStable: "**FAIL**", TierExperimental: "warn"}[g.Tier]
			}
			tier := string(g.Tier)
			if g.Overridden {
				tier += " (overridden)"
			}
			fmt.Fprintf(&b, "| %s | %.4g | %s %.4g | %s | %s |\n", g.Metric, g.Value, op, g.Bound, tier, result)
		}
	}
	fmt.Fprintf(&b, "\nreproduce: `%s`\n", repro)
	for _, path := range []string{os.Getenv("GITHUB_STEP_SUMMARY"), lo.Ternary(os.Getenv("OUTPUT_DIR") != "", filepath.Join(os.Getenv("OUTPUT_DIR"), "perf_summary.md"), "")} {
		if path == "" {
			continue
		}
		f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			GinkgoWriter.Printf("perf summary %s: %v\n", path, err)
			continue
		}
		_, _ = f.WriteString(b.String())
		_ = f.Close()
	}
}
