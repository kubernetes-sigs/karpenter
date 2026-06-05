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

package nodeoverlay

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1/cel"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// Controller for validating NodeOverlay configuration and surfacing conflicts to the user
type Controller struct {
	kubeClient        client.Client
	cloudProvider     cloudprovider.CloudProvider
	clusterState      *state.Cluster
	instanceTypeStore *InstanceTypeStore
	clock             clock.Clock
}

func (c *Controller) Name() string {
	return "nodeoverlay.controller"
}

// NewController constructs a controller for node overlay validation
func NewController(clk clock.Clock, kubeClient client.Client, cp cloudprovider.CloudProvider, instanceTypeStore *InstanceTypeStore, clusterState *state.Cluster) *Controller {
	return &Controller{
		kubeClient:        kubeClient,
		cloudProvider:     cp,
		instanceTypeStore: instanceTypeStore,
		clusterState:      clusterState,
		clock:             clk,
	}
}

// Reconcile validates that all node overlays don't have conflicting requirements
func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	overlayList := &v1alpha1.NodeOverlayList{}
	nodePoolList := &v1.NodePoolList{}
	if err := c.kubeClient.List(ctx, overlayList); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing nodeoverlays, %w", err)
	}
	if err := c.kubeClient.List(ctx, nodePoolList); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing nodepools, %w", err)
	}
	overlayWithRuntimeValidationFailure := map[string]error{}
	overlaysWithConflict := map[string]bool{}
	temporaryStore := newInternalInstanceTypeStore()
	nodePoolToInstanceTypes := map[string][]*cloudprovider.InstanceType{}

	evaluatedNodePoolItems := make([]v1.NodePool, 0, len(nodePoolList.Items))
	var errs error
	for i := range nodePoolList.Items {
		its, err := c.cloudProvider.GetInstanceTypes(ctx, &nodePoolList.Items[i])
		if err != nil {
			// Track the error so we can return it as a multierr below. We keep
			// going so that a single broken NodePool (for example a missing
			// NodeClass) does not block overlays from being applied to the
			// healthy ones.
			errs = multierr.Append(errs, fmt.Errorf("listing instance types for nodepool %q, %w", nodePoolList.Items[i].Name, err))
			continue
		}
		nodePoolToInstanceTypes[nodePoolList.Items[i].Name] = its
		evaluatedNodePoolItems = append(evaluatedNodePoolItems, nodePoolList.Items[i])
	}

	overlaysWithPriceApplied := map[string]bool{}
	overlaysWithNegativePrice := map[string]bool{}
	overlaysWithExpressionError := map[string]bool{}
	overlayList.OrderByWeight()
	for i := range overlayList.Items {
		if err := overlayList.Items[i].RuntimeValidate(ctx); err != nil {
			overlayWithRuntimeValidationFailure[overlayList.Items[i].Name] = err
			continue
		}

		applyResult := c.validateAndUpdateInstanceTypeOverrides(ctx, temporaryStore, evaluatedNodePoolItems, nodePoolToInstanceTypes, overlayList.Items[i])
		if !applyResult.noConflict {
			overlaysWithConflict[overlayList.Items[i].Name] = true
		}
		overlaysWithPriceApplied[overlayList.Items[i].Name] = applyResult.priceApplied
		overlaysWithNegativePrice[overlayList.Items[i].Name] = applyResult.negativePrice
		overlaysWithExpressionError[overlayList.Items[i].Name] = applyResult.evalError
	}
	temporaryStore.evaluatedNodePools.Insert(lo.Map(evaluatedNodePoolItems, func(np v1.NodePool, _ int) string {
		return np.Name
	})...)

	err, requeue := c.updateOverlayStatuses(ctx, overlayList.Items, overlaysWithConflict, overlayWithRuntimeValidationFailure, overlaysWithPriceApplied, overlaysWithNegativePrice, overlaysWithExpressionError)
	if requeue {
		return reconcile.Result{Requeue: true}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("updating nodeoverlay statuses, %w", err)
	}

	c.instanceTypeStore.UpdateStore(temporaryStore)
	c.clusterState.MarkUnconsolidated()

	// If some NodePools failed to resolve instance types, return the
	// aggregated errors so controller-runtime logs them via its standard
	// "Reconciler error" path and requeues via the rate limiter to recover
	// from transient errors (for example temporary API failures or a
	// NodeClass that has not been created yet) without waiting for the full
	// 6h polling interval.
	if errs != nil {
		return reconcile.Result{}, errs
	}
	return reconcile.Result{RequeueAfter: 6 * time.Hour}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	b := controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		// The reconciled overlay does not matter in this case as one reconcile loop
		// will compare every overlay against every other overlay in the cluster.
		For(&v1alpha1.NodeOverlay{}).
		Watches(&v1.NodePool{}, NodeOverlayEventHandler(c.kubeClient)).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
			RateLimiter:             reasonable.RateLimiter(),
		})

	for _, nodeClass := range c.cloudProvider.GetSupportedNodeClasses() {
		b.Watches(nodeClass, NodeOverlayEventHandler(c.kubeClient))
	}

	return b.Complete(c)
}

