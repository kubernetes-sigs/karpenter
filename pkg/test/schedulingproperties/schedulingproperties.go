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

// Package schedulingproperties checks Karpenter's provisioning decisions against Kubernetes scheduling semantics
// for randomly generated pod scheduling constraints. Providers run it against their own instance types and labels.
//
// The generator avoids some known Karpenter bugs so the checks pass while they're open. Each workaround is commented
// where it's applied.
package schedulingproperties

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/go-logr/logr"
	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/prop"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	resourcehelper "k8s.io/component-helpers/resource"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	pkgscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
)

const (
	absentLabelKey = "example.com/absent"
	unmatchedValue = "unmatched"
)

// Config describes the provider under test. CloudProvider, InstanceTypes, and Launch are required.
type Config struct {
	// CloudProvider backs the provisioner that schedules each case through Provisioner.Schedule. Its GetInstanceTypes
	// is replaced with the drawn instance types.
	CloudProvider cloudprovider.CloudProvider
	// InstanceTypes the generator draws from. Only single-valued instance type and offering requirements are used as
	// the labels a launched node is known to carry.
	InstanceTypes []*cloudprovider.InstanceType
	// NodePool is the template for generated NodePools. Defaults to a NodePool named "default".
	NodePool *v1.NodePool
	// Launch returns the NodeClaim launched for a scheduled NodeClaim whose instance type requirement names a single
	// instance type, e.g. CloudProvider.Create. Its labels, taints, and allocatable are judged as the node.
	Launch func(context.Context, *v1.NodeClaim) (*v1.NodeClaim, error)
	// RestrictedLabelDomains are added to v1.RestrictedLabelDomains while checking, as real providers do for their
	// own domains.
	RestrictedLabelDomains []string
	// KnownDeviations are label keys the generator doesn't constrain while a deviation is open. Each entry needs a
	// comment above it linking the open issue, so the entry is removed when the issue is fixed.
	KnownDeviations sets.Set[string]
	// Checks is the number of generated cases unless -schedulingproperties.checks is set. Defaults to 100.
	Checks int
}

// ExpectSatisfiablePodsArePlaced checks that Provisioner.Schedule neither ignores nor leaves unplaced pods satisfied by a
// node the NodePool can launch, and that their placements pass ExpectPlacementsFitNodes' checks. ctx must carry
// operator options.
func ExpectSatisfiablePodsArePlaced(ctx context.Context, t TB, cfg Config) {
	t.Helper()
	defer setup(&cfg)()
	check(t, cfg, func(rt *caseT) {
		np, its, pods := drawScenario(rt, cfg, true)
		scenario := describeScenario(np, its)
		results, prov := solve(ctx, rt, cfg, np, its, pods)
		placed := sets.New[types.UID]()
		for _, nc := range results.NewNodeClaims {
			placed.Insert(lo.Map(nc.Pods, func(pod *corev1.Pod, _ int) types.UID { return pod.UID })...)
		}
		for _, pod := range pods {
			if _, errored := lo.FindKeyBy(results.PodErrors, func(p *corev1.Pod, _ error) bool { return p.UID == pod.UID }); !errored && !placed.Has(pod.UID) {
				rt.Fatalf("pod %s satisfiable by a launchable node was ignored: %v\n%s\n%s", pod.Name, prov.Validate(ctx, pod), describe(pod), scenario)
			}
		}
		for pod, err := range results.PodErrors {
			rt.Fatalf("pod %s satisfiable by a launchable node was not placed: %v\n%s\n%s", pod.Name, err, describe(pod), scenario)
		}
		if count := lo.SumBy(results.NewNodeClaims, func(nc *scheduling.NodeClaim) int { return len(nc.Pods) }); count != len(pods) {
			rt.Fatalf("placed %d of %d satisfiable pods\n%s", count, len(pods), scenario)
		}
		checkPlacements(ctx, rt, cfg, np, pods, results, scenario)
	})
}

// ExpectPlacementsFitNodes checks that every pod Provisioner.Schedule places on a new NodeClaim matches the required
// node affinity, taints, and allocatable resources of each instance type the NodeClaim can launch as. ctx must carry
// operator options.
func ExpectPlacementsFitNodes(ctx context.Context, t TB, cfg Config) {
	t.Helper()
	defer setup(&cfg)()
	check(t, cfg, func(rt *caseT) {
		np, its, pods := drawScenario(rt, cfg, false)
		scenario := describeScenario(np, its)
		results, _ := solve(ctx, rt, cfg, np, its, pods)
		checkPlacements(ctx, rt, cfg, np, pods, results, scenario)
	})
}

