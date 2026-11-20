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
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	pkgscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const (
	absentLabelKey = "example.com/absent"
	unmatchedValue = "unmatched"
	// groupLabelKey labels each pod with its group, which pod affinity, anti-affinity, and topology spread select.
	groupLabelKey = "example.com/group"
)

var groups = []string{"a", "b"}

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
		s := drawScenario(rt, cfg, true)
		results, prov := solve(ctx, rt, cfg, s)
		placed := sets.New[types.UID]()
		for _, nc := range results.NewNodeClaims {
			placed.Insert(lo.Map(nc.Pods, func(pod *corev1.Pod, _ int) types.UID { return pod.UID })...)
		}
		for _, pod := range s.pods {
			if _, errored := lo.FindKeyBy(results.PodErrors, func(p *corev1.Pod, _ error) bool { return p.UID == pod.UID }); !errored && !placed.Has(pod.UID) {
				rt.Fatalf("pod %s satisfiable by a launchable node was ignored: %v\n%s\n%s", pod.Name, prov.Validate(ctx, pod), s.describe(pod), s)
			}
		}
		for pod, err := range results.PodErrors {
			rt.Fatalf("pod %s satisfiable by a launchable node was not placed: %v\n%s\n%s", pod.Name, err, s.describe(pod), s)
		}
		if count := lo.SumBy(results.NewNodeClaims, func(nc *scheduling.NodeClaim) int { return len(nc.Pods) }); count != len(s.pods) {
			rt.Fatalf("placed %d of %d satisfiable pods\n%s", count, len(s.pods), s)
		}
		checkPlacements(ctx, rt, cfg, s, results)
	})
}

// ExpectPlacementsFitNodes checks that every pod Provisioner.Schedule places on a new NodeClaim matches the required
// node affinity, taints, allocatable resources, volume topology, and host ports of each instance type the NodeClaim can
// launch as, that placements honor required pod affinity, anti-affinity, and topology spread, and that each NodeClaim
// keeps the NodePool's minValues unless the BestEffort policy relaxed them. ctx must carry operator options.
func ExpectPlacementsFitNodes(ctx context.Context, t TB, cfg Config) {
	t.Helper()
	defer setup(&cfg)()
	check(t, cfg, func(rt *caseT) {
		s := drawScenario(rt, cfg, false)
		results, _ := solve(ctx, rt, cfg, s)
		checkPlacements(ctx, rt, cfg, s, results)
	})
}