type overlayApplyResult struct {
	noConflict    bool
	priceApplied  bool
	negativePrice bool
	evalError     bool
}

// validateAndUpdateInstanceTypeOverrides validates the overlay for conflicts and expression
// evaluation errors, then stores it. Compilation of any CEL price expression is done once
// and reused across the validate, pre-check, and store phases.
func (c *Controller) validateAndUpdateInstanceTypeOverrides(ctx context.Context, temporaryStore *internalInstanceTypeStore, nodePoolList []v1.NodePool, nodePoolToInstanceTypes map[string][]*cloudprovider.InstanceType, overlay v1alpha1.NodeOverlay) overlayApplyResult {
	// Compile the CEL expression once so that all phases share the same program.
	var compiled *cel.PriceExpression
	if overlay.Spec.PriceExpression != nil {
		var err error
		compiled, err = cel.Compile(*overlay.Spec.PriceExpression)
		if err != nil {
			// Should have been caught by RuntimeValidate; log and skip the whole overlay.
			log.FromContext(ctx).Error(err, "skipping overlay with invalid priceExpression", "overlay", overlay.Name)
			return overlayApplyResult{noConflict: true}
		}
	}

	// Due to reserved capacity type offering being dynamically injected as part of the GetInstanceTypes call
	// We will need to make sure we are validating against each nodepool to make sure. This will ensure that
	// overlays that are targeting reserved instance offerings will be able to apply the offering.
	for i := range nodePoolList {
		if !c.validateInstanceTypesOverride(temporaryStore, nodePoolList[i], nodePoolToInstanceTypes[nodePoolList[i].Name], overlay) {
			return overlayApplyResult{}
		}
	}

	// Expression evaluation pre-check: if any matched offering fails evaluation, reject the
	// entire overlay before touching the store. This prevents a bad expression from blocking
	// lower-weight valid overlays that target the same offerings.
	if compiled != nil && c.hasExpressionEvaluationError(ctx, compiled, nodePoolList, nodePoolToInstanceTypes, overlay) {
		return overlayApplyResult{noConflict: true, evalError: true}
	}

	// We separate the validation and storage steps to prevent partial application of invalid node overlays.
	// This two-step process verifies that all instance types across all NodePools are valid before
	// applying any updates, ensuring atomicity of the operation.
	result := overlayApplyResult{noConflict: true}
	for i := range nodePoolList {
		stored, negative := c.storeUpdatesForInstanceTypeOverride(temporaryStore, nodePoolList[i], nodePoolToInstanceTypes[nodePoolList[i].Name], overlay, compiled)
		if stored {
			result.priceApplied = true
		}
		if negative {
			result.negativePrice = true
		}
	}
	return result
}

// hasExpressionEvaluationError returns true if the compiled expression fails to evaluate for any
// offering matched by the overlay. Logs the first error encountered.
func (c *Controller) hasExpressionEvaluationError(ctx context.Context, compiled *cel.PriceExpression, nodePoolList []v1.NodePool, nodePoolToInstanceTypes map[string][]*cloudprovider.InstanceType, overlay v1alpha1.NodeOverlay) bool {
	overlayRequirements := scheduling.NewNodeSelectorRequirements(lo.Map(overlay.Spec.Requirements, func(r v1alpha1.NodeSelectorRequirement, _ int) corev1.NodeSelectorRequirement {
		return r.AsNodeSelectorRequirement()
	})...)
	for i := range nodePoolList {
		for _, it := range nodePoolToInstanceTypes[nodePoolList[i].Name] {
			for _, of := range getOverlaidOfferings(nodePoolList[i], it, overlayRequirements) {
				if _, err := compiled.Evaluate(of.Price); err != nil {
					log.FromContext(ctx).Error(err, "price expression evaluation failed", "overlay", overlay.Name, "instanceType", it.Name)
					return true
				}
			}
		}
	}
	return false
}

