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
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/client-go/kubernetes/scheme"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"sigs.k8s.io/karpenter/pkg/state/prediction"
)

// Controller periodically lists VerticalPodAutoscaler objects and maintains a prediction
// cache of post-recreation resources for VPA-managed pods. It uses a singleton (polling)
// pattern rather than an informer to gracefully tolerate VPA CRD not being installed.
type VPAController struct {
	kubeClient client.Client
	apiReader  client.Reader
	store      *prediction.Store
	// lastSeen tracks the resourceVersion of each VPA we've processed,
	// so we skip recomputation when nothing changed.
	lastSeen map[types.NamespacedName]string
}

func NewVPAController(kubeClient client.Client, apiReader client.Reader, store *prediction.Store) *VPAController {
	utilruntime.Must(vpav1.AddToScheme(scheme.Scheme))
	return &VPAController{
		kubeClient: kubeClient,
		apiReader:  apiReader,
		store:      store,
		lastSeen:   make(map[types.NamespacedName]string),
	}
}

func (c *VPAController) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	var vpaList vpav1.VerticalPodAutoscalerList
	if err := c.kubeClient.List(ctx, &vpaList); err != nil {
		if meta.IsNoMatchError(err) {
			c.store.MarkHydrated()
			return reconciler.Result{RequeueAfter: 1 * time.Minute}, nil
		}
		return reconciler.Result{}, err
	}

	seen := make(map[types.NamespacedName]bool, len(vpaList.Items))
	allResolved := true

	for i := range vpaList.Items {
		vpa := &vpaList.Items[i]
		key := client.ObjectKeyFromObject(vpa)
		seen[key] = true

		if c.lastSeen[key] == vpa.ResourceVersion {
			continue
		}
		if !c.processVPA(ctx, vpa, key) {
			allResolved = false
		}
	}

	for key := range c.lastSeen {
		if !seen[key] {
			c.store.Delete(key)
			delete(c.lastSeen, key)
		}
	}

	if allResolved {
		c.store.MarkHydrated()
	}

	return reconciler.Result{RequeueAfter: 30 * time.Second}, nil
}

func (c *VPAController) processVPA(ctx context.Context, vpa *vpav1.VerticalPodAutoscaler, key types.NamespacedName) bool {
	var p *prediction.Prediction
	if vpa.Spec.TargetRef != nil {
		p = computePrediction(vpa)
	}
	if p == nil {
		c.store.Delete(key)
		c.lastSeen[key] = vpa.ResourceVersion
		return true
	}
	// Resolve the target workload's UID via the uncached API reader to avoid
	// lazily starting cluster-wide informers for arbitrary target GVKs.
	targetObj := &unstructured.Unstructured{}
	targetObj.SetGroupVersionKind(schema.FromAPIVersionAndKind(vpa.Spec.TargetRef.APIVersion, vpa.Spec.TargetRef.Kind))
	if err := c.apiReader.Get(ctx, types.NamespacedName{Namespace: vpa.Namespace, Name: vpa.Spec.TargetRef.Name}, targetObj); err != nil {
		c.store.Delete(key)
		return apierrors.IsNotFound(err)
	}
	c.store.Set(key, targetObj.GetUID(), p, vpa.CreationTimestamp.Time)
	c.lastSeen[key] = vpa.ResourceVersion
	return true
}

func (c *VPAController) Name() string {
	return "vpa.prediction"
}

func (c *VPAController) Register(_ context.Context, m manager.Manager) error {
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
			if boosted := applyStartupBoost(qty, vpa, policy); boosted != nil {
				qty = *boosted
			}
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

func applyStartupBoost(cpu resource.Quantity, vpa *vpav1.VerticalPodAutoscaler, policy *vpav1.ContainerResourcePolicy) *resource.Quantity {
	boost := findStartupBoost(vpa, policy)
	if boost == nil || boost.CPU == nil {
		return nil
	}
	switch boost.CPU.Type {
	case vpav1.FactorStartupBoostType:
		if boost.CPU.Factor != nil && *boost.CPU.Factor > 1 {
			result := resource.NewMilliQuantity(cpu.MilliValue()*int64(*boost.CPU.Factor), cpu.Format)
			return result
		}
	case vpav1.QuantityStartupBoostType:
		if boost.CPU.Quantity != nil {
			result := cpu.DeepCopy()
			result.Add(*boost.CPU.Quantity)
			return &result
		}
	}
	return nil
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
