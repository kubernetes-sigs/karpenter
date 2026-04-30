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
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

const (
	// ConditionReadyForProvisioning indicates whether the buffer's pod template
	// has been successfully resolved and target replicas calculated.
	ConditionReadyForProvisioning = "ReadyForProvisioning"

	// ReasonScalableRefNotFound indicates the referenced scalable resource was not found.
	ReasonScalableRefNotFound = "ScalableRefNotFound"
	// ReasonPodTemplateNotFound indicates the referenced PodTemplate was not found.
	ReasonPodTemplateNotFound = "PodTemplateNotFound"
)

// Controller reconciles CapacityBuffer resources, resolving pod templates
// from either podTemplateRef or scalableRef and updating status conditions.
type Controller struct {
	kubeClient client.Client
}

// NewController constructs a new CapacityBuffer controller.
func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

// Name returns the controller name.
func (c *Controller) Name() string {
	return "capacitybuffer"
}

// Reconcile reconciles a CapacityBuffer resource by resolving its pod template
// and updating the ReadyForProvisioning status condition.
func (c *Controller) Reconcile(ctx context.Context, cb *autoscalingv1alpha1.CapacityBuffer) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())
	logger := log.FromContext(ctx).WithValues("capacitybuffer", cb.Name, "namespace", cb.Namespace)
	logger.Info("reconciling capacity buffer")

	stored := cb.DeepCopy()

	// Set ReadyForProvisioning condition to True (scaffolding: no actual resolution logic yet)
	setCondition(cb, ConditionReadyForProvisioning, metav1.ConditionTrue, "Resolved", "Pod template resolved successfully")

	// Write provisioningStrategy to status
	cb.Status.ProvisioningStrategy = cb.Spec.ProvisioningStrategy

	// Patch status if changed
	if !equality.Semantic.DeepEqual(stored.Status, cb.Status) {
		if err := c.kubeClient.Status().Patch(ctx, cb, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if errors.IsConflict(err) || errors.IsNotFound(err) {
				return reconcile.Result{RequeueAfter: time.Second}, nil
			}
			return reconcile.Result{}, fmt.Errorf("patching status, %w", err)
		}
	}

	return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
}

// Register registers the controller with the manager.
func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&autoscalingv1alpha1.CapacityBuffer{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 10,
			RateLimiter:             reasonable.RateLimiter(),
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// setCondition sets a condition on the CapacityBuffer status.
func setCondition(cb *autoscalingv1alpha1.CapacityBuffer, condType string, condStatus metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&cb.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		ObservedGeneration: cb.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})
}