// ExpectEquivalentPodsScheduleAlike checks that rewriting pods' node constraints into equivalent ones, and reordering
// the instance types, doesn't change which pods Provisioner.Schedule places together, the instance type options of
// their NodeClaims, or which pods fail. ctx must carry operator options.
func ExpectEquivalentPodsScheduleAlike(ctx context.Context, t TB, cfg Config) {
	t.Helper()
	defer setup(&cfg)()
	check(t, cfg, func(rt *caseT) {
		s := drawScenario(rt, cfg, false)
		// Karpenter breaks ties between topology domains in map order, so pods with pod affinity, anti-affinity, or
		// topology spread don't schedule deterministically.
		for _, pod := range s.pods {
			pod.Spec.TopologySpreadConstraints = nil
			if pod.Spec.Affinity != nil {
				pod.Spec.Affinity.PodAffinity, pod.Spec.Affinity.PodAntiAffinity = nil, nil
			}
		}
		results, _ := solve(ctx, rt, cfg, s)
		rewritten := rewrite(rt, s)
		rewrittenResults, _ := solve(ctx, rt, cfg, rewritten)
		if before, after := outcome(results), outcome(rewrittenResults); before != after {
			rt.Fatalf("equivalent pods scheduled differently\nbefore:\n%s\nafter:\n%s\nrewritten pods:\n%s\n%s", before, after,
				strings.Join(lo.Map(rewritten.pods, func(pod *corev1.Pod, _ int) string { return pod.Name + ":\n" + rewritten.describe(pod) }), "\n"), s)
		}
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

// scenario is one generated case: a NodePool, its instance types, and the pods to schedule.
type scenario struct {
	np   *v1.NodePool
	its  []*cloudprovider.InstanceType
	pods []*corev1.Pod
	// volumes are the PersistentVolumeClaims, PersistentVolumes, and StorageClasses the pods mount.
	volumes []runtime.Object
	// volumeSelectors are the node selectors each pod's volume requires, keyed by pod UID.
	volumeSelectors map[types.UID]*corev1.NodeSelector
	minValuesPolicy options.MinValuesPolicy
}

// drawScenario draws a NodePool, its instance types, and pods. When satisfiable is true, every pod is satisfied by
// a node that the NodePool can launch from one of the instance types.
func drawScenario(t *caseT, cfg Config, satisfiable bool) scenario {
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
	// launchable now also honors the drawn requirements.
	npRequirements = pkgscheduling.NewNodeSelectorRequirementsWithMinValues(np.Spec.Template.Spec.Requirements...)
	drawMinValues(t, np, lo.Filter(its, func(it *cloudprovider.InstanceType, _ int) bool { return len(launchable(it)) > 0 }))
	s := scenario{np: np, its: its, volumeSelectors: map[types.UID]*corev1.NodeSelector{}}
	// Under the Strict policy, minValues can leave satisfiable pods unplaced.
	s.minValuesPolicy = lo.Ternary(satisfiable || t.bool(), options.MinValuesPolicyBestEffort, options.MinValuesPolicyStrict)
	topologyKeys := append([]string{corev1.LabelHostname}, lo.Without([]string{corev1.LabelTopologyZone}, cfg.KnownDeviations.UnsortedList()...)...)
	constraints := lo.Map(groups, func(group string, _ int) podConstraints {
		return drawPodConstraints(t, group, topologyKeys, satisfiable)
	})
	s.pods = make([]*corev1.Pod, t.intRange(1, t.limits.Pods))
	for i := range s.pods {
		pod := drawPod(t, i, np, target.Allocatable(), labels, others, satisfiable)
		group := t.Intn(len(groups))
		constraints[group].apply(pod, groups[group])
		if lo.ContainsBy(pod.Spec.TopologySpreadConstraints, func(c corev1.TopologySpreadConstraint) bool { return c.WhenUnsatisfiable == corev1.DoNotSchedule }) {
			avoidSpreadRelaxation(pod)
		}
		drawHostPort(t, pod)
		if volumes, selector := drawVolume(t, i, pod, labels, others, satisfiable); selector != nil {
			s.volumes = append(s.volumes, volumes...)
			s.volumeSelectors[pod.UID] = selector
		}
		s.pods[i] = pod
	}
	return s
}

// drawMinValues may add a NodePool requirement with minValues on a key every launchable instance type carries a single
// value of, so the nodes a NodeClaim's options launch as carry as many values as the options do. minValues is at most
// the number of values the launchable instance types carry.
func drawMinValues(t *caseT, np *v1.NodePool, launchable []*cloudprovider.InstanceType) {
	if len(launchable) == 0 || t.bool() {
		return
	}
	keys := lo.Filter(launchable[0].Requirements.Keys().UnsortedList(), func(key string, _ int) bool {
		return !lo.ContainsBy(np.Spec.Template.Spec.Requirements, func(r v1.NodeSelectorRequirementWithMinValues) bool { return r.Key == key }) &&
			lo.EveryBy(launchable, func(it *cloudprovider.InstanceType) bool {
				return it.Requirements.Has(key) && it.Requirements.Get(key).Operator() == corev1.NodeSelectorOpIn && it.Requirements.Get(key).Len() == 1
			})
	})
	if len(keys) == 0 {
		return
	}
	slices.Sort(keys)
	key := sampledFrom(t, keys)
	values := sets.New(lo.Map(launchable, func(it *cloudprovider.InstanceType, _ int) string { return it.Requirements.Get(key).Any() })...)
	np.Spec.Template.Spec.Requirements = append(np.Spec.Template.Spec.Requirements, v1.NodeSelectorRequirementWithMinValues{
		Key: key, Operator: corev1.NodeSelectorOpExists, MinValues: new(t.intRange(1, len(values))),
	})
}

// podConstraints are the pod affinity, anti-affinity, and topology spread constraints a group's pods share. Every pod
// a spread selects spreads the same way, so the last one placed in a domain saw every selected pod there.
type podConstraints struct {
	podAffinity     *corev1.PodAffinity
	podAntiAffinity *corev1.PodAntiAffinity
	spread          []corev1.TopologySpreadConstraint
}

// drawPodConstraints draws group's pod constraints over topology keys, the first of which is hostname. Satisfiable pods
// can each have their own node, so their hard spreads and anti-affinity only use hostname, because Karpenter counts
// zones it could launch into as empty spread domains, and they don't require affinity, because pods requiring it may
// not fit on one node.
func drawPodConstraints(t *caseT, group string, keys []string, satisfiable bool) podConstraints {
	required := lo.Ternary(satisfiable, keys[:1], keys)
	selecting := func(selected string) *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchLabels: map[string]string{groupLabelKey: selected}}
	}
	var c podConstraints
	if t.bool() {
		whenUnsatisfiable := sampledFrom(t, []corev1.UnsatisfiableConstraintAction{corev1.DoNotSchedule, corev1.ScheduleAnyway})
		c.spread = []corev1.TopologySpreadConstraint{{
			MaxSkew:           1 + t.Int31n(2),
			TopologyKey:       sampledFrom(t, lo.Ternary(whenUnsatisfiable == corev1.DoNotSchedule, required, keys)),
			WhenUnsatisfiable: whenUnsatisfiable,
			LabelSelector:     selecting(group),
		}}
		// Karpenter only counts a pod toward a spread that honors node affinity if the pod's NodeClaim already satisfies
		// that affinity when the pod is placed, missing pods whose NodeClaim narrows to it later. See #3472.
		if whenUnsatisfiable == corev1.DoNotSchedule {
			c.spread[0].NodeAffinityPolicy = new(corev1.NodeInclusionPolicyIgnore)
		}
	}
	// Required affinity selects its own group, so the first pod placed can satisfy it. It's only on hostname, because
	// Karpenter can give the first pod in another topology several domains, then doesn't count it, so a later pod
	// starts a second domain. See #3470.
	if !satisfiable && t.bool() {
		c.podAffinity = &corev1.PodAffinity{}
		c.podAffinity.RequiredDuringSchedulingIgnoredDuringExecution = []corev1.PodAffinityTerm{{LabelSelector: selecting(group), TopologyKey: keys[0]}}
	}
	if t.bool() {
		c.podAntiAffinity = &corev1.PodAntiAffinity{}
		c.podAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = []corev1.PodAffinityTerm{{LabelSelector: selecting(sampledFrom(t, groups)), TopologyKey: sampledFrom(t, required)}}
	}
	if t.bool() {
		c.podAffinity = lo.CoalesceOrEmpty(c.podAffinity, &corev1.PodAffinity{})
		c.podAffinity.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.WeightedPodAffinityTerm{{
			Weight: 1 + t.Int31n(100), PodAffinityTerm: corev1.PodAffinityTerm{LabelSelector: selecting(sampledFrom(t, groups)), TopologyKey: sampledFrom(t, keys)},
		}}
	}
	if t.bool() {
		c.podAntiAffinity = lo.CoalesceOrEmpty(c.podAntiAffinity, &corev1.PodAntiAffinity{})
		c.podAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.WeightedPodAffinityTerm{{
			Weight: 1 + t.Int31n(100), PodAffinityTerm: corev1.PodAffinityTerm{LabelSelector: selecting(sampledFrom(t, groups)), TopologyKey: sampledFrom(t, keys)},
		}}
	}
	return c
}

// apply adds the group label and c to pod.
func (c podConstraints) apply(pod *corev1.Pod, group string) {
	pod.Labels = lo.Assign(pod.Labels, map[string]string{groupLabelKey: group})
	for _, spread := range c.spread {
		pod.Spec.TopologySpreadConstraints = append(pod.Spec.TopologySpreadConstraints, *spread.DeepCopy())
	}
	if c.podAffinity != nil || c.podAntiAffinity != nil {
		pod.Spec.Affinity = lo.CoalesceOrEmpty(pod.Spec.Affinity, &corev1.Affinity{})
		pod.Spec.Affinity.PodAffinity, pod.Spec.Affinity.PodAntiAffinity = c.podAffinity.DeepCopy(), c.podAntiAffinity.DeepCopy()
	}
}

// avoidSpreadRelaxation keeps scheduling from relaxing pod's required node affinity or tolerations. Karpenter keys a
// pod's spread counts by both, so relaxing either starts counts that miss pods already placed. See #3401.
func avoidSpreadRelaxation(pod *corev1.Pod) {
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectPreferNoSchedule})
	if affinity := pod.Spec.Affinity; affinity != nil && affinity.NodeAffinity != nil && affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		// Satisfiable pods' last term is the one the target satisfies.
		terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = terms[max(0, len(terms)-1):]
	}
}

