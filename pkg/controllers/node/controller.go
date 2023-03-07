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

package node

import (
	"context"

	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/utils/result"
)

type nodeReconciler interface {
	Reconcile(context.Context, *v1alpha5.Provisioner, *v1.Node) (reconcile.Result, error)
}

var _ corecontroller.TypedController[*v1.Node] = (*Controller)(nil)

// Controller manages a set of properties on karpenter provisioned nodes, such as
// taints, labels, finalizers.
type Controller struct {
	kubeClient client.Client
	cluster    *state.Cluster

	emptiness *Emptiness
	drift     *Drift
}

// NewController constructs a nodeController instance
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster) corecontroller.Controller {
	return corecontroller.Typed[*v1.Node](kubeClient, &Controller{
		kubeClient: kubeClient,
		cluster:    cluster,

		emptiness: &Emptiness{kubeClient: kubeClient, clock: clk, cluster: cluster},
		drift:     &Drift{kubeClient: kubeClient, cloudProvider: cloudProvider},
	})
}

func (c *Controller) Name() string {
	return "node"
}

// Reconcile executes a reallocation control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, node *v1.Node) (reconcile.Result, error) {
	stored := node.DeepCopy()
	if _, ok := node.Labels[v1alpha5.ProvisionerNameLabelKey]; !ok {
		return reconcile.Result{}, nil
	}
	if !node.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: node.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("provisioner", provisioner.Name))

	reconcilers := []nodeReconciler{
		c.emptiness,
	}
	if settings.FromContext(ctx).DriftEnabled {
		reconcilers = append(reconcilers, c.drift)
	}

	// Execute Reconcilers
	var results []reconcile.Result
	var errs error
	for _, reconciler := range reconcilers {
		res, err := reconciler.Reconcile(ctx, provisioner, node)
		errs = multierr.Append(errs, err)
		results = append(results, res)
	}
	if !equality.Semantic.DeepEqual(stored, node) {
		if err := c.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, err
		}
	}
	return result.Min(results...), errs
}

func (c *Controller) Builder(ctx context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1.Node{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Watches(
			// Reconcile all nodes related to a provisioner when it changes.
			&source.Kind{Type: &v1alpha5.Provisioner{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
				nodes := &v1.NodeList{}
				if err := c.kubeClient.List(ctx, nodes, client.MatchingLabels(map[string]string{v1alpha5.ProvisionerNameLabelKey: o.GetName()})); err != nil {
					logging.FromContext(ctx).Errorf("Failed to list nodes when mapping expiration watch events, %s", err)
					return requests
				}
				for _, node := range nodes.Items {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
				}
				return requests
			}),
		).
		Watches(
			// Reconcile node when a pod assigned to it changes.
			&source.Kind{Type: &v1.Pod{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
				if name := o.(*v1.Pod).Spec.NodeName; name != "" {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
				}
				return requests
			}),
		))
}
