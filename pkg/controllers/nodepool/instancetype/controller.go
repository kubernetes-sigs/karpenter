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

package instancetype

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/samber/lo"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

// Controller for reconciling on node overlay resources
type Controller struct {
	cluster       *state.Cluster
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController constructs a controller for node overlay validation
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster) *Controller {
	return &Controller{
		cluster:       cluster,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Name() string {
	return "nodepool.instancetype"
}

func (c *Controller) Reconcile(ctx context.Context, nodePool *v1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	nodeOverlayList := &v1alpha1.NodeOverlayList{}
	err := c.kubeClient.List(ctx, nodeOverlayList)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	nodeOverlayList.OrderByWeight()

	its, err := c.cloudProvider.GetInstanceTypes(ctx, nodePool)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return reconcile.Result{}, fmt.Errorf("getting instance types, %w", err)
		}
		return reconcile.Result{}, fmt.Errorf("skipping, unable to resolve instance types, %w", err)
	}
	if len(its) == 0 {
		log.FromContext(ctx).WithValues("NodePool", nodePool.Name).Info("skipping, no resolved instance types found")
		return reconcile.Result{}, nil
	}
	adjInstanceTypes := c.overlayInstanceTypes(nodeOverlayList.Items, its)
	c.cluster.UpdateInstanceTypes(nodePool.Name, adjInstanceTypes)

	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1.NodePool{}).
		Watches(&v1alpha1.NodeOverlay{}, nodepoolutils.NodeOverlayEventHandler(c.kubeClient)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func (c *Controller) overlayInstanceTypes(overlays []v1alpha1.NodeOverlay, instanceTypesSet []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	remainingInstanceTypes := instanceTypesSet
	overriddenInstanceTypes := []*cloudprovider.InstanceType{}

	for _, overlay := range overlays {
		// Only apply overlays that have passed runtime validation
		if !overlay.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).IsTrue() {
			continue
		}

		reqs := scheduling.NewRequirements()
		reqs.Add(scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...).Values()...)

		updatedITs := lo.FilterMap(remainingInstanceTypes, func(it *cloudprovider.InstanceType, _ int) (*cloudprovider.InstanceType, bool) {
			// skip instance types that do not match the overlay selector
			if it.Requirements.Intersects(reqs) != nil {
				return nil, false
			}

			// The offering prices that match the overlay selectors need will be updated by the price
			updatedOfferings := updatePricePerOfferings(it.Offerings, reqs, overlay)

			return &cloudprovider.InstanceType{
				Name:         it.Name,
				Offerings:    updatedOfferings,
				Capacity:     it.Capacity,
				Overhead:     it.Overhead,
				Requirements: it.Requirements,
			}, true
		})

		// We want to only apply the overlay based on the weight, once an instance type has been updated
		// by an overlay, avoid updating the instance type again
		remainingInstanceTypes = lo.Filter(remainingInstanceTypes, func(it *cloudprovider.InstanceType, _ int) bool {
			return !lo.ContainsBy(updatedITs, func(upIt *cloudprovider.InstanceType) bool {
				return upIt.Name == it.Name
			})
		})

		// update the list of overridden instance types
		overriddenInstanceTypes = append(overriddenInstanceTypes, updatedITs...)
	}

	return append(overriddenInstanceTypes, remainingInstanceTypes...)
}

func updatePricePerOfferings(offerings cloudprovider.Offerings, overlaySelector scheduling.Requirements, overlay v1alpha1.NodeOverlay) cloudprovider.Offerings {
	results := cloudprovider.Offerings{}

	for _, of := range offerings {
		if of.Available && overlaySelector.IsCompatible(of.Requirements, scheduling.AllowUndefinedWellKnownLabels) {
			results = append(results, &cloudprovider.Offering{
				Requirements:        of.Requirements,
				Available:           of.Available,
				ReservationCapacity: of.ReservationCapacity,
				Price:               overlay.AdjustedPrice(of.Price),
			})
		}
	}

	return results
}