// drawHostPort may give pod a host port from a small set, so pods placed together can conflict.
func drawHostPort(t *caseT, pod *corev1.Pod) {
	if t.bool() {
		return
	}
	port := sampledFrom(t, []int32{80, 81})
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{
		ContainerPort: port,
		HostPort:      port,
		HostIP:        sampledFrom(t, []string{"", "0.0.0.0", "10.0.0.1", "10.0.0.2"}),
		// The API server defaults the protocol to TCP, but the fake client doesn't.
		Protocol: sampledFrom(t, []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}),
	}}
}

// drawVolume may mount a PersistentVolumeClaim on pod, either bound to a PersistentVolume whose node affinity requires
// one of labels or unbound with a WaitForFirstConsumer StorageClass whose allowed topologies do, returning the objects
// and the node selector the volume requires. Its terms are OR'd, so a satisfiable pod's volume has one term the target
// satisfies. The volume constrains a key the pod doesn't, because Karpenter collapses contradictory requirements on one
// key into DoesNotExist (#3423), and never the nodepool label, because Karpenter ignores pods whose requirements reduce
// to nodepool DoesNotExist (#1265).
func drawVolume(t *caseT, i int, pod *corev1.Pod, labels map[string]string, others map[string][]string, satisfiable bool) ([]runtime.Object, *corev1.NodeSelector) {
	keys := lo.Without(lo.Keys(labels), append(nodeAffinityKeys(pod), v1.NodePoolLabelKey)...)
	if len(keys) == 0 || t.bool() {
		return nil, nil
	}
	slices.Sort(keys)
	key := sampledFrom(t, keys)
	claim := metav1.ObjectMeta{Name: fmt.Sprintf("pvc-%d", i), Namespace: pod.Namespace}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "volume", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name},
	}})
	terms := t.intRange(1, 2)
	selector := &corev1.NodeSelector{}
	if t.bool() {
		for j := range terms {
			selector.NodeSelectorTerms = append(selector.NodeSelectorTerms, corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{drawRequirement(t, labels, others, key, satisfiable && j == terms-1)},
			})
		}
		pv := test.PersistentVolume(test.PersistentVolumeOptions{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pv-%d", i)}, NodeSelectorTerms: selector.NodeSelectorTerms})
		pv.Namespace = "" // PersistentVolumes are cluster scoped.
		return []runtime.Object{pv, test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{ObjectMeta: claim, VolumeName: pv.Name})}, selector
	}
	var topologies []corev1.TopologySelectorTerm
	for j := range terms {
		values := []string{labels[key]}
		if (!satisfiable || j != terms-1) && t.bool() {
			values = []string{otherValue(key, labels[key])}
		}
		topologies = append(topologies, corev1.TopologySelectorTerm{MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{{Key: key, Values: values}}})
		selector.NodeSelectorTerms = append(selector.NodeSelectorTerms, corev1.NodeSelectorTerm{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: key, Operator: corev1.NodeSelectorOpIn, Values: values}},
		})
	}
	sc := test.StorageClass(test.StorageClassOptions{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("sc-%d", i)}, AllowedTopologies: topologies, VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer),
	})
	return []runtime.Object{sc, test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{ObjectMeta: claim, StorageClassName: &sc.Name})}, selector
}

