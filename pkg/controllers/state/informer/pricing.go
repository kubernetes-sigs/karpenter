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
	"github.com/samber/lo"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

type Controller struct {
	client        client.Client
	cloudProvider cloudprovider.CloudProvider
	clusterCost   *state.ClusterCost
	npItMap       map[string]map[string]*cloudprovider.InstanceType
}

func NewController(client client.Client, cloudProvider cloudprovider.CloudProvider, clusterCost *state.ClusterCost) *Controller {
	return &Controller{
		client:        client,
		cloudProvider: cloudProvider,
		clusterCost:   clusterCost,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	npl := &v1.NodePoolList{}
	err := c.client.List(ctx, npl)
	if err != nil {
		return reconciler.Result{}, err
	}

	var shouldUpdate bool

	for _, np := range npl.Items {
		oldIts, exists := c.npItMap[client.ObjectKeyFromObject(&np).String()]
		if !exists {
			shouldUpdate = true
			break
		}
		newIts, err := c.cloudProvider.GetInstanceTypes(ctx, &np)
		if err != nil {
			return reconciler.Result{}, err
		}
		if !equal(oldIts, newIts) {
			shouldUpdate = true
			break
		}
	}

	if shouldUpdate {
		newNpItMap := make(map[string]map[string]*cloudprovider.InstanceType)
		for _, np := range npl.Items {
			newIts, err := c.cloudProvider.GetInstanceTypes(ctx, &np)
			if err != nil {
				return reconciler.Result{}, err
			}
			err = c.clusterCost.UpdateOfferings(ctx, &np, newIts)
			if err != nil {
				return reconciler.Result{}, err
			}
			newNpItMap[client.ObjectKeyFromObject(&np).String()] = lo.SliceToMap(newIts, func(it *cloudprovider.InstanceType) (string, *cloudprovider.InstanceType) {
				return it.Name, it
			})
		}
		c.npItMap = newNpItMap

	}

	return reconciler.Result{RequeueAfter: 1 * time.Hour}, nil
}

func equal(oldIts map[string]*cloudprovider.InstanceType, newIts []*cloudprovider.InstanceType) bool {
	for _, it := range newIts {
		oldIt, exists := oldIts[it.Name]
		if !exists {
			return false
		}
		oldItOffMap := lo.SliceToMap(oldIt.Offerings, func(o *cloudprovider.Offering) (state.OfferingKey, *cloudprovider.Offering) {
			return state.OfferingKey{Capacity: o.CapacityType(), Zone: o.Zone(), InstanceName: it.Name}, o
		})
		for _, of := range it.Offerings {
			ofKey := state.OfferingKey{Capacity: of.CapacityType(), Zone: of.Zone(), InstanceName: it.Name}
			oldOf, exists := oldItOffMap[ofKey]
			if !exists {
				return false
			}
			if oldOf.Price != of.Price {
				return false
			}
		}
	}
	return true
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("state.pricing").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
