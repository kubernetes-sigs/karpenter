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

package scheduling

import (
	"context"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	karpopts "sigs.k8s.io/karpenter/pkg/operator/options"
)

// The owner kinds whose selectors kube-scheduler's helper.DefaultSelector deduces a default topology spread selector
// from. A pod controlled by anything else (a Job, a DaemonSet, a custom resource) contributes no owner selector, and is
// only defaulted if a Service selects it.
var (
	replicationControllerKind = corev1.SchemeGroupVersion.WithKind("ReplicationController")
	replicaSetKind            = appsv1.SchemeGroupVersion.WithKind("ReplicaSet")
	statefulSetKind           = appsv1.SchemeGroupVersion.WithKind("StatefulSet")
)

// DefaultTopologySpreadInjector applies the cluster-level default topology spread constraints configured via
// --scheduler-config to scheduling-time pod copies, deriving each pod's label selector the same way kube-scheduler's
// PodTopologySpread plugin does.
//
// kube-scheduler forbids a labelSelector on defaultConstraints and instead deduces one per pod, so Karpenter has to
// deduce the same selector to count the same pods. See buildDefaultConstraints and helper.DefaultSelector in
// k8s.io/kubernetes/pkg/scheduler/framework/plugins.
//
// A single injector is used for one injection pass and caches its lookups, since a scheduling loop can ingest many pods
// belonging to only a handful of namespaces and workloads.
type DefaultTopologySpreadInjector struct {
	kubeClient client.Client
	// servicesByNamespace caches the Services in a namespace, keyed by namespace. A namespace with no Services caches
	// an empty slice, so it isn't re-listed for every pod.
	servicesByNamespace map[string][]corev1.Service
	// ownerSelectorByUID caches the resolved selector of a pod's controller, keyed by the controller's UID. A
	// controller that couldn't be resolved caches a nil selector so it isn't re-fetched.
	ownerSelectorByUID map[types.UID]*metav1.LabelSelector
	// selectorByPodShape caches the fully deduced selector for pods that share the same deduction inputs, so a
	// workload's pods resolve it once rather than once each. A pod for which no selector could be deduced caches nil.
	selectorByPodShape map[selectorCacheKey]*metav1.LabelSelector
}

// selectorCacheKey identifies pods that must deduce the same default selector: the deduction reads only the pod's
// namespace, its controller, and its own labels.
type selectorCacheKey struct {
	namespace string
	ownerUID  types.UID
	// labels is the pod's label set in apimachinery's canonical sorted k=v form, so it's comparable as a map key.
	labels string
}

func NewDefaultTopologySpreadInjector(kubeClient client.Client) *DefaultTopologySpreadInjector {
	return &DefaultTopologySpreadInjector{
		kubeClient:          kubeClient,
		servicesByNamespace: map[string][]corev1.Service{},
		ownerSelectorByUID:  map[types.UID]*metav1.LabelSelector{},
		selectorByPodShape:  map[selectorCacheKey]*metav1.LabelSelector{},
	}
}

// Inject applies the configured default topology spread constraints to the given scheduling-time pod copies.
//
// Mirroring kube-scheduler's PodTopologySpread plugin, the defaults are applied only to a pod that declares no
// topologySpreadConstraints of its own (all-or-nothing); a pod with any of its own constraints is left untouched and
// takes precedence over the cluster default. A pod for which no selector can be deduced is left unconstrained, matching
// upstream's `if selector.Empty() { return nil, nil }`. When no defaults are configured this is a no-op, preserving
// today's behavior.
//
// The injected constraints exist only on these scheduling-time copies and are never written back to the API server.
// NOTE: mutating the pods in place is safe because every pod reaching the scheduler is obtained from a cache-backed
// List, which deep-copies. Callers passing pods from another source must copy them first.
func (d *DefaultTopologySpreadInjector) Inject(ctx context.Context, pods []*corev1.Pod) {
	cfg := karpopts.FromContext(ctx).SchedulerConfig
	if cfg == nil || cfg.PodTopologySpread == nil || len(cfg.PodTopologySpread.DefaultConstraints) == 0 {
		return
	}
	for _, p := range pods {
		if len(p.Spec.TopologySpreadConstraints) != 0 {
			continue
		}
		selector := d.cachedDefaultSelector(ctx, p)
		if selector == nil {
			continue
		}
		// Deep-copy the shared default constraints so per-pod preference relaxation (which mutates this slice) can't
		// leak across pods, then stamp each with the pod's deduced selector.
		defaults := make([]corev1.TopologySpreadConstraint, len(cfg.PodTopologySpread.DefaultConstraints))
		for i, tsc := range cfg.PodTopologySpread.DefaultConstraints {
			defaults[i] = *tsc.DeepCopy()
			defaults[i].LabelSelector = selector.DeepCopy()
		}
		p.Spec.TopologySpreadConstraints = defaults
	}
}