// setup filters out instance types that can't launch and adds cfg's restricted label domains, returning a func that
// removes them. Checks therefore can't run in parallel with tests that read v1.RestrictedLabelDomains.
func setup(cfg *Config) func() {
	cfg.InstanceTypes = lo.Filter(cfg.InstanceTypes, func(it *cloudprovider.InstanceType, _ int) bool { return len(it.Offerings.Available()) > 0 })
	added := lo.Filter(cfg.RestrictedLabelDomains, func(d string, _ int) bool { return !v1.RestrictedLabelDomains.Has(d) })
	v1.RestrictedLabelDomains.Insert(added...)
	return func() { v1.RestrictedLabelDomains.Delete(added...) }
}

// drawScenario draws a NodePool, its instance types, and pods. When satisfiable is true, every pod is satisfied by
// a node that the NodePool can launch from one of the instance types.
func drawScenario(t *caseT, cfg Config, satisfiable bool) (*v1.NodePool, []*cloudprovider.InstanceType, []*corev1.Pod) {
	indices := t.distinct(len(cfg.InstanceTypes), 1, t.limits.InstanceTypes)
	slices.Sort(indices)
	its := lo.Map(indices, func(i int, _ int) *cloudprovider.InstanceType { return cfg.InstanceTypes[i] })

	np := lo.TernaryF(cfg.NodePool == nil, func() *v1.NodePool {
		return test.NodePool(v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	}, cfg.NodePool.DeepCopy)
	for i := range t.intRange(0, 2) {
		np.Spec.Template.Labels = lo.Assign(np.Spec.Template.Labels, map[string]string{fmt.Sprintf("example.com/label-%d", i): "true"})
	}
	for i := range t.intRange(0, 2) {
		np.Spec.Template.Spec.Taints = append(np.Spec.Template.Spec.Taints, corev1.Taint{
			Key:    fmt.Sprintf("example.com/taint-%d", i),
			Value:  "true",
			Effect: sampledFrom(t, []corev1.TaintEffect{corev1.TaintEffectNoSchedule, corev1.TaintEffectNoExecute, corev1.TaintEffectPreferNoSchedule}),
		})
	}

	// The target must be an instance type and offering the NodePool's own requirements allow.
	npRequirements := pkgscheduling.NewNodeSelectorRequirementsWithMinValues(np.Spec.Template.Spec.Requirements...)
	launchable := func(it *cloudprovider.InstanceType) cloudprovider.Offerings {
		if it.Requirements.Intersects(npRequirements) != nil {
			return nil
		}
		return lo.Filter(it.Offerings.Available(), func(o *cloudprovider.Offering, _ int) bool {
			return npRequirements.IsCompatible(o.Requirements, pkgscheduling.AllowUndefinedWellKnownLabels)
		})
	}
	candidates := lo.Filter(its, func(it *cloudprovider.InstanceType, _ int) bool { return len(launchable(it)) > 0 })
	if len(candidates) == 0 {
		panic(skip{}) // no instance type satisfies the NodePool requirements
	}
	target := sampledFrom(t, candidates)
	offering := sampledFrom(t, launchable(target))
	labels := lo.Assign(scheduling.NewNodeClaimTemplate(np).Labels, singleValuedLabels(target, offering))
	delete(labels, corev1.LabelHostname)
	for key := range cfg.KnownDeviations {
		delete(labels, key)
	}
	others := otherValues(its, labels)
	drawNodePoolRequirements(t, np, singleValuedLabels(target, offering), labels, others)
	// Only draw Exists for keys every instance type defines. Karpenter intersects Exists with NotIn as NotIn, so a pod
	// requiring a label can share a NodeClaim that lands on a node without it. See #3425.
	universal := sets.New(lo.Filter(lo.Keys(labels), func(key string, _ int) bool {
		return lo.EveryBy(its, func(it *cloudprovider.InstanceType) bool {
			return !it.Requirements.Has(key) || it.Requirements.Get(key).Operator() == corev1.NodeSelectorOpIn
		})
	})...)
	pods := make([]*corev1.Pod, t.intRange(1, t.limits.Pods))
	for i := range pods {
		pods[i] = drawPod(t, i, np, target.Allocatable(), labels, others, universal, satisfiable)
	}
	return np, its, pods
}

// drawNodePoolRequirements adds NodePool requirements the target satisfies on its instance type and offering labels.
// Pods don't constrain those keys, because Karpenter collapses contradictory requirements on one key into
// DoesNotExist (#3423).
func drawNodePoolRequirements(t *caseT, np *v1.NodePool, targetLabels, labels map[string]string, others map[string][]string) {
	// A single-valued requirement on a custom key becomes a node label.
	if t.bool() {
		np.Spec.Template.Spec.Requirements = append(np.Spec.Template.Spec.Requirements, v1.NodeSelectorRequirementWithMinValues{
			Key: "example.com/requirement", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"},
		})
	}
	keys := lo.Filter(lo.Keys(targetLabels), func(key string, _ int) bool { _, ok := labels[key]; return ok })
	if len(keys) == 0 {
		return
	}
	slices.Sort(keys)
	for _, key := range distinctOf(t, keys, 0, 2) {
		req := v1.NodeSelectorRequirementWithMinValues{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{labels[key]}}
		if len(others[key]) > 0 && t.bool() {
			req.Operator, req.Values = corev1.NodeSelectorOpNotIn, []string{sampledFrom(t, others[key])}
		}
		np.Spec.Template.Spec.Requirements = append(np.Spec.Template.Spec.Requirements, req)
		delete(labels, key)
	}
}

// otherValues returns, for each of the target's labels, the values other instance types carry, so excluding them with
// NotIn matters.
func otherValues(its []*cloudprovider.InstanceType, labels map[string]string) map[string][]string {
	others := map[string][]string{}
	for _, it := range its {
		for _, offering := range it.Offerings.Available() {
			for key, value := range singleValuedLabels(it, offering) {
				if _, ok := labels[key]; ok && value != labels[key] && !slices.Contains(others[key], value) {
					others[key] = append(others[key], value)
				}
			}
		}
	}
	for key := range others {
		slices.Sort(others[key])
	}
	return others
}

// singleValuedLabels returns the labels a node launched from the instance type and offering is known to carry.
func singleValuedLabels(it *cloudprovider.InstanceType, offering *cloudprovider.Offering) map[string]string {
	labels := map[string]string{}
	for _, req := range append(it.Requirements.Values(), offering.Requirements.Values()...) {
		if req.Operator() == corev1.NodeSelectorOpIn && req.Len() == 1 {
			labels[req.Key] = req.Any()
		}
	}
	return labels
}

func drawPod(t *caseT, i int, np *v1.NodePool, allocatable corev1.ResourceList, labels map[string]string, others map[string][]string, universal sets.Set[string], satisfiable bool) *corev1.Pod {
	keys := lo.Keys(labels)
	slices.Sort(keys)
	nodeSelector := map[string]string{}
	for _, key := range distinctOf(t, keys, 0, 2) {
		nodeSelector[key] = labels[key]
		if !satisfiable {
			nodeSelector[key] = sampledFrom(t, []string{labels[key], unmatchedValue})
		}
	}
	var tolerations []corev1.Toleration
	for _, taint := range np.Spec.Template.Spec.Taints {
		// PreferNoSchedule taints are soft, so satisfiable pods needn't tolerate them.
		if (satisfiable && taint.Effect != corev1.TaintEffectPreferNoSchedule) || t.bool() {
			toleration := corev1.Toleration{Key: taint.Key, Operator: corev1.TolerationOpExists, Effect: taint.Effect}
			if t.bool() {
				toleration.Operator, toleration.Value = corev1.TolerationOpEqual, taint.Value
			}
			tolerations = append(tolerations, toleration)
		}
	}
	// The scheduler orders pods by creation time then UID, which the API server would normally assign.
	name := fmt.Sprintf("pod-%d", i)
	pod := test.UnschedulablePod(test.PodOptions{
		ObjectMeta:   metav1.ObjectMeta{Name: name, UID: types.UID(name)},
		NodeSelector: nodeSelector,
		Tolerations:  tolerations,
		// Requests up to the target's allocatable, so pods that don't fit together split across NodeClaims.
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(max(1, allocatable.Cpu().MilliValue()*int64(t.intRange(1, 100))/100), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(max(1, allocatable.Memory().Value()*int64(t.intRange(1, 100))/100), resource.BinarySI),
		}},
	})
	pod.CreationTimestamp = metav1.NewTime(time.Unix(0, 0))
	// Each key appears at most once across nodeSelector and affinity. Karpenter collapses contradictory requirements on
	// one key, like Exists and DoesNotExist, into DoesNotExist and launches a node the pod can't schedule to. See #3423.
	if affinity := drawNodeAffinity(t, labels, others, universal, lo.Without(keys, lo.Keys(nodeSelector)...), satisfiable); affinity != nil {
		pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: affinity}
	}
	return pod
}