// nodeAffinityKeys returns the keys pod's nodeSelector and node affinity constrain.
func nodeAffinityKeys(pod *corev1.Pod) []string {
	keys := lo.Keys(pod.Spec.NodeSelector)
	for _, term := range nodeSelectorTerms(pod) {
		keys = append(keys, lo.Map(term.MatchExpressions, func(r corev1.NodeSelectorRequirement, _ int) string { return r.Key })...)
	}
	return keys
}

// nodeSelectorTerms returns pod's required and preferred node affinity terms.
func nodeSelectorTerms(pod *corev1.Pod) []*corev1.NodeSelectorTerm {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return nil
	}
	var terms []*corev1.NodeSelectorTerm
	if required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution; required != nil {
		for i := range required.NodeSelectorTerms {
			terms = append(terms, &required.NodeSelectorTerms[i])
		}
	}
	for i := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		terms = append(terms, &pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[i].Preference)
	}
	return terms
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

func drawPod(t *caseT, i int, np *v1.NodePool, allocatable corev1.ResourceList, labels map[string]string, others map[string][]string, satisfiable bool) *corev1.Pod {
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
	if affinity := drawNodeAffinity(t, labels, others, lo.Without(keys, lo.Keys(nodeSelector)...), satisfiable); affinity != nil {
		pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: affinity}
	}
	return pod
}

// drawNodeAffinity draws required and preferred node affinity over keys. Required terms are OR'd, so a satisfiable
// pod has one term the target satisfies. It's last because Karpenter relaxes required terms before preferred terms, so
// an unsatisfiable preference would discard an earlier satisfiable term (#3424). Preferred terms don't reuse required
// keys, because Karpenter collapses contradictory requirements on one key into DoesNotExist (#3423).
func drawNodeAffinity(t *caseT, labels map[string]string, others map[string][]string, keys []string, satisfiable bool) *corev1.NodeAffinity {
	keys = append([]string{absentLabelKey}, keys...)
	affinity := &corev1.NodeAffinity{}
	if terms := t.intRange(0, t.limits.RequiredTerms); terms > 0 {
		affinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
		for i := range terms {
			affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = append(affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms,
				drawTerm(t, labels, others, keys, satisfiable && i == terms-1))
		}
		for _, term := range affinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			keys = lo.Without(keys, lo.Map(term.MatchExpressions, func(r corev1.NodeSelectorRequirement, _ int) string { return r.Key })...)
		}
	}
	keys = lo.Without(keys, v1.NodePoolLabelKey)
	for range lo.Ternary(len(keys) > 0, t.intRange(0, t.limits.PreferredTerms), 0) {
		affinity.PreferredDuringSchedulingIgnoredDuringExecution = append(affinity.PreferredDuringSchedulingIgnoredDuringExecution, corev1.PreferredSchedulingTerm{
			Weight:     1 + t.Int31n(100),
			Preference: drawTerm(t, labels, others, keys, false),
		})
	}
	if affinity.RequiredDuringSchedulingIgnoredDuringExecution == nil && affinity.PreferredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	return affinity
}

// drawTerm draws a term over distinct keys. When satisfy is true, the target satisfies it.
func drawTerm(t *caseT, labels map[string]string, others map[string][]string, keys []string, satisfy bool) corev1.NodeSelectorTerm {
	if !satisfy {
		// Unsatisfiable terms never constrain the nodepool label. Karpenter ignores pods whose requirements reduce to
		// nodepool DoesNotExist as opting out of Karpenter. See #1265.
		keys = lo.Without(keys, v1.NodePoolLabelKey)
	}
	var term corev1.NodeSelectorTerm
	for _, key := range distinctOf(t, keys, 1, 3) {
		term.MatchExpressions = append(term.MatchExpressions, drawRequirement(t, labels, others, key, satisfy))
	}
	return term
}

// drawRequirement draws a requirement on key. When satisfy is true, the target satisfies it.
func drawRequirement(t *caseT, labels map[string]string, others map[string][]string, key string, satisfy bool) corev1.NodeSelectorRequirement {
	if key == absentLabelKey {
		return corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpDoesNotExist}
	}
	value := labels[key]
	ops := []corev1.NodeSelectorOperator{corev1.NodeSelectorOpIn, corev1.NodeSelectorOpNotIn, corev1.NodeSelectorOpExists}
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

