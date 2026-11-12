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

package informer

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"sigs.k8s.io/karpenter/pkg/state/prediction"
)

const vpaCRDName = "verticalpodautoscalers.autoscaling.k8s.io"

// VPAListTimeout bounds the VPA List. A cached List against a deleted CRD blocks until its context is done, so this
// must stay at most prediction.HydrationTimeout/2 for a reconcile that times out and then finds the CRD gone to mark
// the store hydrated in time.
var VPAListTimeout = 30 * time.Second

// vpaCRDLookupTimeout bounds each VPA CRD lookup, so a reconcile that looks up the CRD on both sides of its List still
// finishes before prediction.HydrationTimeout.
const vpaCRDLookupTimeout = 5 * time.Second

type crdState int

const (
	crdUnknown crdState = iota
	crdPresent
	crdAbsent
	crdTerminating
)

// Controller periodically lists VerticalPodAutoscaler objects and maintains a prediction
// cache of post-recreation resources for VPA-managed pods. It uses a singleton (polling)
// pattern rather than an informer to gracefully tolerate VPA CRD not being installed.
type VPAController struct {
	kubeClient client.Client
	// apiReader reads the VPA CRD uncached, so checking it needs only get on that CRD
	apiReader client.Reader
	// informers holds the VPA informer that kubeClient's List starts, so it can be removed once the CRD is gone
	informers cache.Informers
	store     *prediction.Store
	cluster   *state.Cluster
	recorder  events.Recorder
	// lastSeen tracks the resourceVersion of each VPA we've processed,
	// so we skip recomputation when nothing changed.
	lastSeen map[types.NamespacedName]string
	// listing reports whether the last List returned VPAs
	listing            bool
	crdForbiddenLogged bool
}

func NewVPAController(kubeClient client.Client, apiReader client.Reader, informers cache.Informers, store *prediction.Store, cluster *state.Cluster, recorder events.Recorder) *VPAController {
	utilruntime.Must(vpav1.AddToScheme(scheme.Scheme))
	return &VPAController{
		kubeClient: kubeClient,
		apiReader:  apiReader,
		informers:  informers,
		store:      store,
		cluster:    cluster,
		recorder:   recorder,
		lastSeen:   make(map[types.NamespacedName]string),
	}
}

func (c *VPAController) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	// Once the CRD is deleted, a List that starts a new informer blocks until it times out instead of failing with
	// NoMatch while the RESTMapper still maps VPAs, so the CRD is checked before listing and again when the List fails.
	// A synced informer keeps answering Lists from its cache after the CRD is deleted, and deleting the CRD deletes its
	// VPAs, so an empty List can't tell a present CRD from a cache whose objects are gone. The check before listing is
	// skipped only while a List is returning VPAs.
	// A Terminating CRD still serves its objects, so it is listed, and only counts as gone when the List fails.
	if !c.listing && c.vpaCRDState(ctx) == crdAbsent {
		return c.disable(ctx)
	}
	var vpaList vpav1.VerticalPodAutoscalerList
	listCtx, cancel := context.WithTimeout(ctx, VPAListTimeout)
	err := c.kubeClient.List(listCtx, &vpaList)
	cancel()
	if err != nil {
		if meta.IsNoMatchError(err) || lo.Contains([]crdState{crdAbsent, crdTerminating}, c.vpaCRDState(ctx)) {
			return c.disable(ctx)
		}
		return reconciler.Result{}, err
	}

	seen := make(map[types.NamespacedName]bool, len(vpaList.Items))
	changed := false
	for i := range vpaList.Items {
		vpa := &vpaList.Items[i]
		key := client.ObjectKeyFromObject(vpa)
		seen[key] = true
		if c.lastSeen[key] == vpa.ResourceVersion {
			continue
		}
		changed = c.processVPA(vpa, key) || changed
	}
	changed = c.deleteUnseen(seen) || changed
	// A prediction change can make a node consolidatable, but isn't a cluster state change consolidation watches for
	if changed {
		c.cluster.MarkUnconsolidated()
	}
	c.listing = len(vpaList.Items) > 0
	c.store.MarkHydrated()
	return reconciler.Result{RequeueAfter: 30 * time.Second}, nil
}