// drawNodeAffinity draws required and preferred node affinity over keys. Required terms are OR'd, so a satisfiable
// pod has one term the target satisfies. It's last because Karpenter relaxes required terms before preferred terms, so
// an unsatisfiable preference would discard an earlier satisfiable term (#3424). Preferred terms don't reuse required
// keys, because Karpenter collapses contradictory requirements on one key into DoesNotExist (#3423).
func drawNodeAffinity(t *caseT, labels map[string]string, others map[string][]string, universal sets.Set[string], keys []string, satisfiable bool) *corev1.NodeAffinity {
	keys = append([]string{absentLabelKey}, keys...)
	affinity := &corev1.NodeAffinity{}
	if terms := t.intRange(0, t.limits.RequiredTerms); terms > 0 {
		affinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
		for i := range terms {
			affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = append(affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms,
				drawTerm(t, labels, others, universal, keys, satisfiable && i == terms-1))
		}
		for _, term := range affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			keys = lo.Without(keys, lo.Map(term.MatchExpressions, func(r corev1.NodeSelectorRequirement, _ int) string { return r.Key })...)
		}
	}
	keys = lo.Without(keys, v1.NodePoolLabelKey)
	for range lo.Ternary(len(keys) > 0, t.intRange(0, t.limits.PreferredTerms), 0) {
		affinity.PreferredDuringSchedulingIgnoredDuringExecution = append(affinity.PreferredDuringSchedulingIgnoredDuringExecution, corev1.PreferredSchedulingTerm{
			Weight:     1 + t.Int31n(100),
			Preference: drawTerm(t, labels, others, universal, keys, false),
		})
	}
	if affinity.RequiredDuringSchedulingIgnoredDuringExecution == nil && affinity.PreferredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	return affinity
}

