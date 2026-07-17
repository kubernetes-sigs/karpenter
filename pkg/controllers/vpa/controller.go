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

package vpa

import (
	"context"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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

func init() {
	utilruntime.Must(vpav1.AddToScheme(scheme.Scheme))
}

// Controller periodically lists VerticalPodAutoscaler objects and maintains a prediction
// cache of post-recreation resources for VPA-managed pods. It uses a singleton (polling)
// pattern rather than an informer to gracefully tolerate VPA CRD not being installed.
type Controller struct {
	kubeClient client.Client
	store      *prediction.Store
	// lastSeen tracks the resourceVersion of each VPA we've processed,
	// so we skip recomputation when nothing changed.
	lastSeen map[types.NamespacedName]string
}

func NewController(kubeClient client.Client, store *prediction.Store) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		store:      store,
		lastSeen:   make(map[types.NamespacedName]string),
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	var vpaList vpav1.VerticalPodAutoscalerList
	if err := c.kubeClient.List(ctx, &vpaList); err != nil {
		if meta.IsNoMatchError(err) {
			return reconciler.Result{RequeueAfter: 1 * time.Minute}, nil
		}
		return reconciler.Result{}, err
	}

	// Track which VPAs we see this cycle, to detect deletions
	seen := make(map[types.NamespacedName]bool, len(vpaList.Items))

	for i := range vpaList.Items {
		vpa := &vpaList.Items[i]
		key := client.ObjectKeyFromObject(vpa)
		seen[key] = true

		if c.lastSeen[key] == vpa.ResourceVersion {
			continue
		}
		c.lastSeen[key] = vpa.ResourceVersion

		var p *prediction.Prediction
		if vpa.Spec.TargetRef != nil {
			p = computePrediction(vpa)
		}
		if p == nil {
			c.store.Delete(key)
			continue
		}

		c.store.Set(key, prediction.TargetKey{
			Namespace: vpa.Namespace,
			Kind:      vpa.Spec.TargetRef.Kind,
			Name:      vpa.Spec.TargetRef.Name,
		}, p)
	}

	// Clean up predictions for deleted VPAs
	for key := range c.lastSeen {
		if !seen[key] {
			c.store.Delete(key)
			delete(c.lastSeen, key)
		}
	}

	return reconciler.Result{RequeueAfter: 30 * time.Second}, nil
}

func (c *Controller) Name() string {
	return "vpa.prediction"
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
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
		requests[res] = clamp(qty, res, policy)
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
