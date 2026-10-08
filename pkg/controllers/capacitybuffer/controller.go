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
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/state/virtualpods"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/utils/apps"
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
	kubeClient      client.Client
	trigger         ProvisionerTrigger
	virtualPodCache *virtualpods.Cache
}

func NewController(kubeClient client.Client, trigger ProvisionerTrigger, virtualPodCache *virtualpods.Cache) *Controller {
	return &Controller{
		kubeClient:      kubeClient,
		trigger:         trigger,
		virtualPodCache: virtualPodCache,
	}
}

func (c *Controller) Name() string {
	return "capacitybuffer"
}

func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	cb := &autoscalingv1beta1.CapacityBuffer{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, cb); err != nil {
		if errors.IsNotFound(err) {
			// The buffer was deleted; drop its virtual pods from the cache.
			c.virtualPodCache.RemoveEntry(req.NamespacedName)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	stored := cb.DeepCopy()

	// Resolve pod shape, compute replicas, and update status.
	resolved, podSpec, resolveErr := c.resolveAndUpdateStatus(ctx, cb)
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

	if resolveErr != nil {
		return reconcile.Result{}, resolveErr
	}

	// Refresh the virtual pod cache with the spec we already resolved above,
	// avoiding a second lookup of the same PodTemplate/workload. On resolution
	// failure (resolved == false) the buffer's ReadyForProvisioning condition is
	// False, so UpdateEntry drops any stale entry for it.
	if resolved {
		c.virtualPodCache.UpdateEntry(cb, *podSpec)
	} else {
		c.virtualPodCache.RemoveEntry(client.ObjectKeyFromObject(cb))
	}

	// Notify the provisioner so it can construct virtual pods and update the
	// Provisioning condition in the next reconciliation. We trigger on every
	// successful reconcile (not just status changes) so newly-applied buffers
	// that already have accurate status still cause a provisioning pass.
	if c.trigger != nil && resolved {
		c.trigger.Trigger(cb.UID)
	}

	return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
}

const (
	// podTemplateRefIndex indexes CapacityBuffers by spec.podTemplateRef.name.
	podTemplateRefIndex = "spec.podTemplateRef.name"
	// scalableRefIndex indexes CapacityBuffers by the workload their spec.scalableRef names, keyed by scalableRefKey.
	scalableRefIndex = "spec.scalableRef"
)

// scalableRefKey identifies a workload for scalableRefIndex. An empty group means apps, as in apps.ResolveScalableRef.
func scalableRefKey(group, kind, name string) string {
	return fmt.Sprintf("%s/%s/%s", lo.CoalesceOrEmpty(group, appsv1.GroupName), kind, name)
}

// indexFields registers the field indexes the watch mappers list buffers by.
func indexFields(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &autoscalingv1beta1.CapacityBuffer{}, podTemplateRefIndex, func(o client.Object) []string {
		if ref := o.(*autoscalingv1beta1.CapacityBuffer).Spec.PodTemplateRef; ref != nil {
			return []string{ref.Name}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("indexing %s, %w", podTemplateRefIndex, err)
	}
	if err := indexer.IndexField(ctx, &autoscalingv1beta1.CapacityBuffer{}, scalableRefIndex, func(o client.Object) []string {
		if ref := o.(*autoscalingv1beta1.CapacityBuffer).Spec.ScalableRef; ref != nil {
			return []string{scalableRefKey(ref.APIGroup, ref.Kind, ref.Name)}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("indexing %s, %w", scalableRefIndex, err)
	}
	return nil
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	if err := indexFields(ctx, m.GetFieldIndexer()); err != nil {
		return err
	}
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&autoscalingv1beta1.CapacityBuffer{}).
		Watches(
			&v1.PodTemplate{},
			handler.EnqueueRequestsFromMapFunc(c.podTemplateToBuffers),
		).
		// A buffer whose scalableRef doesn't exist yet otherwise only re-resolves on its periodic requeue. Spec changes
		// (including replicas) bump generation; status-only updates can't change the resolved shape or replica count.
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(c.scalableRefToBuffers(autoscalingv1beta1.KindDeployment)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&appsv1.StatefulSet{},
			handler.EnqueueRequestsFromMapFunc(c.scalableRefToBuffers(autoscalingv1beta1.KindStatefulSet)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&appsv1.ReplicaSet{},
			handler.EnqueueRequestsFromMapFunc(c.scalableRefToBuffers(autoscalingv1beta1.KindReplicaSet)),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 10,
			RateLimiter:             reasonable.RateLimiter(),
		}).
		Complete(c)
}

func (c *Controller) podTemplateToBuffers(ctx context.Context, obj client.Object) []reconcile.Request {
	return c.buffersMatching(ctx, obj.GetNamespace(), podTemplateRefIndex, obj.GetName())
}

func (c *Controller) scalableRefToBuffers(kind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		return c.buffersMatching(ctx, obj.GetNamespace(), scalableRefIndex, scalableRefKey(appsv1.GroupName, kind, obj.GetName()))
	}
}

func (c *Controller) buffersMatching(ctx context.Context, namespace, index, value string) []reconcile.Request {
	buffers := &autoscalingv1beta1.CapacityBufferList{}
	if err := c.kubeClient.List(ctx, buffers, client.InNamespace(namespace), client.MatchingFields{index: value}); err != nil {
		return nil
	}
	return lo.Map(buffers.Items, func(cb autoscalingv1beta1.CapacityBuffer, _ int) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Name: cb.Name, Namespace: cb.Namespace}}
	})
}