// drawTerm draws a term over distinct keys. When satisfy is true, the target satisfies it.
func drawTerm(t *caseT, labels map[string]string, others map[string][]string, universal sets.Set[string], keys []string, satisfy bool) corev1.NodeSelectorTerm {
	if !satisfy {
		// Unsatisfiable terms never constrain the nodepool label. Karpenter ignores pods whose requirements reduce to
		// nodepool DoesNotExist as opting out of Karpenter. See #1265.
		keys = lo.Without(keys, v1.NodePoolLabelKey)
	}
	var term corev1.NodeSelectorTerm
	for _, key := range distinctOf(t, keys, 1, 3) {
		term.MatchExpressions = append(term.MatchExpressions, drawRequirement(t, labels, others, universal, key, satisfy))
	}
	return term
}

// drawRequirement draws a requirement on key. When satisfy is true, the target satisfies it.
func drawRequirement(t *caseT, labels map[string]string, others map[string][]string, universal sets.Set[string], key string, satisfy bool) corev1.NodeSelectorRequirement {
	if key == absentLabelKey {
		return corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpDoesNotExist}
	}
	value := labels[key]
	ops := []corev1.NodeSelectorOperator{corev1.NodeSelectorOpIn, corev1.NodeSelectorOpNotIn}
	if universal.Has(key) {
		ops = append(ops, corev1.NodeSelectorOpExists)
	}
	n, err := strconv.Atoi(value)
	if err == nil && n > 0 {
		ops = append(ops, corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt)
	}
	if !satisfy {
		ops = append(ops, corev1.NodeSelectorOpDoesNotExist)
	}
	req := corev1.NodeSelectorRequirement{Key: key, Operator: sampledFrom(t, ops)}
	miss := !satisfy && t.bool()
	switch req.Operator {
	case corev1.NodeSelectorOpIn:
		req.Values = lo.Ternary(miss, []string{otherValue(key, value)}, []string{value, unmatchedValue})
	case corev1.NodeSelectorOpNotIn:
		req.Values = []string{excludedValue(t, value, others[key], miss)}
	case corev1.NodeSelectorOpGt:
		req.Values = []string{strconv.Itoa(lo.Ternary(miss, n, n-1))}
	case corev1.NodeSelectorOpLt:
		req.Values = []string{strconv.Itoa(lo.Ternary(miss, n, n+1))}
	}
	return req
}

