/*
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

package garbagecollection

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/metrics"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func NewController(c clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return &Controller{
		clock:         c,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Name() string {
	return "nodeclaim.garbagecollection"
}

func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	nodeClaimList, err := nodeclaimutil.List(ctx, c.kubeClient)
	if err != nil {
		return reconcile.Result{}, err
	}
	cloudProviderNodeClaims, err := c.cloudProvider.List(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	cloudProviderNodeClaims = lo.Filter(cloudProviderNodeClaims, func(nc *v1beta1.NodeClaim, _ int) bool {
		return nc.DeletionTimestamp.IsZero()
	})
	cloudProviderProviderIDs := sets.New[string](lo.Map(cloudProviderNodeClaims, func(nc *v1beta1.NodeClaim, _ int) string {
		return nc.Status.ProviderID
	})...)
	nodeClaims := lo.Filter(lo.ToSlicePtr(nodeClaimList.Items), func(n *v1beta1.NodeClaim, _ int) bool {
		return n.StatusConditions().GetCondition(v1beta1.Launched).IsTrue() &&
			n.DeletionTimestamp.IsZero() &&
			c.clock.Since(n.StatusConditions().GetCondition(v1beta1.Launched).LastTransitionTime.Inner.Time) > time.Second*10 &&
			!cloudProviderProviderIDs.Has(n.Status.ProviderID)
	})

	errs := make([]error, len(nodeClaims))
	workqueue.ParallelizeUntil(ctx, 20, len(nodeClaims), func(i int) {
		if err := c.kubeClient.Delete(ctx, nodeClaims[i]); err != nil {
			errs[i] = client.IgnoreNotFound(err)
			return
		}
		logging.FromContext(ctx).
			With(
				lo.Ternary(nodeClaims[i].IsMachine, "machine", "nodeclaim"), nodeClaims[i].Name,
				"provider-id", nodeClaims[i].Status.ProviderID,
				lo.Ternary(nodeClaims[i].IsMachine, "provisioner", "nodepool"), nodeclaimutil.OwnerKey(nodeClaims[i]).Name,
			).
			Debugf("garbage collecting %s with no cloudprovider representation", lo.Ternary(nodeClaims[i].IsMachine, "machine", "nodeclaim"))
		metrics.NodeClaimsTerminatedCounter.With(prometheus.Labels{
			metrics.ReasonLabel:   "garbage_collected",
			metrics.NodePoolLabel: nodeClaims[i].Labels[v1beta1.NodePoolLabelKey],
		}).Inc()
	})
	if err = multierr.Combine(errs...); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: time.Minute * 2}, nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.NewSingletonManagedBy(m)
}
