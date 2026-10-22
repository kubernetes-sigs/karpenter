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
}

func newRun(ctx context.Context, p *Provider, s *Scenario, cell map[Vector]int) *Run {
	nc := p.NodeClass()
	np := p.NodePool(nc)
	sizes := map[Vector]int{}
	for v, def := range s.Vectors {
		sizes[v] = p.size(s.Name, v, def)
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

func (r *Run) Deployment(name string, replicas int, cpu, memory string, opts ...test.DeploymentOptionModifier) *appsv1.Deployment {
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
	}).WithTimeout(timeout).WithPolling(2 * time.Second).Should(Succeed())
}

func (r *Run) Scale(replicas int, deps ...*appsv1.Deployment) {
	GinkgoHelper()
	for _, d := range deps {
		base := d.DeepCopy()
		d.Spec.Replicas = lo.ToPtr(int32(replicas)) //nolint:gosec // scenario sizes are far below MaxInt32
		Expect(r.Provider.Client.Patch(r.ctx, d, client.MergeFrom(base))).To(Succeed())
	}
}

func (r *Run) cleanup(ctx context.Context) {
	c := r.Provider.Client
	r.ctx = ctx
	for i := len(r.created) - 1; i >= 0; i-- {
		_ = client.IgnoreNotFound(c.Delete(r.ctx, r.created[i], client.PropagationPolicy(metav1.DeletePropagationBackground)))
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

func (r *Run) phaseID(name string) (string, []Gate) {
	GinkgoHelper()
	id := r.Scenario.Name + "/" + name
	_, declared := r.Scenario.phase(name)
	Expect(declared).To(BeTrue(), "scenario %s runs phase %q but doesn't declare it in Phases", r.Scenario.Name, name)
	gates, ok := r.Provider.Levels[id]
	Expect(ok).To(BeTrue(), "provider %s has no levels for phase %s; add an entry, even an empty one", r.Provider.Name, id)
	return id, gates
}

func (r *Run) finish(id string, gates []Gate, rep *Result) *Result {
	GinkgoHelper()
	rep.Provider, rep.Phase, rep.Sizes, rep.Time = r.Provider.Name, id, r.Sizes, time.Now()
	rep.Gates = evaluate(gates, id, rep)
	write(rep)
	if !r.sweep {
		assertGates(r, id, rep.Gates)
	}
	return rep
}

func write(rep *Result) {
	GinkgoWriter.Printf("perf %s [%s]: total=%s active=%s pods=%d nodes=%d pod_ready_p95/p99/max=%.0f/%.0f/%.0fs heap_p95=%.1fMB cpu_avg=%.3f startup=%.1fs samples=%d\n",
		rep.Phase, sizesLabel(rep.Sizes, "=", " "), rep.Total.Round(time.Second), rep.Active.Round(time.Second), rep.Pods, rep.Nodes,
		rep.PodReady.P95, rep.PodReady.P99, rep.PodReady.Max, rep.Resources.P95HeapMB, rep.Resources.AvgCPUCores, rep.StartupSeconds, rep.Resources.Samples)
	dir := os.Getenv("OUTPUT_DIR")
	if dir == "" {
		return
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	Expect(err).ToNot(HaveOccurred())
	name := strings.NewReplacer("/", "_").Replace(fmt.Sprintf("%s_%s_%s_performance_report.json", rep.Provider, rep.Phase, sizesLabel(rep.Sizes, "-", "_")))
	Expect(os.WriteFile(filepath.Join(filepath.Clean(dir), filepath.Base(name)), b, 0o600)).To(Succeed())
}