// resolveAndUpdateStatus resolves the buffer's pod spec, computes replicas, and
// updates status conditions. Returns (true, spec, nil) on success — the resolved
// spec is returned so the caller can reuse it (e.g. to build virtual pods)
// without re-fetching. Returns (false, nil, nil) for customer-induced errors
// (not found, unsupported kind), or (false, nil, err) for unexpected failures
// that should be retried.
func (c *Controller) resolveAndUpdateStatus(ctx context.Context, cb *autoscalingv1beta1.CapacityBuffer) (bool, *v1.PodTemplateSpec, error) {
	if cb.Spec.PodTemplateRef == nil && cb.Spec.ScalableRef == nil {
		cb.SetCondition(autoscalingv1beta1.ReadyForProvisioningCondition, metav1.ConditionFalse, ReasonResolutionFailed, "Neither podTemplateRef nor scalableRef is set")
		return false, nil, nil
	}

	result, err := apps.ResolveCapacityBuffer(ctx, c.kubeClient, cb)
	if err != nil {
		reason := ReasonScalableRefNotFound
		if cb.Spec.PodTemplateRef != nil {
			reason = ReasonPodTemplateNotFound
		}
		return false, nil, handleResolveError(cb, err, reason)
	}

	var candidates []int32
	if result.UsesPodTemplate {
		cb.Status.PodTemplateRef = &autoscalingv1beta1.LocalObjectRef{Name: result.PodTemplateName}
		cb.Status.PodTemplateGeneration = &result.PodTemplateGeneration
	} else {
		cb.Status.PodTemplateRef = nil
		cb.Status.PodTemplateGeneration = nil
		if cb.Spec.Percentage != nil && result.ScalableReplicas > 0 {
			candidates = append(candidates, calculatePercentageReplicas(result.ScalableReplicas, *cb.Spec.Percentage))
		}
	}

	// Compute replicas from all applicable constraints.
	replicas := computeReplicas(cb, &result.PodTemplateSpec.Spec, candidates)
	cb.SetCondition(autoscalingv1beta1.ReadyForProvisioningCondition, metav1.ConditionTrue, ReasonResolved, "Pod template resolved successfully")
	cb.Status.Replicas = &replicas
	return true, &result.PodTemplateSpec, nil
}

// computeReplicas derives the desired buffer replica count from the configured
// constraints, following Cluster Autoscaler's semantics:
//   - replicas and percentage are combined by taking the MAX of the two.
//   - limits act as an upper bound (MIN) on that value.
//   - if neither replicas nor percentage is set, limits alone determine how many
//     units fit within the resource limits.
//
// candidates holds the percentage-derived replica count (if percentage is set);
// the fixed replicas value is added here.
func computeReplicas(cb *autoscalingv1beta1.CapacityBuffer, podSpec *v1.PodSpec, candidates []int32) int32 {
	if cb.Spec.Replicas != nil {
		candidates = append(candidates, *cb.Spec.Replicas)
	}

	// desired is the max of replicas and percentage. hasSizeConstraint is false
	// when neither is set, in which case limits alone determine the count.
	hasSizeConstraint := len(candidates) > 0
	desired := lo.Max(candidates)

	if cb.Spec.Limits != nil && podSpec != nil {
		if limitReplicas, ok := calculateLimitReplicas(v1.ResourceList(cb.Spec.Limits), podSpec); ok {
			if hasSizeConstraint {
				return lo.Min([]int32{desired, limitReplicas})
			}
			return limitReplicas
		}
	}
	return desired
}

// handleResolveError sets the ReadyForProvisioning condition to False and returns
// nil for NotFound (customer error) or the original error for everything else (retry).
func handleResolveError(cb *autoscalingv1beta1.CapacityBuffer, err error, notFoundReason string) error {
	reason := ReasonResolutionFailed
	if errors.IsNotFound(err) {
		reason = notFoundReason
	}
	cb.SetCondition(autoscalingv1beta1.ReadyForProvisioningCondition, metav1.ConditionFalse, reason, err.Error())
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