// cachedDefaultSelector returns the deduced selector for the pod, reusing a previously computed one when another pod
// shares the same (namespace, controller UID, labels) inputs.
//
// Every pod of a workload deduces an identical selector, since the inputs are its namespace's Services, its
// controller's selector, and its own labels - and the pods of a ReplicaSet share all three. Recomputing per pod means
// re-matching every Service in the namespace against the pod's labels, which is the O(pods x services-per-namespace)
// term in this pass. Keying on the pod's labels (not just its owner) keeps the result exact: pods of the same owner
// with different labels may match different Services, and a pod with no controller still caches by its labels alone.
func (d *DefaultTopologySpreadInjector) cachedDefaultSelector(ctx context.Context, p *corev1.Pod) *metav1.LabelSelector {
	var ownerUID types.UID
	if owner := metav1.GetControllerOfNoCopy(p); owner != nil {
		ownerUID = owner.UID
	}
	key := selectorCacheKey{namespace: p.Namespace, ownerUID: ownerUID, labels: labels.Set(p.Labels).String()}
	if cached, ok := d.selectorByPodShape[key]; ok {
		return cached
	}
	selector := d.defaultSelector(ctx, p)
	d.selectorByPodShape[key] = selector
	return selector
}

// defaultSelector deduces the label selector kube-scheduler would use for the pod's default topology spread
// constraints, or nil if no selector can be deduced and the pod should therefore not be defaulted.
//
// This mirrors helper.DefaultSelector: the selectors of all Services in the pod's namespace that match the pod are
// merged as equality matches, then the selector of the pod's controller (only a ReplicationController, ReplicaSet or
// StatefulSet) is added on top. Upstream tolerates lookup failures and proceeds with a partial selector, so we do too -
// a transient API error shouldn't fail a scheduling loop.
func (d *DefaultTopologySpreadInjector) defaultSelector(ctx context.Context, p *corev1.Pod) *metav1.LabelSelector {
	matchLabels := map[string]string{}
	for _, svc := range d.services(ctx, p.Namespace) {
		// A nil selector matches nothing, not everything, so such a Service never contributes. A non-nil but empty
		// selector matches every pod, but contributes no labels.
		if svc.Spec.Selector == nil {
			continue
		}
		if labels.SelectorFromValidatedSet(svc.Spec.Selector).Matches(labels.Set(p.Labels)) {
			for k, v := range svc.Spec.Selector {
				matchLabels[k] = v
			}
		}
	}

	var matchExpressions []metav1.LabelSelectorRequirement
	if ownerSelector := d.ownerSelector(ctx, p); ownerSelector != nil {
		// Upstream merges a ReplicationController's selector into the label set, but *adds* a ReplicaSet's or
		// StatefulSet's requirements onto the service-derived selector, so a key present in both is ANDed rather than
		// overwritten. Carrying the owner's contribution as expressions preserves that: a contradictory pair selects
		// nothing, exactly as upstream. (In practice this can't arise, since every contributing selector already
		// matches the pod.)
		for k, v := range ownerSelector.MatchLabels {
			matchExpressions = append(matchExpressions, metav1.LabelSelectorRequirement{
				Key:      k,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{v},
			})
		}
		matchExpressions = append(matchExpressions, ownerSelector.MatchExpressions...)
	}

	// Mirrors upstream's `if selector.Empty() { return nil, nil }`. This is load-bearing: an empty LabelSelector would
	// resolve to labels.Everything() and count every pod in the namespace.
	if len(matchLabels) == 0 && len(matchExpressions) == 0 {
		return nil
	}
	if len(matchLabels) == 0 {
		// Keep the emitted selector canonical, so TopologyGroup.Hash() can't distinguish an empty map from an absent one.
		matchLabels = nil
	}
	return &metav1.LabelSelector{MatchLabels: matchLabels, MatchExpressions: matchExpressions}
}