// vpaCRDState reports crdUnknown when the CRD can't be read, so the caller decides from its List result. A Forbidden
// lookup is logged once, so the controller keeps working if its RBAC doesn't allow the get yet.
func (c *VPAController) vpaCRDState(ctx context.Context) crdState {
	ctx, cancel := context.WithTimeout(ctx, vpaCRDLookupTimeout)
	defer cancel()
	crd := &metav1.PartialObjectMetadata{}
	crd.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))
	if err := c.apiReader.Get(ctx, client.ObjectKey{Name: vpaCRDName}, crd); err != nil {
		logger := log.FromContext(ctx).WithValues("CustomResourceDefinition", klog.KRef("", vpaCRDName))
		switch {
		case apierrors.IsNotFound(err):
			return crdAbsent
		case apierrors.IsForbidden(err):
			if !c.crdForbiddenLogged {
				c.crdForbiddenLogged = true
				logger.Info("forbidden from getting the VPA CRD, falling back to list errors to detect a missing CRD", "error", err)
			}
		default:
			logger.V(1).Info("failed getting the VPA CRD", "error", err)
		}
		return crdUnknown
	}
	if !crd.DeletionTimestamp.IsZero() {
		return crdTerminating
	}
	return crdPresent
}

// disable marks the store hydrated, since without the VPA CRD there are no VPAs to wait for.
func (c *VPAController) disable(ctx context.Context) (reconciler.Result, error) {
	if c.store.Reset() {
		c.cluster.MarkUnconsolidated()
	}
	c.lastSeen = make(map[types.NamespacedName]string)
	c.listing = false
	c.store.MarkHydrated()
	// Only this controller reads VPAs through the cache. Removing the informer stops it retrying against a deleted CRD,
	// and the next List starts a new one if the CRD comes back.
	if err := c.informers.RemoveInformer(ctx, &vpav1.VerticalPodAutoscaler{}); err != nil {
		return reconciler.Result{}, serrors.Wrap(fmt.Errorf("removing informer, %w", err), "kind", "VerticalPodAutoscaler")
	}
	return reconciler.Result{RequeueAfter: 1 * time.Minute}, nil
}

// deleteUnseen removes predictions of VPAs that no longer exist and reports whether the store's predictions changed.
func (c *VPAController) deleteUnseen(seen map[types.NamespacedName]bool) bool {
	changed := false
	for key := range c.lastSeen {
		if !seen[key] {
			changed = c.store.Delete(key) || changed
			delete(c.lastSeen, key)
		}
	}
	return changed
}

// processVPA reports whether the store's predictions changed.
func (c *VPAController) processVPA(vpa *vpav1.VerticalPodAutoscaler, key types.NamespacedName) bool {
	c.lastSeen[key] = vpa.ResourceVersion
	var p *prediction.Prediction
	if vpa.Spec.TargetRef != nil {
		p = computePrediction(vpa)
	}
	if p == nil {
		return c.store.Delete(key)
	}
	workload := prediction.WorkloadReference{
		GroupKind:      schema.FromAPIVersionAndKind(vpa.Spec.TargetRef.APIVersion, vpa.Spec.TargetRef.Kind).GroupKind(),
		NamespacedName: types.NamespacedName{Namespace: vpa.Namespace, Name: vpa.Spec.TargetRef.Name},
	}
	if !prediction.SupportedTargets.Has(workload.GroupKind) {
		c.recorder.Publish(PredictionTargetUnsupportedEvent(vpa))
		return c.store.Delete(key)
	}
	if !c.store.Set(key, workload, p, vpa.CreationTimestamp.Time) {
		return false
	}
	c.recorder.Publish(PredictionStoredEvent(vpa))
	return true
}

func (c *VPAController) Name() string {
	return "vpa.prediction"
}

