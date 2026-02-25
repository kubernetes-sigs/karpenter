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
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/types"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/state/cost"
)

// This whole pricing controller only exists because CP's surface InstanceType information
// via a poll-method. Long term, this controller should likely be removed in favor of refactoring the
// cloudprovider interface to a push model for instance type information:
// https://github.com/kubernetes-sigs/karpenter/issues/2605
type PricingController struct {
	client        client.Client
	cloudProvider cloudprovider.CloudProvider
	clusterCost   *cost.ClusterCost
	npOfMap       map[types.NamespacedName]map[cost.OfferingKey]float64
}

func NewPricingController(client client.Client, cloudProvider cloudprovider.CloudProvider, clusterCost *cost.ClusterCost) *PricingController {
	return &PricingController{
		client:        client,
		cloudProvider: cloudProvider,
		clusterCost:   clusterCost,
	}
}

func (c *PricingController) Reconcile(ctx context.Context) (reconciler.Result, error) {
	npl := &v1.NodePoolList{}
	err := c.client.List(ctx, npl)
	if err != nil {
		return reconciler.Result{}, err
	}

	newNpOfMap := make(map[types.NamespacedName]map[cost.OfferingKey]float64)
	var errs error
	for _, np := range npl.Items {
		oldOfs, exists := c.npOfMap[client.ObjectKeyFromObject(&np)]
		newIts, err := c.cloudProvider.GetInstanceTypes(ctx, &np)
		if err != nil {
			errs = multierr.Append(errs, err)
			continue
		}

		newNpOfMap[client.ObjectKeyFromObject(&np)] = make(map[cost.OfferingKey]float64)

		for _, it := range newIts {
			for _, o := range it.Offerings {
				offeringKey := cost.OfferingKey{InstanceName: it.Name, Zone: o.Zone(), CapacityType: o.CapacityType()}
				newNpOfMap[client.ObjectKeyFromObject(&np)][offeringKey] = o.Price
			}
		}

		if exists && equal(oldOfs, newNpOfMap[client.ObjectKeyFromObject(&np)]) {
			continue
		}
		c.clusterCost.UpdateOfferings(ctx, &np, newIts)
	}
	if errs != nil {
		return reconciler.Result{}, fmt.Errorf("refreshing pricing info, %w", errs)
	}
	c.npOfMap = newNpOfMap

	return reconciler.Result{RequeueAfter: 1 * time.Hour}, nil
}

func equal(oldOfs map[cost.OfferingKey]float64, newOfs map[cost.OfferingKey]float64) bool {
	if len(lo.Values(oldOfs)) != len(newOfs) {
		return false
	}
	for newOf, newPrice := range newOfs {
		oldPrice, exists := oldOfs[newOf]
		if !exists {
			return false
		}
		if oldPrice != newPrice {
			return false
		}
	}
	return true
}

func (c *PricingController) Name() string {
	return "state.pricing"
}

func (c *PricingController) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
