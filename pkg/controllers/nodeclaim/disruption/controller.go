/*
Copyright 2023 The Kubernetes Authors.

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

package disruption

import (
	"context"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	operatorcontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
	"sigs.k8s.io/karpenter/pkg/utils/result"
)

var _ operatorcontroller.TypedController[*v1beta1.NodeClaim] = (*Controller)(nil)

type nodeClaimReconciler interface {
	Reconcile(context.Context, *v1beta1.NodePool, *v1beta1.NodeClaim) (reconcile.Result, error)
}

// Controller is a disruption controller that adds StatusConditions to nodeclaims when they meet certain disruption conditions
// e.g. When the NodeClaim has surpassed its owning provisioner's expirationTTL, then it is marked as "Expired" in the StatusConditions
type Controller struct {
	kubeClient client.Client

	drift      *Drift
	expiration *Expiration
	emptiness  *Emptiness
}

// NewController constructs a nodeclaim disruption controller
func NewController(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider) operatorcontroller.Controller {
	return operatorcontroller.Typed[*v1beta1.NodeClaim](kubeClient, &Controller{
		kubeClient: kubeClient,
		drift:      &Drift{cloudProvider: cloudProvider},
		expiration: &Expiration{kubeClient: kubeClient, clock: clk},
		emptiness:  &Emptiness{kubeClient: kubeClient, cluster: cluster, clock: clk},
	})
}

// Reconcile executes a control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	stored := nodeClaim.DeepCopy()
	nodePoolName, ok := nodeClaim.Labels[v1beta1.NodePoolLabelKey]
	if !ok {
		return reconcile.Result{}, nil
	}
	nodePool := &v1beta1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	var results []reconcile.Result
	var errs error
	reconcilers := []nodeClaimReconciler{
		c.expiration,
		c.drift,
		c.emptiness,
	}
	for _, reconciler := range reconcilers {
		res, err := reconciler.Reconcile(ctx, nodePool, nodeClaim)
		errs = multierr.Append(errs, err)
		results = append(results, res)
	}
	if !equality.Semantic.DeepEqual(stored, nodeClaim) {
		if err := c.kubeClient.Status().Update(ctx, nodeClaim); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	if errs != nil {
		return reconcile.Result{}, errs
	}
	return result.Min(results...), nil
}

func (c *Controller) Name() string {
	return "nodeclaim.disruption"
}

// PodEventHandler is a watcher on v1.Pods that maps Pods to NodeClaim based on the node names
// and enqueues reconcile.Requests for the NodeClaims
func PodEventHandler(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) (requests []reconcile.Request) {
		if name := o.(*v1.Pod).Spec.NodeName; name != "" {
			node := &v1.Node{}
			if err := c.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
				return []reconcile.Request{}
			}
			nodeClaimList := &v1beta1.NodeClaimList{}
			if err := c.List(ctx, nodeClaimList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}); err != nil {
				return []reconcile.Request{}
			}
			return lo.Map(nodeClaimList.Items, func(n v1beta1.NodeClaim, _ int) reconcile.Request {
				return reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(&n),
				}
			})
		}
		return requests
	})
}

// NodePoolEventHandler is a watcher on v1beta1.NodeClaim that maps Provisioner to NodeClaims based
// on the v1beta1.NodePoolLabelKey and enqueues reconcile.Requests for the NodeClaim
func NodePoolEventHandler(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) (requests []reconcile.Request) {
		nodeClaimList := &v1beta1.NodeClaimList{}
		if err := c.List(ctx, nodeClaimList, client.MatchingLabels(map[string]string{v1beta1.NodePoolLabelKey: o.GetName()})); err != nil {
			return requests
		}
		return lo.Map(nodeClaimList.Items, func(n v1beta1.NodeClaim, _ int) reconcile.Request {
			return reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&n),
			}
		})
	})
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return operatorcontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1beta1.NodeClaim{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Watches(
			&v1beta1.NodePool{},
			NodePoolEventHandler(c.kubeClient),
		).
		Watches(
			&v1.Pod{},
			PodEventHandler(c.kubeClient),
		),
	)
}