// services returns the Services in the given namespace, listing them once per namespace per injection pass. A list
// failure is logged and treated as "no services", mirroring upstream's swallowing of lister errors.
//
// NOTE: this lists with UnsafeDisableDeepCopy, so the returned Services alias the informer cache and MUST be treated as
// read-only. We only ever read Spec.Selector, and the derived selector is deep-copied before being stamped onto a pod.
func (d *DefaultTopologySpreadInjector) services(ctx context.Context, namespace string) []corev1.Service {
	if cached, ok := d.servicesByNamespace[namespace]; ok {
		return cached
	}
	serviceList := &corev1.ServiceList{}
	if err := d.kubeClient.List(ctx, serviceList, client.InNamespace(namespace), client.UnsafeDisableDeepCopy); err != nil {
		log.FromContext(ctx).V(1).WithValues("namespace", namespace).Error(err, "ignoring services when deducing default topology spread constraints")
		d.servicesByNamespace[namespace] = nil
		return nil
	}
	d.servicesByNamespace[namespace] = serviceList.Items
	return serviceList.Items
}

// ownerSelector returns the selector of the pod's controller, or nil if the pod has no controller, the controller isn't
// one of the kinds kube-scheduler deduces selectors from, or it couldn't be resolved.
func (d *DefaultTopologySpreadInjector) ownerSelector(ctx context.Context, p *corev1.Pod) *metav1.LabelSelector {
	owner := metav1.GetControllerOfNoCopy(p)
	if owner == nil {
		return nil
	}
	if cached, ok := d.ownerSelectorByUID[owner.UID]; ok {
		return cached
	}
	selector, err := d.resolveOwnerSelector(ctx, p.Namespace, owner)
	if err != nil {
		// The owner may have been deleted while its pods linger, or be a kind we don't have access to. Upstream
		// silently proceeds without the owner's contribution in this case.
		log.FromContext(ctx).V(1).WithValues("Pod", klog.KObj(p), "owner", owner.Kind+"/"+owner.Name).Error(err, "ignoring pod owner when deducing default topology spread constraints")
		selector = nil
	}
	d.ownerSelectorByUID[owner.UID] = selector
	return selector
}

// resolveOwnerSelector fetches the owner and returns a copy of just its selector.
//
// The owners are fetched with UnsafeDisableDeepCopy, since deep-copying a ReplicaSet or StatefulSet would copy its whole
// Spec.Template when all we need is Spec.Selector. That aliases the informer cache, so the selector is copied out before
// being returned - it's retained in ownerSelectorByUID and stamped onto pods, neither of which may alias the cache.
func (d *DefaultTopologySpreadInjector) resolveOwnerSelector(ctx context.Context, namespace string, owner *metav1.OwnerReference) (*metav1.LabelSelector, error) {
	gv, err := schema.ParseGroupVersion(owner.APIVersion)
	if err != nil {
		// Mirrors upstream, which returns the service-derived selector on a parse failure rather than erroring.
		return nil, nil //nolint:nilerr
	}
	key := types.NamespacedName{Name: owner.Name, Namespace: namespace}
	switch gv.WithKind(owner.Kind) {
	case replicationControllerKind:
		rc := &corev1.ReplicationController{}
		if err := d.kubeClient.Get(ctx, key, rc, client.UnsafeDisableDeepCopy); err != nil {
			return nil, fmt.Errorf("getting replicationcontroller, %w", err)
		}
		// A ReplicationController's selector is an equality-only map, so it carries no expressions.
		if len(rc.Spec.Selector) == 0 {
			return nil, nil
		}
		return &metav1.LabelSelector{MatchLabels: maps.Clone(rc.Spec.Selector)}, nil
	case replicaSetKind:
		rs := &appsv1.ReplicaSet{}
		if err := d.kubeClient.Get(ctx, key, rs, client.UnsafeDisableDeepCopy); err != nil {
			return nil, fmt.Errorf("getting replicaset, %w", err)
		}
		return rs.Spec.Selector.DeepCopy(), nil
	case statefulSetKind:
		ss := &appsv1.StatefulSet{}
		if err := d.kubeClient.Get(ctx, key, ss, client.UnsafeDisableDeepCopy); err != nil {
			return nil, fmt.Errorf("getting statefulset, %w", err)
		}
		return ss.Spec.Selector.DeepCopy(), nil
	default:
		// Not owned by a supported controller.
		return nil, nil
	}
}