// solve schedules pending pods for s through Provisioner.Schedule over an in-memory client.
func solve(ctx context.Context, t *caseT, cfg Config, s scenario) (scheduling.Results, *provisioning.Provisioner) {
	np := s.np.DeepCopy()
	np.StatusConditions().SetTrue(status.ConditionReady)
	objects := append([]runtime.Object{np}, lo.Map(s.pods, func(pod *corev1.Pod, _ int) runtime.Object { return pod.DeepCopy() })...)
	objects = append(objects, lo.Map(s.volumes, func(o runtime.Object, _ int) runtime.Object { return o.DeepCopyObject() })...)
	kubeClient := fakecr.NewClientBuilder().WithRuntimeObjects(objects...).WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*corev1.Pod).Spec.NodeName}
	}).WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		// The API server ignores the namespace when getting a cluster-scoped object, but the fake client doesn't find it.
		switch obj.(type) {
		case *corev1.PersistentVolume, *storagev1.StorageClass:
			key.Namespace = ""
		}
		return c.Get(ctx, key, obj, opts...)
	}}).Build()
	cp := drawnInstanceTypes{CloudProvider: cfg.CloudProvider, its: s.its}
	cluster := state.NewCluster(clock.RealClock{}, kubeClient, cp)
	prov := provisioning.NewProvisioner(kubeClient, events.NewRecorder(&record.FakeRecorder{}), cp, cluster, clock.RealClock{}, nil, nil, nil)
	opts := *options.FromContext(ctx)
	opts.MinValuesPolicy = s.minValuesPolicy
	// Scheduling logs bury the failure report.
	results, err := prov.Schedule(log.IntoContext(options.ToContext(ctx, &opts), logr.Discard()))
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

// placement is a new NodeClaim with the pods placed on it and the nodes its instance type options launch as.
type placement struct {
	name     string
	pods     []*corev1.Pod
	launched []*v1.NodeClaim
}

// domains returns the values of key the placement's nodes can carry. Each NodeClaim is its own hostname.
func (p placement) domains(key string) sets.Set[string] {
	if key == corev1.LabelHostname {
		return sets.New(p.name)
	}
	return sets.New(lo.FilterMap(p.launched, func(nc *v1.NodeClaim, _ int) (string, bool) { value, ok := nc.Labels[key]; return value, ok })...)
}

// checkPlacements launches every instance type option of every new NodeClaim and checks the resulting node against
// the placed pods as submitted, not as relaxed by scheduling.
func checkPlacements(ctx context.Context, t *caseT, cfg Config, s scenario, results scheduling.Results) {
	submitted := lo.KeyBy(s.pods, func(pod *corev1.Pod) types.UID { return pod.UID })
	var placements []placement
	for i, nc := range results.NewNodeClaims {
		p := placement{name: fmt.Sprintf("nodeclaim-%d", i), pods: lo.Map(nc.Pods, func(pod *corev1.Pod, _ int) *corev1.Pod { return submitted[pod.UID] })}
		nodeClaim := nc.ToNodeClaim()
		for _, it := range nc.InstanceTypeOptions {
			launch := nodeClaim.DeepCopy()
			launch.Spec.Requirements = append(lo.FilterMap(launch.Spec.Requirements, func(r v1.NodeSelectorRequirementWithMinValues, _ int) (v1.NodeSelectorRequirementWithMinValues, bool) {
				// Each option launches alone, so minValues, which checkMinValues judges across the options, can't hold.
				r.MinValues = nil
				return r, r.Key != corev1.LabelInstanceTypeStable
			}), v1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{it.Name}})
			launched, err := cfg.Launch(ctx, launch)
			if err != nil {
				t.Fatalf("launching instance type option %s, %v", it.Name, err)
			}
			checkNodePool(t, s.np, it, launched)
			checkNode(t, s, it, launched, p.pods)
			p.launched = append(p.launched, launched)
		}
		checkHostPorts(t, s, p)
		checkMinValues(t, s, nodeClaim, p)
		placements = append(placements, p)
	}
	checkPodAntiAffinity(t, s, placements)
	checkPodAffinity(t, s, placements)
	checkTopologySpread(t, s, placements)
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

func checkNode(t *caseT, s scenario, it *cloudprovider.InstanceType, launched *v1.NodeClaim, pods []*corev1.Pod) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: launched.Labels}, Spec: corev1.NodeSpec{Taints: launched.Spec.Taints}}
	requests := corev1.ResourceList{corev1.ResourcePods: *resource.NewQuantity(int64(len(pods)), resource.DecimalSI)}
	for _, pod := range pods {
		if ok, err := nodeaffinity.GetRequiredNodeAffinity(pod).Match(node); err != nil {
			t.Fatalf("matching pod %s against %s, %v", pod.Name, it.Name, err)
		} else if !ok {
			t.Fatalf("pod %s placed on %s doesn't match node labels %v\n%s\n%s", pod.Name, it.Name, node.Labels, s.describe(pod), s)
		}
		checkVolume(t, s, it, node, pod)
		if taint, untolerated := corev1helpers.FindMatchingUntoleratedTaint(klog.Background(), node.Spec.Taints, pod.Spec.Tolerations, func(t *corev1.Taint) bool {
			return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
		}, false); untolerated {
			t.Fatalf("pod %s placed on %s doesn't tolerate taint %v\n%s\n%s", pod.Name, it.Name, taint, s.describe(pod), s)
		}
		resources.MergeInto(requests, resources.RequestsForSpec(&pod.Spec))
	}
	for name, quantity := range requests {
		if allocatable := launched.Status.Allocatable[name]; quantity.Cmp(allocatable) > 0 {
			t.Fatalf("pods placed on %s request %s %s, exceeding allocatable %s\n%s", it.Name, quantity.String(), name, allocatable.String(), s)
		}
	}
}