func (c *VPAController) Register(ctx context.Context, m manager.Manager) error {
	if err := c.store.RegisterTargetKinds(ctx, m.GetCache(), prediction.SupportedTargets.UnsortedList()...); err != nil {
		return err
	}
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

// computePrediction derives a Prediction from a VPA's spec and status.
// It skips VPAs with updateMode "Off" (which don't mutate pods), applies
// per-container mode checks, controlledResources filtering, and min/max clamping.
func computePrediction(vpa *vpav1.VerticalPodAutoscaler) *prediction.Prediction {
	if vpa.Status.Recommendation == nil {
		return nil
	}
	if vpa.Spec.UpdatePolicy != nil && vpa.Spec.UpdatePolicy.UpdateMode != nil && *vpa.Spec.UpdatePolicy.UpdateMode == vpav1.UpdateModeOff {
		return nil
	}

	containers := make(map[string]corev1.ResourceList, len(vpa.Status.Recommendation.ContainerRecommendations))
	for _, rec := range vpa.Status.Recommendation.ContainerRecommendations {
		if requests := computeContainerResources(vpa, rec); len(requests) > 0 {
			containers[rec.ContainerName] = requests
		}
	}

	if len(containers) == 0 {
		return nil
	}
	return &prediction.Prediction{Containers: containers}
}

func computeContainerResources(vpa *vpav1.VerticalPodAutoscaler, rec vpav1.RecommendedContainerResources) corev1.ResourceList {
	policy := findContainerPolicy(vpa, rec.ContainerName)
	if policy != nil && policy.Mode != nil && *policy.Mode == vpav1.ContainerScalingModeOff {
		return nil
	}
	controlled := controlledResources(policy)

	requests := make(corev1.ResourceList, len(controlled))
	for _, res := range controlled {
		qty, ok := rec.Target[res]
		if !ok {
			continue
		}
		qty = clamp(qty, res, policy)
		if res == corev1.ResourceCPU {
			qty = applyStartupBoost(qty, vpa, policy)
		}
		requests[res] = qty
	}
	return requests
}

func findContainerPolicy(vpa *vpav1.VerticalPodAutoscaler, containerName string) *vpav1.ContainerResourcePolicy {
	if vpa.Spec.ResourcePolicy == nil {
		return nil
	}
	var defaultPolicy *vpav1.ContainerResourcePolicy
	for i := range vpa.Spec.ResourcePolicy.ContainerPolicies {
		p := &vpa.Spec.ResourcePolicy.ContainerPolicies[i]
		if p.ContainerName == containerName {
			return p
		}
		if p.ContainerName == "*" {
			defaultPolicy = p
		}
	}
	return defaultPolicy
}

// controlledResources returns the list of resources managed by the policy.
// Defaults to [cpu, memory] per VPA spec.
func controlledResources(policy *vpav1.ContainerResourcePolicy) []corev1.ResourceName {
	if policy != nil && policy.ControlledResources != nil {
		return *policy.ControlledResources
	}
	return []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}
}

// clamp applies min/max bounds from the container policy.
func clamp(qty resource.Quantity, res corev1.ResourceName, policy *vpav1.ContainerResourcePolicy) resource.Quantity {
	if policy == nil {
		return qty
	}
	if min, ok := policy.MinAllowed[res]; ok {
		if qty.Cmp(min) < 0 {
			return min
		}
	}
	if max, ok := policy.MaxAllowed[res]; ok {
		if qty.Cmp(max) > 0 {
			return max
		}
	}
	return qty
}

func applyStartupBoost(cpu resource.Quantity, vpa *vpav1.VerticalPodAutoscaler, policy *vpav1.ContainerResourcePolicy) resource.Quantity {
	boost := findStartupBoost(vpa, policy)
	if boost == nil || boost.CPU == nil {
		return cpu
	}
	switch boost.CPU.Type {
	case vpav1.FactorStartupBoostType:
		if boost.CPU.Factor != nil && *boost.CPU.Factor > 1 {
			return *resource.NewMilliQuantity(cpu.MilliValue()*int64(*boost.CPU.Factor), cpu.Format)
		}
	case vpav1.QuantityStartupBoostType:
		if boost.CPU.Quantity != nil {
			result := cpu.DeepCopy()
			result.Add(*boost.CPU.Quantity)
			return result
		}
	}
	return cpu
}

func findStartupBoost(vpa *vpav1.VerticalPodAutoscaler, policy *vpav1.ContainerResourcePolicy) *vpav1.StartupBoost {
	if policy != nil && policy.StartupBoost != nil {
		return policy.StartupBoost
	}
	if vpa.Spec.StartupBoost != nil {
		return vpa.Spec.StartupBoost
	}
	return nil
}
