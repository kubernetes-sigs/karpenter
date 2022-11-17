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
	"net/http"

	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/utils/result"
)

const controllerName = "node"

// NewController constructs a controller instance
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster) *Controller {
	return &Controller{
		kubeClient:     kubeClient,
		cluster:        cluster,
		initialization: &Initialization{kubeClient: kubeClient, cloudProvider: cloudProvider},
		emptiness:      &Emptiness{kubeClient: kubeClient, clock: clk, cluster: cluster},
	}
}

// Controller manages a set of properties on karpenter provisioned nodes, such as
// taints, labels, finalizers.
type Controller struct {
	kubeClient     client.Client
	cluster        *state.Cluster
	initialization *Initialization
	emptiness      *Emptiness
	finalizer      *Finalizer
}

// Reconcile executes a reallocation control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, node *v1.Node) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(controllerName).With("node", node.Name))
	if _, ok := node.Labels[v1alpha5.ProvisionerNameLabelKey]; !ok {
		return reconcile.Result{}, nil
	}
	if !node.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	// 2. Retrieve Provisioner
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: node.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// 3. Execute reconcilers
	var results []reconcile.Result
	var errs error
	for _, reconciler := range []interface {
		Reconcile(context.Context, *v1alpha5.Provisioner, *v1.Node) (reconcile.Result, error)
	}{
		c.initialization,
		c.emptiness,
		c.finalizer,
	} {
		res, err := reconciler.Reconcile(ctx, provisioner, node)
		errs = multierr.Append(errs, err)
		results = append(results, res)
	}
	return result.Min(results...), errs
}

func (c *Controller) Finalize(_ context.Context, _ *v1.Node) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (c *Controller) Builder(ctx context.Context, m manager.Manager) corecontroller.TypedBuilder {
	// Enqueues a reconcile request when nominated node expiration is triggered
	ch := make(chan event.GenericEvent, 300)
	c.cluster.AddNominatedNodeEvictionObserver(func(nodeName string) {
		ch <- event.GenericEvent{Object: &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		}}
	})

	return corecontroller.NewTypedBuilderAdapter(controllerruntime.
		NewControllerManagedBy(m).
		Named(controllerName).
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
		).
		Watches(&source.Channel{Source: ch}, &handler.EnqueueRequestForObject{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}))
}

func (c *Controller) LivenessProbe(_ *http.Request) error {
	return nil
}
