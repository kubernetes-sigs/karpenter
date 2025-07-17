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

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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
	nodeOverlayList.OrderByWeight() // Test

	// Test
	its, err := c.cloudProvider.GetInstanceTypes(ctx, nodePool)
	if err != nil {
		c.cluster.UpdateInstanceTypes(nodePool.Name, []*cloudprovider.InstanceType{}) // Test
		if errors.Is(err, context.DeadlineExceeded) {
			return reconcile.Result{}, fmt.Errorf("getting instance types, %w", err)
		}
		return reconcile.Result{}, fmt.Errorf("skipping, unable to resolve instance types, %w", err)
	}
	// Test
	if len(its) == 0 {
		c.cluster.UpdateInstanceTypes(nodePool.Name, []*cloudprovider.InstanceType{}) // Test
		return reconcile.Result{}, nil
	}
	c.overlayInstanceTypes(nodeOverlayList.Items, its)
	c.cluster.UpdateInstanceTypes(nodePool.Name, its) // Test

	return reconcile.Result{RequeueAfter: time.Hour}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1.NodePool{}).
		Watches(&v1alpha1.NodeOverlay{}, nodepoolutils.NodeOverlayEventHandler(c.kubeClient)).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// Explain weights. That will be hard to understand
func (c *Controller) overlayInstanceTypes(overlays []v1alpha1.NodeOverlay, instanceTypes []*cloudprovider.InstanceType) {
	// Only apply overlays that have passed runtime validation
	validOverlays := lo.Filter(overlays, func(o v1alpha1.NodeOverlay, _ int) bool {
		return o.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).IsTrue()
	})

	overlayCapacityOnInstanceTypes(validOverlays, instanceTypes)
	overlayPriceOnInstanceTypes(validOverlays, instanceTypes)
}

// Test with different offerings
func overlayCapacityOnInstanceTypes(overlays []v1alpha1.NodeOverlay, its []*cloudprovider.InstanceType) {
	for _, overlay := range overlays {
		overlaySelector := scheduling.NewRequirements()
		overlaySelector.Add(scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...).Values()...)

		for _, it := range its {
			it.Capacity = lo.Ternary(it.Requirements.Intersects(overlaySelector) == nil, lo.Assign(overlay.Spec.Capacity, it.Capacity), it.Capacity)
		}
	}
}

// Test with different offerings
func overlayPriceOnInstanceTypes(overlays []v1alpha1.NodeOverlay, its []*cloudprovider.InstanceType) {
	overriddenInstanceType := map[string][]scheduling.Requirements{}

	for _, overlay := range overlays {
		overlaySelector := scheduling.NewRequirements()
		overlaySelector.Add(scheduling.NewNodeSelectorRequirements(overlay.Spec.Requirements...).Values()...)

		for _, it := range its {
			if err := it.Requirements.Intersects(overlaySelector); err != nil {
				continue
			}

			overriddenInstanceType[it.Name] = []scheduling.Requirements{}
			for _, of := range it.Offerings {
				alreadyOverridden := lo.ContainsBy(overriddenInstanceType[it.Name], func(tempOf scheduling.Requirements) bool {
					return equality.Semantic.DeepEqual(tempOf, of.Requirements)
				})
				if overlaySelector.IsCompatible(of.Requirements, scheduling.AllowUndefinedWellKnownLabels) && !alreadyOverridden {
					overriddenInstanceType[it.Name] = append(overriddenInstanceType[it.Name], of.Requirements)
					of.Price = overlay.AdjustedPrice(of.Price)
				}
			}
		}
	}
}