// checkVolume checks that node matches the topology of pod's volume.
func checkVolume(t *caseT, s scenario, it *cloudprovider.InstanceType, node *corev1.Node, pod *corev1.Pod) {
	selector, ok := s.volumeSelectors[pod.UID]
	if !ok {
		return
	}
	if matches, err := nodeaffinity.NewNodeSelector(selector); err != nil {
		t.Fatalf("parsing volume topology of pod %s, %v", pod.Name, err)
	} else if !matches.Match(node) {
		t.Fatalf("pod %s placed on %s doesn't match its volume's topology with node labels %v\n%s\n%s", pod.Name, it.Name, node.Labels, s.describe(pod), s)
	}
}

// checkHostPorts checks that no two pods on a node use the same host port and protocol on overlapping host IPs, where
// an empty or unspecified host IP overlaps every IP.
func checkHostPorts(t *caseT, s scenario, p placement) {
	used := map[corev1.ContainerPort]*corev1.Pod{}
	for _, pod := range p.pods {
		for _, container := range pod.Spec.Containers {
			for _, port := range container.Ports {
				if port.HostPort == 0 {
					continue
				}
				for existing, other := range used {
					wildcard := lo.Contains([]string{"", "0.0.0.0"}, existing.HostIP) || lo.Contains([]string{"", "0.0.0.0"}, port.HostIP)
					if existing.HostPort == port.HostPort && existing.Protocol == port.Protocol && (existing.HostIP == port.HostIP || wildcard) {
						t.Fatalf("pods %s and %s placed on one node both use host port %d/%s on %q and %q\n%s\n%s\n%s", other.Name, pod.Name, port.HostPort,
							port.Protocol, existing.HostIP, port.HostIP, s.describe(other), s.describe(pod), s)
					}
				}
				used[port] = pod
			}
		}
	}
}

// checkMinValues checks that, for each NodePool requirement with minValues, the nodes a NodeClaim's options launch as
// carry at least the NodeClaim's minValues, which only the BestEffort policy may relax below the NodePool's. The
// relaxed annotation isn't checked: NodeClaims from one NodePool share an annotations map, so the last one created
// sets it for all of them. See #3469.
func checkMinValues(t *caseT, s scenario, nodeClaim *v1.NodeClaim, p placement) {
	for _, req := range s.np.Spec.Template.Spec.Requirements {
		if req.MinValues == nil {
			continue
		}
		minValues := *req.MinValues
		if relaxed, ok := lo.Find(nodeClaim.Spec.Requirements, func(r v1.NodeSelectorRequirementWithMinValues) bool { return r.Key == req.Key }); ok && lo.FromPtr(relaxed.MinValues) < minValues {
			if s.minValuesPolicy == options.MinValuesPolicyStrict {
				t.Fatalf("NodeClaim for pods %v relaxed minValues of %s to %d under the Strict policy\n%s", podNames(p.pods), req.Key, lo.FromPtr(relaxed.MinValues), s)
			}
			minValues = lo.FromPtr(relaxed.MinValues)
		}
		if values := sets.New(lo.FilterMap(p.launched, func(nc *v1.NodeClaim, _ int) (string, bool) { value, ok := nc.Labels[req.Key]; return value, ok })...); values.Len() < minValues {
			t.Fatalf("NodeClaim for pods %v can launch with %d values %v of %s, fewer than minValues %d\n%s", podNames(p.pods), values.Len(), sets.List(values), req.Key, minValues, s)
		}
	}
}

// checkPodAntiAffinity checks that no two pods can land in one domain of a required anti-affinity term either has
// that selects the other.
func checkPodAntiAffinity(t *caseT, s scenario, placements []placement) {
	for _, a := range placements {
		for _, b := range placements {
			for _, pod := range a.pods {
				if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAntiAffinity == nil {
					continue
				}
				for _, term := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
					for _, other := range b.pods {
						if other.UID != pod.UID && selects(term, pod, other) && a.domains(term.TopologyKey).HasAny(b.domains(term.TopologyKey).UnsortedList()...) {
							t.Fatalf("pod %s can land in the same %s as pod %s, which its anti-affinity selects\n%s\n%s\n%s\n%s", pod.Name, term.TopologyKey, other.Name,
								s.describe(pod), s.describe(other), describePlacements(placements), s)
						}
					}
				}
			}
		}
	}
}