func (c *Controller) validateInstanceTypesOverride(store *internalInstanceTypeStore, nodePool v1.NodePool, its []*cloudprovider.InstanceType, overlay v1alpha1.NodeOverlay) bool {
	overlayRequirements := scheduling.NewNodeSelectorRequirements(lo.Map(overlay.Spec.Requirements, func(r v1alpha1.NodeSelectorRequirement, _ int) corev1.NodeSelectorRequirement {
		return r.AsNodeSelectorRequirement()
	})...)

	for _, it := range its {
		offerings := getOverlaidOfferings(nodePool, it, overlayRequirements)
		// if we are not able to find any offerings for an instance type
		// This will mean that the overlay does not select on the instance all together
		if len(offerings) == 0 {
			continue
		}

		conflictingPriceOverlay := c.isPriceUpdatesConflicting(store, nodePool.Name, it.Name, offerings, overlay)
		conflictingCapacityOverlay := c.isCapacityUpdatesConflicting(store, nodePool.Name, it.Name, overlay)
		// When we find an instance type that is matches a set offering, we will track that based on the
		// overlay that is applied
		if conflictingPriceOverlay || conflictingCapacityOverlay {
			return false
		}
	}

	return true
}

// storeUpdatesForInstanceTypeOverride stores the overlay updates and returns (priceStored, negativePrice).
// compiled is the pre-compiled CEL expression from validateAndUpdateInstanceTypeOverrides; it must be
// non-nil when overlay.Spec.PriceExpression is set.
func (c *Controller) storeUpdatesForInstanceTypeOverride(store *internalInstanceTypeStore, nodePool v1.NodePool, its []*cloudprovider.InstanceType, overlay v1alpha1.NodeOverlay, compiled *cel.PriceExpression) (bool, bool) {
	overlayRequirements := scheduling.NewNodeSelectorRequirements(lo.Map(overlay.Spec.Requirements, func(r v1alpha1.NodeSelectorRequirement, _ int) corev1.NodeSelectorRequirement {
		return r.AsNodeSelectorRequirement()
	})...)

	hasPriceSpec := overlay.Spec.Price != nil || overlay.Spec.PriceAdjustment != nil || compiled != nil
	var priceStored, negativePrice bool
	for _, it := range its {
		offerings := getOverlaidOfferings(nodePool, it, overlayRequirements)
		if len(offerings) == 0 {
			continue
		}
		store.updateInstanceTypeOffering(nodePool.Name, it.Name, overlay, offerings, compiled)
		if hasPriceSpec {
			priceStored = true
		}
		if compiled != nil {
			for _, of := range offerings {
				// Evaluation cannot fail here because the pre-check in validateAndUpdateInstanceTypeOverrides
				// already verified all offerings evaluate successfully.
				if price, err := compiled.Evaluate(of.Price); err == nil && price < 0 {
					negativePrice = true
				}
			}
		}
		store.updateInstanceTypeCapacity(nodePool.Name, it.Name, overlay)
	}
	return priceStored, negativePrice
}

// getOverlaidOfferings returns the offerings of the given instance type that are compatible with the
// overlay requirements in the context of the given NodePool. If no offerings match, the instance type
// is considered incompatible with the overlay and nil is returned. Capacity overlays are applied at the
// instance type level: if any offering matches, the capacity change applies to the whole instance type.
func getOverlaidOfferings(nodePool v1.NodePool, it *cloudprovider.InstanceType, overlayReq scheduling.Requirements) cloudprovider.Offerings {
	// The additional requirements will be added to the instance type during scheduling simulation
	// Since getting instance types is done on a NodePool level, these requirements were always assumed
	// to be allowed with these instance types.
	instanceTypeRequirements := scheduling.NewRequirements(
		scheduling.NewRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpIn, nodePool.Name),
	)
	instanceTypeRequirements.Add(scheduling.NewLabelRequirements(nodePool.Spec.Template.ObjectMeta.Labels).Values()...)
	instanceTypeRequirements.Add(it.Requirements.Values()...)

	if !instanceTypeRequirements.IsCompatible(overlayReq) {
		return nil
	}
	return it.Offerings.Compatible(overlayReq)
}