// solve schedules pending pods for np through Provisioner.Schedule over an in-memory client.
func solve(ctx context.Context, t *caseT, cfg Config, np *v1.NodePool, its []*cloudprovider.InstanceType, pods []*corev1.Pod) (scheduling.Results, *provisioning.Provisioner) {
	np = np.DeepCopy()
	np.StatusConditions().SetTrue(status.ConditionReady)
	objects := append([]runtime.Object{np}, lo.Map(pods, func(pod *corev1.Pod, _ int) runtime.Object { return pod.DeepCopy() })...)
	kubeClient := fakecr.NewClientBuilder().WithRuntimeObjects(objects...).WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*corev1.Pod).Spec.NodeName}
	}).Build()
	cp := drawnInstanceTypes{CloudProvider: cfg.CloudProvider, its: its}
	cluster := state.NewCluster(clock.RealClock{}, kubeClient, cp)
	prov := provisioning.NewProvisioner(kubeClient, events.NewRecorder(&record.FakeRecorder{}), cp, cluster, clock.RealClock{}, nil, nil, nil)
	// Scheduling logs bury the failure report.
	results, err := prov.Schedule(log.IntoContext(ctx, logr.Discard()))
	if err != nil {
		t.Fatalf("scheduling, %v", err)
	}
	return results, prov
}

// drawnInstanceTypes returns the drawn instance types for every NodePool.
type drawnInstanceTypes struct {
	cloudprovider.CloudProvider
	its []*cloudprovider.InstanceType
}

func (d drawnInstanceTypes) GetInstanceTypes(context.Context, *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	return d.its, nil
}

// checkPlacements launches every instance type option of every new NodeClaim and checks the resulting node against
// the placed pods as submitted, not as relaxed by scheduling.
func checkPlacements(ctx context.Context, t *caseT, cfg Config, np *v1.NodePool, pods []*corev1.Pod, results scheduling.Results, scenario string) {
	submitted := lo.KeyBy(pods, func(pod *corev1.Pod) types.UID { return pod.UID })
	for _, nc := range results.NewNodeClaims {
		nodeClaim := nc.ToNodeClaim()
		for _, it := range nc.InstanceTypeOptions {
			launch := nodeClaim.DeepCopy()
			launch.Spec.Requirements = append(lo.Reject(launch.Spec.Requirements, func(r v1.NodeSelectorRequirementWithMinValues, _ int) bool {
				return r.Key == corev1.LabelInstanceTypeStable
			}), v1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{it.Name}})
			launched, err := cfg.Launch(ctx, launch)
			if err != nil {
				t.Fatalf("launching instance type option %s, %v", it.Name, err)
			}
			checkNodePool(t, np, it, launched)
			checkNode(t, it, launched, lo.Map(nc.Pods, func(pod *corev1.Pod, _ int) *corev1.Pod { return submitted[pod.UID] }), scenario)
		}
	}
}

// checkNodePool checks that the launched node satisfies the NodePool's requirements.
func checkNodePool(t *caseT, np *v1.NodePool, it *cloudprovider.InstanceType, launched *v1.NodeClaim) {
	if len(np.Spec.Template.Spec.Requirements) == 0 {
		return
	}
	selector, err := nodeaffinity.NewNodeSelector(&corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: lo.Map(np.Spec.Template.Spec.Requirements, func(r v1.NodeSelectorRequirementWithMinValues, _ int) corev1.NodeSelectorRequirement {
			return corev1.NodeSelectorRequirement{Key: r.Key, Operator: r.Operator, Values: r.Values}
		}),
	}}})
	if err != nil {
		t.Fatalf("parsing NodePool requirements, %v", err)
	}
	if !selector.Match(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: launched.Labels}}) {
		t.Fatalf("%s launched with labels %v outside NodePool requirements %v", it.Name, launched.Labels, np.Spec.Template.Spec.Requirements)
	}
}