// checkPodAffinity checks that each pod with a required affinity term lands, whichever domain its NodeClaim launches in,
// with another pod the term selects. Only the first pod the term selects can be placed without one, so at most one pod
// per term goes without, and only if the term selects it.
func checkPodAffinity(t *caseT, s scenario, placements []placement) {
	unanchored := map[string]*corev1.Pod{}
	for _, a := range placements {
		for _, pod := range a.pods {
			if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAffinity == nil {
				continue
			}
			for _, term := range pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
				if anchored(pod, term, a, placements) {
					continue
				}
				key := fmt.Sprintf("%s %s", metav1.FormatLabelSelector(term.LabelSelector), term.TopologyKey)
				if first, ok := unanchored[key]; ok {
					t.Fatalf("pods %s and %s can each land in a %s without a pod their affinity selects\n%s\n%s\n%s", first.Name, pod.Name, term.TopologyKey,
						s.describe(pod), describePlacements(placements), s)
				} else if !selects(term, pod, pod) {
					t.Fatalf("pod %s can land in a %s without a pod its affinity selects\n%s\n%s\n%s", pod.Name, term.TopologyKey, s.describe(pod), describePlacements(placements), s)
				}
				unanchored[key] = pod
			}
		}
	}
}

// anchored returns whether, whichever domain a launches in, another pod term selects lands there too.
func anchored(pod *corev1.Pod, term corev1.PodAffinityTerm, a placement, placements []placement) bool {
	domains := a.domains(term.TopologyKey)
	return domains.Len() > 0 && lo.EveryBy(sets.List(domains), func(domain string) bool {
		return lo.SomeBy(placements, func(b placement) bool {
			return (b.name == a.name || b.domains(term.TopologyKey).Equal(sets.New(domain))) &&
				lo.SomeBy(b.pods, func(other *corev1.Pod) bool { return other.UID != pod.UID && selects(term, pod, other) })
		})
	})
}

// checkTopologySpread checks the skew of each DoNotSchedule topology spread constraint. Every pod a constraint selects
// shares it, so the last one placed in a domain saw every selected pod there, and the domain's count may exceed the
// minimum over the domains of the nodes that pod's constraint includes by at most maxSkew. Pods that a NodeClaim's
// options disagree on including aren't judged.
func checkTopologySpread(t *caseT, s scenario, placements []placement) {
	type spread struct {
		key      string
		selector string
		domain   string
	}
	type violation struct {
		pod     *corev1.Pod
		maxSkew int32
		counts  map[string]int
	}
	satisfied := sets.New[spread]()
	violations := map[spread]violation{}
	for _, a := range placements {
		for _, pod := range a.pods {
			for _, constraint := range pod.Spec.TopologySpreadConstraints {
				if constraint.WhenUnsatisfiable != corev1.DoNotSchedule {
					continue
				}
				domains := a.domains(constraint.TopologyKey)
				if domains.Len() != 1 {
					t.Fatalf("pod %s spreads on %s but its NodeClaim can launch in %v\n%s\n%s", pod.Name, constraint.TopologyKey, sets.List(domains), s.describe(pod), s)
				}
				counts, ok := spreadCounts(pod, constraint, placements)
				if !ok {
					continue
				}
				domain := domains.UnsortedList()[0]
				key := spread{key: constraint.TopologyKey, selector: metav1.FormatLabelSelector(constraint.LabelSelector), domain: domain}
				if counts[domain]-lo.Min(lo.Values(counts)) <= int(constraint.MaxSkew) {
					satisfied.Insert(key)
				} else {
					violations[key] = violation{pod: pod, maxSkew: constraint.MaxSkew, counts: counts}
				}
			}
		}
	}
	for key, v := range violations {
		if !satisfied.Has(key) {
			t.Fatalf("pods spreading on %s with selector %s exceed maxSkew %d in %s, with counts %v\n%s\n%s\n%s", key.key, key.selector, v.maxSkew, key.domain, v.counts,
				s.describe(v.pod), describePlacements(placements), s)
		}
	}
}

// spreadCounts counts the pods constraint selects in each domain of the nodes it includes, which are those matching
// pod's node affinity unless its node affinity policy is Ignore. A NodeClaim that could launch in several domains holds
// no selected pods, since they spread the same way, so counting each of its domains is the worst case. It returns false
// when a NodeClaim's options disagree on whether they're included.
func spreadCounts(pod *corev1.Pod, constraint corev1.TopologySpreadConstraint, placements []placement) (map[string]int, bool) {
	selector := lo.Must(metav1.LabelSelectorAsSelector(constraint.LabelSelector))
	counts := map[string]int{}
	for _, p := range placements {
		matches := lo.Map(p.launched, func(nc *v1.NodeClaim, _ int) bool {
			return lo.FromPtr(constraint.NodeAffinityPolicy) == corev1.NodeInclusionPolicyIgnore ||
				lo.Must(nodeaffinity.GetRequiredNodeAffinity(pod).Match(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: nc.Labels}}))
		})
		if len(lo.Uniq(matches)) > 1 {
			return nil, false
		}
		if len(matches) == 0 || !matches[0] {
			continue
		}
		selected := lo.CountBy(p.pods, func(other *corev1.Pod) bool {
			return other.Namespace == pod.Namespace && selector.Matches(labels.Set(other.Labels))
		})
		domains := p.domains(constraint.TopologyKey)
		if domains.Len() > 1 && selected > 0 {
			return nil, false
		}
		for domain := range domains {
			counts[domain] += selected
		}
	}
	return counts, true
}

// selects returns whether term, belonging to pod, selects other.
func selects(term corev1.PodAffinityTerm, pod, other *corev1.Pod) bool {
	namespaces := lo.Ternary(len(term.Namespaces) == 0, []string{pod.Namespace}, term.Namespaces)
	return lo.Contains(namespaces, other.Namespace) && lo.Must(metav1.LabelSelectorAsSelector(term.LabelSelector)).Matches(labels.Set(other.Labels))
}

