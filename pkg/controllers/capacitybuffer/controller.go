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

package capacitybuffer

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

// ProvisionerTrigger is the minimal surface of the provisioner that the buffer
// controller needs — so tests can substitute a fake, and we avoid an import
// cycle on the provisioner package.
type ProvisionerTrigger interface {
	Trigger(uid types.UID)
}

// Controller reconciles CapacityBuffer resources by resolving their pod template
// (from podTemplateRef or scalableRef), computing target replica count, and
// updating status so the provisioner knows what buffer capacity to maintain.
type Controller struct {
	kubeClient client.Client
	trigger    ProvisionerTrigger
}

func NewController(kubeClient client.Client, trigger ProvisionerTrigger) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		trigger:    trigger,
	}
}

func (c *Controller) Name() string {
	return "capacitybuffer"
}

func (c *Controller) Reconcile(ctx context.Context, cb *autoscalingv1alpha1.CapacityBuffer) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())
	logger := log.FromContext(ctx).WithValues("capacitybuffer", cb.Name, "namespace", cb.Namespace)
	logger.Info("reconciling capacity buffer")

	stored := cb.DeepCopy()

	// Resolve pod shape and compute replicas. On failure, the condition is set
	// to False with a descriptive reason—we still patch status so users see why.
	podSpec, resolveErr := c.resolvePodSpec(ctx, cb)
	if resolveErr == nil {
		replicas, calcErr := c.calculateReplicas(ctx, cb, podSpec)
		if calcErr == nil {
			setCondition(cb, ReadyForProvisioningCondition, metav1.ConditionTrue, ReasonResolved, "Pod template resolved successfully")
			cb.Status.Replicas = &replicas
		}
	}
	cb.Status.ProvisioningStrategy = cb.Spec.ProvisioningStrategy

	// Always attempt to patch status so conditions are visible even on errors.
	statusChanged := !equality.Semantic.DeepEqual(stored.Status, cb.Status)
	if statusChanged {
		if err := c.kubeClient.Status().Patch(ctx, cb, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if errors.IsConflict(err) || errors.IsNotFound(err) {
				return reconcile.Result{RequeueAfter: time.Second}, nil
			}
			return reconcile.Result{}, fmt.Errorf("patching status, %w", err)
		}
	}

	// Notify the provisioner so it can construct virtual pods and update the
	// Provisioning condition in the next reconciliation. We trigger on every
	// successful reconcile (not just status changes) so newly-applied buffers
	// that already have accurate status still cause a provisioning pass.
	if c.trigger != nil && resolveErr == nil {
		c.trigger.Trigger(cb.UID)
	}

	if resolveErr != nil {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
}

func (c *Controller) resolvePodSpec(ctx context.Context, cb *autoscalingv1alpha1.CapacityBuffer) (*v1.PodSpec, error) {
	if cb.Spec.PodTemplateRef != nil {
		return c.resolvePodTemplateRef(ctx, cb)
	}
	if cb.Spec.ScalableRef != nil {
		return c.resolveScalableRef(ctx, cb)
	}
	setCondition(cb, ReadyForProvisioningCondition, metav1.ConditionFalse, ReasonResolutionFailed, "Neither podTemplateRef nor scalableRef is set")
	return nil, fmt.Errorf("neither podTemplateRef nor scalableRef is set")
}

func (c *Controller) resolvePodTemplateRef(ctx context.Context, cb *autoscalingv1alpha1.CapacityBuffer) (*v1.PodSpec, error) {
	pt := &v1.PodTemplate{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{
		Name:      cb.Spec.PodTemplateRef.Name,
		Namespace: cb.Namespace,
	}, pt); err != nil {
		if errors.IsNotFound(err) {
			setCondition(cb, ReadyForProvisioningCondition, metav1.ConditionFalse, ReasonPodTemplateNotFound,
				fmt.Sprintf("PodTemplate %q not found in namespace %q", cb.Spec.PodTemplateRef.Name, cb.Namespace))
		} else {
			setCondition(cb, ReadyForProvisioningCondition, metav1.ConditionFalse, ReasonResolutionFailed,
				fmt.Sprintf("Failed to get PodTemplate: %v", err))
		}
		return nil, err
	}

	cb.Status.PodTemplateRef = &autoscalingv1alpha1.LocalObjectRef{Name: pt.Name}
	cb.Status.PodTemplateGeneration = &pt.Generation
	return &pt.Template.Spec, nil
}

