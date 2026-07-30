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

package nodeclaimgc

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

// gracePeriod is how long after a NodeClaim is created we wait before verifying it still exists. It only needs to
// comfortably exceed the worst-case delay for the provisioner's post-create seed to land
const gracePeriod = 15 * time.Second

type Controller struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

func NewController(kubeClient client.Client, cluster *state.Cluster) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		cluster:    cluster,
	}
}

func (c *Controller) Name() string {
	return "state.nodeclaimgc"
}

// Garbage collects unlaunched NodeClaim entries from cluster state that no longer exist on the API
// server, so a create/delete race in the seed window can't hold Synced() false indefinitely as seen
// in https://github.com/kubernetes-sigs/karpenter/issues/3176.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	// This runs a grace period after the NodeClaim was created, so the seed has landed. If the NodeClaim still
	// exists or there is no entry in nodeClaimNameToProviderID (cluster state), there's nothing to heal.
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: req.Name}, &v1.NodeClaim{}); err != nil {
		if !errors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		if c.cluster.UnlaunchedNodeClaimExists(req.Name) {
			log.FromContext(ctx).WithValues("NodeClaim", klog.KRef("", req.Name)).Info("garbage collecting stale unlaunched nodeclaim from cluster state")
			c.cluster.DeleteNodeClaim(req.Name)
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		Watches(&v1.NodeClaim{}, handler.Funcs{
			CreateFunc: func(_ context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				q.AddAfter(reconcile.Request{NamespacedName: types.NamespacedName{Name: e.Object.GetName()}}, gracePeriod)
			},
		}).
		Complete(c)
}