// rewrite returns s with each pod's node constraints rewritten into equivalent ones and the instance types reordered.
func rewrite(t *caseT, s scenario) scenario {
	s.its = lo.Map(t.Perm(len(s.its)), func(i int, _ int) *cloudprovider.InstanceType { return s.its[i] })
	s.pods = lo.Map(s.pods, func(pod *corev1.Pod, _ int) *corev1.Pod {
		pod = pod.DeepCopy()
		rewritePod(t, pod)
		return pod
	})
	return s
}

func rewritePod(t *caseT, pod *corev1.Pod) {
	if len(pod.Spec.NodeSelector) > 0 && t.bool() {
		foldNodeSelector(pod)
	}
	// No node carries the absent label.
	if affinity := pod.Spec.Affinity; affinity != nil && affinity.NodeAffinity != nil && affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil && t.bool() {
		required := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		for i := range required.NodeSelectorTerms {
			required.NodeSelectorTerms[i].MatchExpressions = append(required.NodeSelectorTerms[i].MatchExpressions, corev1.NodeSelectorRequirement{
				Key: absentLabelKey, Operator: corev1.NodeSelectorOpDoesNotExist,
			})
		}
	}
	// No node has the untainted taint.
	if t.bool() {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: "example.com/untainted", Operator: corev1.TolerationOpExists})
	}
	// Neither requirements in a term, values in a requirement, nor tolerations are ordered.
	for _, term := range nodeSelectorTerms(pod) {
		shuffle(t, term.MatchExpressions)
		for i := range term.MatchExpressions {
			shuffle(t, term.MatchExpressions[i].Values)
		}
	}
	shuffle(t, pod.Spec.Tolerations)
}

// foldNodeSelector moves pod's nodeSelector into every required term, which is equivalent.
func foldNodeSelector(pod *corev1.Pod) {
	pod.Spec.Affinity = lo.CoalesceOrEmpty(pod.Spec.Affinity, &corev1.Affinity{})
	pod.Spec.Affinity.NodeAffinity = lo.CoalesceOrEmpty(pod.Spec.Affinity.NodeAffinity, &corev1.NodeAffinity{})
	required := lo.CoalesceOrEmpty(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution, &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}})
	keys := lo.Keys(pod.Spec.NodeSelector)
	slices.Sort(keys)
	for i := range required.NodeSelectorTerms {
		for _, key := range keys {
			required.NodeSelectorTerms[i].MatchExpressions = append(required.NodeSelectorTerms[i].MatchExpressions, corev1.NodeSelectorRequirement{
				Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{pod.Spec.NodeSelector[key]},
			})
		}
	}
	pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = required
	pod.Spec.NodeSelector = nil
}

func shuffle[E any](t *caseT, s []E) { t.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] }) }

// outcome describes which pods Provisioner.Schedule placed together with which instance type options, and which pods
// failed, independent of order.
func outcome(results scheduling.Results) string {
	nodeClaims := lo.Map(results.NewNodeClaims, func(nc *scheduling.NodeClaim, _ int) string {
		its := lo.Map(nc.InstanceTypeOptions, func(it *cloudprovider.InstanceType, _ int) string { return it.Name })
		slices.Sort(its)
		return fmt.Sprintf("%v on %v", podNames(nc.Pods), its)
	})
	slices.Sort(nodeClaims)
	return fmt.Sprintf("%s\nfailed: %v", strings.Join(nodeClaims, "\n"), podNames(lo.Keys(results.PodErrors)))
}

// podNames returns the pods' sorted names.
func podNames(pods []*corev1.Pod) []string {
	names := lo.Map(pods, func(pod *corev1.Pod, _ int) string { return pod.Name })
	slices.Sort(names)
	return names
}

func describePlacements(placements []placement) string {
	return strings.Join(lo.Map(placements, func(p placement, _ int) string {
		return fmt.Sprintf("%s: pods %v, zones %v", p.name, podNames(p.pods), sets.List(p.domains(corev1.LabelTopologyZone)))
	}), "\n")
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

// String describes the NodePool and instance types a failure depends on, since draws aren't reported.
func (s scenario) String() string {
	return fmt.Sprintf("NodePool labels: %v\nNodePool taints: %v\nNodePool requirements: %v\nminValues policy: %s\ninstance types: %v", s.np.Spec.Template.Labels,
		s.np.Spec.Template.Spec.Taints, s.np.Spec.Template.Spec.Requirements, s.minValuesPolicy, lo.Map(s.its, func(it *cloudprovider.InstanceType, _ int) string { return it.Name }))
}

func (s scenario) describe(pod *corev1.Pod) string {
	return fmt.Sprintf("labels: %v\nnodeSelector: %v\naffinity: %v\ntopologySpreadConstraints: %v\ntolerations: %v\nports: %v\nvolume topology: %v", pod.Labels,
		pod.Spec.NodeSelector, pod.Spec.Affinity, pod.Spec.TopologySpreadConstraints, pod.Spec.Tolerations, pod.Spec.Containers[0].Ports, s.volumeSelectors[pod.UID])
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
