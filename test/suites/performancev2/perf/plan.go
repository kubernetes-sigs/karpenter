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
	"sync"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// planSettle covers consolidation draining candidates after the replacement initializes.
const planSettle = 30 * time.Second

// planSlack stays under the scenarios' smallest shape difference (100m: 4x900m+3500m vs 2x3500m).
var planSlack = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")}

// PlanKept compares NodeClaim spec requests (planned pods + DaemonSet overhead) with the pods bound once initialized.
type PlanKept struct {
	Kept    int `json:"plan_kept_nodeclaims"`
	Over    int `json:"plan_over_nodeclaims"`
	Under   int `json:"plan_under_nodeclaims"`
	Unknown int `json:"plan_unknown_nodeclaims"`
}

func planKept(since time.Time, nodeClaims []v1.NodeClaim, bound map[string]corev1.ResourceList) PlanKept {
	var p PlanKept
	for _, nc := range nodeClaims {
		if nc.CreationTimestamp.Time.Before(since.Truncate(time.Second)) {
			continue
		}
		actual, ok := bound[nc.Name]
		if !ok {
			p.Unknown++
			continue
		}
		over, under := false, false
		for name, slack := range planSlack {
			d := actual[name].DeepCopy()
			d.Sub(nc.Spec.Resources.Requests[name])
			over = over || d.Cmp(slack) > 0
			d.Add(slack)
			under = under || d.Sign() < 0
		}
		switch {
		case over:
			p.Over++
		case under:
			p.Under++
		default:
			p.Kept++
		}
	}
	return p
}

type planWatch struct {
	r       *Run
	since   time.Time
	pools   sets.Set[string]
	ctx     context.Context
	cancel  context.CancelFunc
	store   cache.Store
	wg      sync.WaitGroup
	mu      sync.Mutex
	done    bool
	timers  map[string]*time.Timer
	bound   map[string]corev1.ResourceList
	deleted []v1.NodeClaim
}

func (r *Run) watchPlans(since time.Time) *planWatch {
	ctx, cancel := context.WithCancel(r.ctx)
	w := &planWatch{r: r, since: since.Truncate(time.Second), pools: r.nodePools.Clone(), ctx: ctx, cancel: cancel,
		timers: map[string]*time.Timer{}, bound: map[string]corev1.ResourceList{}}
	c := r.Provider.Client
	inf := cache.NewSharedIndexInformer(&cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			o.LabelSelector, o.Watch = v1.NodePoolLabelKey, false
			l := &v1.NodeClaimList{}
			return l, c.List(ctx, l, &client.ListOptions{Raw: &o})
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.LabelSelector = v1.NodePoolLabelKey
			return c.Watch(ctx, &v1.NodeClaimList{}, &client.ListOptions{Raw: &o})
		},
	}, &v1.NodeClaim{}, 0, nil)
	w.store = inf.GetStore()
	lo.Must(inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { w.update(o.(*v1.NodeClaim)) },
		UpdateFunc: func(_, o any) { w.update(o.(*v1.NodeClaim)) },
		DeleteFunc: func(o any) {
			if t, ok := o.(cache.DeletedFinalStateUnknown); ok {
				o = t.Obj
			}
			if nc, ok := o.(*v1.NodeClaim); ok && w.ours(nc) {
				w.mu.Lock()
				if !w.done {
					w.deleted = append(w.deleted, *nc)
				}
				w.mu.Unlock()
			}
		},
	}))
	go inf.RunWithContext(ctx)
	return w
}

func (w *planWatch) ours(nc *v1.NodeClaim) bool {
	return !nc.CreationTimestamp.Time.Before(w.since) && w.pools.Has(nc.Labels[v1.NodePoolLabelKey])
}

func (w *planWatch) update(nc *v1.NodeClaim) {
	if !w.ours(nc) || !nc.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.timers[nc.Name]; ok || w.done {
		return
	}
	w.wg.Add(1)
	w.timers[nc.Name] = time.AfterFunc(planSettle, func() { defer w.wg.Done(); w.sample(nc.Name) })
}

func (w *planWatch) sample(name string) {
	o, ok, _ := w.store.GetByKey(name)
	nc, _ := o.(*v1.NodeClaim)
	if !ok || nc == nil || nc.DeletionTimestamp != nil || nc.Status.NodeName == "" {
		return
	}
	pods := &corev1.PodList{}
	if err := w.r.Provider.Client.List(w.ctx, pods, client.MatchingFields{"spec.nodeName": nc.Status.NodeName}); err != nil {
		return
	}
	live := lo.FilterMap(pods.Items, func(p corev1.Pod, _ int) (*corev1.Pod, bool) {
		return &p, p.DeletionTimestamp == nil && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed
	})
	w.mu.Lock()
	w.bound[name] = resources.RequestsForPods(live...)
	w.mu.Unlock()
}

// stop samples NodeClaims still inside planSettle now; the phase ended settled.
func (w *planWatch) stop() (deleted []v1.NodeClaim, bound map[string]corev1.ResourceList) {
	w.mu.Lock()
	w.done = true
	timers := lo.Assign(w.timers)
	w.mu.Unlock()
	for name, t := range timers {
		if t.Stop() {
			w.sample(name)
			w.wg.Done()
		}
	}
	w.wg.Wait()
	w.cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deleted, w.bound
}
