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

	"github.com/awslabs/operatorpkg/serrors"
	"k8s.io/klog/v2"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"go.uber.org/multierr"

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

	var errs error
	for _, np := range npl.Items {
		newIts, err := c.cloudProvider.GetInstanceTypes(ctx, &np)
		if err != nil {
			errs = multierr.Append(errs, err)
			continue
		}
		c.clusterCost.UpdateOfferings(ctx, &np, newIts)
	}
	mutable, err := c.refreshRunningNodePrices(ctx)
	errs = multierr.Append(errs, err)
	if errs != nil {
		return reconciler.Result{}, fmt.Errorf("refreshing pricing info, %w", errs)
	}
	if mutable {
		return reconciler.Result{RequeueAfter: time.Minute}, nil
	}

	return reconciler.Result{RequeueAfter: 1 * time.Hour}, nil
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

// Mutable running-node prices refresh independently of Kubernetes label convergence.
func (c *PricingController) refreshRunningNodePrices(ctx context.Context) (bool, error) {
	var errs error
	claims := &v1.NodeClaimList{}
	if err := c.client.List(ctx, claims); err != nil {
		return false, err
	}
	mutable := []*v1.NodeClaim{}
	for i := range claims.Items {
		if claims.Items[i].Annotations[v1.CapacityTypeTransitionsAnnotationKey] != "" {
			mutable = append(mutable, &claims.Items[i])
		}
	}
	if len(mutable) > 0 {
		states, err := cloudprovider.RefreshNodeClaimStates(ctx, c.cloudProvider, mutable)
		errs = multierr.Append(errs, err)
		for _, nc := range mutable {
			if state, ok := states[nc.Status.ProviderID]; ok && state.ValidAt(time.Now()) {
				c.clusterCost.UpdateNodeClaimState(nc, state)
			} else {
				errs = multierr.Append(errs, serrors.Wrap(fmt.Errorf("fresh running-node price unavailable"), "NodeClaim", klog.KObj(nc)))
			}
		}
	}
	return len(mutable) > 0, errs
}