func checkNode(t *caseT, it *cloudprovider.InstanceType, launched *v1.NodeClaim, pods []*corev1.Pod, scenario string) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: launched.Labels}, Spec: corev1.NodeSpec{Taints: launched.Spec.Taints}}
	requests := corev1.ResourceList{corev1.ResourcePods: *resource.NewQuantity(int64(len(pods)), resource.DecimalSI)}
	for _, pod := range pods {
		if ok, err := nodeaffinity.GetRequiredNodeAffinity(pod).Match(node); err != nil {
			t.Fatalf("matching pod %s against %s, %v", pod.Name, it.Name, err)
		} else if !ok {
			t.Fatalf("pod %s placed on %s doesn't match node labels %v\n%s\n%s", pod.Name, it.Name, node.Labels, describe(pod), scenario)
		}
		if taint, untolerated := corev1helpers.FindMatchingUntoleratedTaint(klog.Background(), node.Spec.Taints, pod.Spec.Tolerations, func(t *corev1.Taint) bool {
			return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
		}, false); untolerated {
			t.Fatalf("pod %s placed on %s doesn't tolerate taint %v\n%s\n%s", pod.Name, it.Name, taint, describe(pod), scenario)
		}
		for name, quantity := range resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{}) {
			total := requests[name]
			total.Add(quantity)
			requests[name] = total
		}
	}
	for name, quantity := range requests {
		if allocatable := launched.Status.Allocatable[name]; quantity.Cmp(allocatable) > 0 {
			t.Fatalf("pods placed on %s request %s %s, exceeding allocatable %s\n%s", it.Name, quantity.String(), name, allocatable.String(), scenario)
		}
	}
}

// excludedValue returns the value a NotIn excludes: the target's own value on a miss, otherwise a value another
// instance type carries when there is one.
func excludedValue(t *caseT, value string, others []string, miss bool) string {
	if miss {
		return value
	}
	if len(others) > 0 {
		return sampledFrom(t, others)
	}
	return unmatchedValue
}

// otherValue returns a value other than value for key, preferring another well known value. Karpenter rejects a pod
// when any required term has only unknown values for a key with well known values, even if another term is valid.
// See #2230.
func otherValue(key, value string) string {
	if known := sets.List(v1.WellKnownValuesForRequirements[key].Clone().Delete(value)); len(known) > 0 {
		return known[0]
	}
	return unmatchedValue
}

// describeScenario describes the NodePool and instance types a failure depends on, since draws aren't reported.
func describeScenario(np *v1.NodePool, its []*cloudprovider.InstanceType) string {
	return fmt.Sprintf("NodePool labels: %v\nNodePool taints: %v\nNodePool requirements: %v\ninstance types: %v", np.Spec.Template.Labels,
		np.Spec.Template.Spec.Taints, np.Spec.Template.Spec.Requirements, lo.Map(its, func(it *cloudprovider.InstanceType, _ int) string { return it.Name }))
}

func describe(pod *corev1.Pod) string {
	return fmt.Sprintf("nodeSelector: %v\naffinity: %v\ntolerations: %v", pod.Spec.NodeSelector, pod.Spec.Affinity, pod.Spec.Tolerations)
}

var (
	seedFlag   = flag.Int64("schedulingproperties.seed", 1, "seed for generated cases, pinned so unrelated changes don't surface new counterexamples; 0 picks a random seed")
	checksFlag = flag.Int("schedulingproperties.checks", 0, "number of generated cases, overriding Config.Checks")
)

// TB is the subset of testing.TB the checks use, satisfied by *testing.T and GinkgoT().
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// limits bound the sizes a case draws. Shrinking lowers them and keeps the seed.
type limits struct {
	Seed           int64
	InstanceTypes  int
	Pods           int
	RequiredTerms  int
	PreferredTerms int
}

var defaultLimits = limits{InstanceTypes: 20, Pods: 5, RequiredTerms: 3, PreferredTerms: 2}

// caseT draws one generated case from its seed and fails it through Fatalf.
type caseT struct {
	*rand.Rand
	limits limits
}

type (
	failure string
	skip    struct{}
)

func (t *caseT) Fatalf(format string, args ...any) { panic(failure(fmt.Sprintf(format, args...))) }