func (c *Controller) isPriceUpdatesConflicting(store *internalInstanceTypeStore, nodePoolName string, instanceTypeName string, offerings cloudprovider.Offerings, overlay v1alpha1.NodeOverlay) bool {
	if overlay.Spec.Price == nil && overlay.Spec.PriceAdjustment == nil && overlay.Spec.PriceExpression == nil {
		return false
	}

	for _, of := range offerings {
		if foundConflict := store.isOfferingUpdateConflicting(nodePoolName, instanceTypeName, of, overlay); foundConflict {
			return true
		}
	}
	return false
}

func (c *Controller) isCapacityUpdatesConflicting(store *internalInstanceTypeStore, nodePoolName string, instanceTypeName string, overlay v1alpha1.NodeOverlay) bool {
	if overlay.Spec.Capacity == nil {
		return false
	}

	if foundConflict := store.isCapacityUpdateConflicting(nodePoolName, instanceTypeName, overlay); foundConflict {
		return true
	}
	return false
}

func (c *Controller) updateOverlayStatuses(ctx context.Context, overlayList []v1alpha1.NodeOverlay, overlaysWithConflict map[string]bool, overlayWithRuntimeValidationFailure map[string]error, overlaysWithPriceApplied map[string]bool, overlaysWithNegativePrice map[string]bool, overlaysWithExpressionError map[string]bool) (error, bool) {
	errs := make([]error, 0, len(overlayList))
	for i := range overlayList {
		stored := overlayList[i].DeepCopy()
		setValidationCondition(&overlayList[i], overlayWithRuntimeValidationFailure, overlaysWithConflict, overlaysWithExpressionError)
		setPriceAppliedCondition(&overlayList[i], overlaysWithPriceApplied)
		setNonNegativeCondition(&overlayList[i], overlaysWithNegativePrice)

		if !equality.Semantic.DeepEqual(stored, overlayList[i]) {
			// We use client.MergeFromWithOptimisticLock because patching a list with a JSON merge patch
			// can cause races due to the fact that it fully replaces the list on a change
			// Here, we are updating the status condition list
			if err := c.kubeClient.Status().Patch(ctx, &overlayList[i], client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
				// Recompute validation in two scenarios:
				// 1. When encountering a conflict error - this may indicate changes were made to a node overlay
				// 2. When an expected overlay is missing from the cluster - this may indicate a previously
				//    identified validation error might have been resolved
				if errors.IsConflict(err) || errors.IsNotFound(err) {
					return nil, true
				}
				errs = append(errs, err)
			}
		}
	}
	return multierr.Combine(errs...), false
}

func setValidationCondition(overlay *v1alpha1.NodeOverlay, runtimeFailures map[string]error, conflicts map[string]bool, expressionErrors map[string]bool) {
	overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
	if err, ok := runtimeFailures[overlay.Name]; ok {
		overlay.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "RuntimeValidation", err.Error())
	} else if conflicts[overlay.Name] {
		overlay.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "Conflict", "conflict with another overlay")
	} else if expressionErrors[overlay.Name] {
		overlay.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded, "ExpressionEvaluationError", "price expression failed to evaluate for one or more matched offerings")
	}
}

func setPriceAppliedCondition(overlay *v1alpha1.NodeOverlay, overlaysWithPriceApplied map[string]bool) {
	hasPriceSpec := overlay.Spec.Price != nil || overlay.Spec.PriceAdjustment != nil || overlay.Spec.PriceExpression != nil
	if !hasPriceSpec || overlaysWithPriceApplied[overlay.Name] {
		overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypePriceApplied)
	} else {
		overlay.StatusConditions().SetFalse(v1alpha1.ConditionTypePriceApplied, "NoMatchingInstanceTypes", "price configuration did not match any instance types")
	}
}

func setNonNegativeCondition(overlay *v1alpha1.NodeOverlay, overlaysWithNegativePrice map[string]bool) {
	if overlaysWithNegativePrice[overlay.Name] {
		overlay.StatusConditions().SetFalse(v1alpha1.ConditionTypePriceNonNegative, "NegativePrice", "price expression produced a negative value for one or more offerings")
	} else {
		overlay.StatusConditions().SetTrue(v1alpha1.ConditionTypePriceNonNegative)
	}
}

// NodeOverlayEventHandler is a watcher on any object to trigger a overlay reconciliation to validate the Node Overlays
// and update the instance type store
func NodeOverlayEventHandler(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		nodeOverlayList := &v1alpha1.NodeOverlayList{}
		err := c.List(ctx, nodeOverlayList)
		if err != nil {
			return nil
		}
		if len(nodeOverlayList.Items) > 0 {
			return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(&nodeOverlayList.Items[0])}}
		}
		return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(o)}}
	})
}