// resolveScalableRef fetches the referenced workload (e.g. Deployment, StatefulSet)
// via unstructured GET and extracts its pod template spec. We use unstructured
// to avoid importing every possible workload type.
func (c *Controller) resolveScalableRef(ctx context.Context, cb *autoscalingv1alpha1.CapacityBuffer) (*v1.PodSpec, error) {
	ref := cb.Spec.ScalableRef
	gvk := schema.GroupVersionKind{
		Group:   ref.APIGroup,
		Kind:    ref.Kind,
		Version: "v1",
	}
	if ref.APIGroup == "apps" || ref.APIGroup == "" {
		gvk.Group = "apps"
		gvk.Version = "v1"
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := c.kubeClient.Get(ctx, types.NamespacedName{
		Name:      ref.Name,
		Namespace: cb.Namespace,
	}, obj); err != nil {
		if errors.IsNotFound(err) {
			setCondition(cb, ReadyForProvisioningCondition, metav1.ConditionFalse, ReasonScalableRefNotFound,
				fmt.Sprintf("%s %q not found in namespace %q", ref.Kind, ref.Name, cb.Namespace))
		} else {
			setCondition(cb, ReadyForProvisioningCondition, metav1.ConditionFalse, ReasonResolutionFailed,
				fmt.Sprintf("Failed to get scalable resource: %v", err))
		}
		return nil, err
	}

	podSpec, err := extractPodSpecFromUnstructured(obj)
	if err != nil {
		setCondition(cb, ReadyForProvisioningCondition, metav1.ConditionFalse, ReasonResolutionFailed,
			fmt.Sprintf("Failed to extract pod spec from %s/%s: %v", ref.Kind, ref.Name, err))
		return nil, err
	}

	return podSpec, nil
}


// calculateReplicas gathers all applicable constraints (fixed, percentage, limits)
// and returns the minimum. This ensures we never exceed any single constraint.
func (c *Controller) calculateReplicas(ctx context.Context, cb *autoscalingv1alpha1.CapacityBuffer, podSpec *v1.PodSpec) (int32, error) {
	var candidates []int32

	if cb.Spec.Replicas != nil {
		candidates = append(candidates, *cb.Spec.Replicas)
	}

	if cb.Spec.Percentage != nil && cb.Spec.ScalableRef != nil {
		scalableReplicas, err := c.getScalableReplicas(ctx, cb)
		if err != nil {
			return 0, err
		}
		candidates = append(candidates, calculatePercentageReplicas(scalableReplicas, *cb.Spec.Percentage))
	}

	if cb.Spec.Limits != nil && podSpec != nil {
		limitReplicas := calculateLimitReplicas(v1.ResourceList(cb.Spec.Limits), podSpec)
		if limitReplicas >= 0 {
			candidates = append(candidates, limitReplicas)
		}
	}

	if len(candidates) == 0 {
		return 0, nil
	}

	// Use the minimum of all constraints
	result := candidates[0]
	for _, c := range candidates[1:] {
		if c < result {
			result = c
		}
	}
	return result, nil
}

func (c *Controller) getScalableReplicas(ctx context.Context, cb *autoscalingv1alpha1.CapacityBuffer) (int32, error) {
	ref := cb.Spec.ScalableRef
	gvk := schema.GroupVersionKind{
		Group:   ref.APIGroup,
		Kind:    ref.Kind,
		Version: "v1",
	}
	if ref.APIGroup == "apps" || ref.APIGroup == "" {
		gvk.Group = "apps"
		gvk.Version = "v1"
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := c.kubeClient.Get(ctx, types.NamespacedName{
		Name:      ref.Name,
		Namespace: cb.Namespace,
	}, obj); err != nil {
		return 0, err
	}

	replicas, found, err := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	if err != nil || !found {
		return 0, fmt.Errorf("unable to read spec.replicas from %s/%s", ref.Kind, ref.Name)
	}
	return int32(replicas), nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&autoscalingv1alpha1.CapacityBuffer{}).
		Watches(
			&v1.PodTemplate{},
			handler.EnqueueRequestsFromMapFunc(c.podTemplateToBuffers),
		).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 10,
			RateLimiter:             reasonable.RateLimiter(),
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// podTemplateToBuffers returns reconcile requests for every CapacityBuffer in
// the PodTemplate's namespace that references it via spec.podTemplateRef.
// This keeps buffer status in sync with template changes without waiting for
// the periodic requeue.
func (c *Controller) podTemplateToBuffers(ctx context.Context, obj client.Object) []reconcile.Request {
	buffers := &autoscalingv1alpha1.CapacityBufferList{}
	if err := c.kubeClient.List(ctx, buffers, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range buffers.Items {
		cb := &buffers.Items[i]
		if cb.Spec.PodTemplateRef == nil || cb.Spec.PodTemplateRef.Name != obj.GetName() {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: cb.Name, Namespace: cb.Namespace},
		})
	}
	return requests
}