// intRange returns an int in [lo, hi].
func (t *caseT) intRange(lo, hi int) int { return lo + t.Intn(hi-lo+1) }

func (t *caseT) bool() bool { return t.Intn(2) == 1 }

// distinct returns between lo and hi distinct indices below n.
func (t *caseT) distinct(n, lo, hi int) []int { return t.Perm(n)[:t.intRange(lo, min(hi, n))] }

func sampledFrom[E any](t *caseT, s []E) E { return s[t.Intn(len(s))] }

func distinctOf[E any](t *caseT, s []E, low, high int) []E {
	return lo.Map(t.distinct(len(s), low, high), func(i int, _ int) E { return s[i] })
}

// check runs property against generated cases, shrinking a failing case's limits before reporting it.
func check(t TB, cfg Config, property func(*caseT)) {
	t.Helper()
	seed := lo.Ternary(*seedFlag == 0, time.Now().UnixNano(), *seedFlag)
	parameters := gopter.DefaultTestParametersWithSeed(seed)
	parameters.MinSuccessfulTests = lo.CoalesceOrEmpty(*checksFlag, cfg.Checks, parameters.MinSuccessfulTests)
	properties := gopter.NewProperties(parameters)
	// Once a case fails, every later run is a shrink candidate, and a skipped candidate mustn't count as failing.
	var shrinking bool
	properties.Property("", prop.ForAll(func(l limits) *gopter.PropResult {
		result := run(property, l, shrinking)
		shrinking = shrinking || result.Status == gopter.PropFalse
		return result
	}, func(p *gopter.GenParameters) *gopter.GenResult {
		return gopter.NewGenResult(limits{Seed: p.Rng.Int63(), InstanceTypes: defaultLimits.InstanceTypes, Pods: defaultLimits.Pods,
			RequiredTerms: defaultLimits.RequiredTerms, PreferredTerms: defaultLimits.PreferredTerms}, shrink)
	}))
	var result *gopter.TestResult
	if properties.Run(reporter(func(r *gopter.TestResult) { result = r })) {
		return
	}
	args := lo.Map(result.Args, func(a *gopter.PropArg, _ int) string { return fmt.Sprintf("%+v after %d shrinks", a.Arg, a.Shrinks) })
	t.Fatalf("[schedulingproperties] %s after %d passed cases with -schedulingproperties.seed=%d\n%s\n%s%s",
		result.Status, result.Succeeded, seed, strings.Join(args, "\n"), strings.Join(result.Labels, "\n"), lo.Ternary(result.Error != nil, fmt.Sprintf("%v\n%s", result.Error, result.ErrorStack), ""))
}

// run draws one case from l and checks property. Cases with no launchable instance type are discarded, or pass while
// shrinking.
func run(property func(*caseT), l limits, shrinking bool) (result *gopter.PropResult) {
	defer func() {
		switch r := recover().(type) {
		case nil:
		case failure:
			result = &gopter.PropResult{Status: gopter.PropFalse, Labels: []string{string(r)}}
		case skip:
			result = &gopter.PropResult{Status: lo.Ternary(shrinking, gopter.PropTrue, gopter.PropUndecided)}
		default:
			panic(r)
		}
	}()
	property(&caseT{Rand: rand.New(rand.NewSource(l.Seed)), limits: l}) //nolint:gosec
	return &gopter.PropResult{Status: gopter.PropTrue}
}

// shrink lowers each limit toward its minimum, one at a time.
func shrink(value any) gopter.Shrink {
	l := value.(limits)
	var candidates []any
	for _, field := range []*int{&l.InstanceTypes, &l.Pods, &l.RequiredTerms, &l.PreferredTerms} {
		minimum := lo.Ternary(field == &l.InstanceTypes || field == &l.Pods, 1, 0)
		for _, smaller := range lo.Uniq([]int{minimum, *field / 2, *field - 1}) {
			if smaller >= minimum && smaller < *field {
				previous := *field
				*field = smaller
				candidates = append(candidates, l)
				*field = previous
			}
		}
	}
	return func() (any, bool) {
		if len(candidates) == 0 {
			return nil, false
		}
		next := candidates[0]
		candidates = candidates[1:]
		return next, true
	}
}

type reporter func(*gopter.TestResult)

func (r reporter) ReportTestResult(_ string, result *gopter.TestResult) { r(result) }
