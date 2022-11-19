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

package termination

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/injection"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
)

const controllerName = "termination"

var (
	terminationSummary = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Namespace:  "karpenter",
			Subsystem:  "nodes",
			Name:       "termination_time_seconds",
			Help:       "The time taken between a node's deletion request and the removal of its finalizer",
			Objectives: metrics.SummaryObjectives(),
		},
	)
)

func init() {
	crmetrics.Registry.MustRegister(terminationSummary)
}

var _ corecontroller.TypedControllerWithFinalizer[*v1.Node] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	Terminator *Terminator
	KubeClient client.Client
	Recorder   events.Recorder
}

// NewController constructs a terminationController instance
func NewController(clk clock.Clock, kubeClient client.Client, evictionQueue *EvictionQueue,
	recorder events.Recorder, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {

	return corecontroller.For[*v1.Node](kubeClient, &Controller{
		KubeClient: kubeClient,
		Terminator: &Terminator{
			KubeClient:    kubeClient,
			CloudProvider: cloudProvider,
			EvictionQueue: evictionQueue,
			Clock:         clk,
		},
		Recorder: recorder,
	})
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(_ context.Context, node *v1.Node) (*v1.Node, reconcile.Result, error) {
	return node, reconcile.Result{}, nil
}

func (c *Controller) Finalize(ctx context.Context, node *v1.Node) (*v1.Node, reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(controllerName).With("node", node.Name))
	ctx = injection.WithControllerName(ctx, controllerName)

	// 1. Cordon node
	node = c.Terminator.cordon(ctx, node)
	// 2. Drain node
	drained, err := c.Terminator.drain(ctx, node)
	if err != nil {
		if !IsNodeDrainErr(err) {
			return node, reconcile.Result{}, err
		}
		c.Recorder.Publish(events.NodeFailedToDrain(node, err))
	}
	if !drained {
		return node, reconcile.Result{Requeue: true}, nil
	}
	// 3. If fully drained, terminate the node
	if err = c.Terminator.terminate(ctx, node); err != nil {
		return node, reconcile.Result{}, fmt.Errorf("terminating node %s, %w", node.Name, err)
	}
	controllerutil.RemoveFinalizer(node, v1alpha5.TerminationFinalizer)
	return node, reconcile.Result{}, nil
}

func (c *Controller) OnFinalizerRemoved(ctx context.Context, node *v1.Node) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(controllerName).With("node", node.Name))
	logging.FromContext(ctx).Infof("deleted node")
	terminationSummary.Observe(time.Since(node.DeletionTimestamp.Time).Seconds())
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.TypedBuilder {
	return corecontroller.NewTypedBuilderAdapter(controllerruntime.
		NewControllerManagedBy(m).
		Named(controllerName).
		WithOptions(
			controller.Options{
				RateLimiter: workqueue.NewMaxOfRateLimiter(
					workqueue.NewItemExponentialFailureRateLimiter(100*time.Millisecond, 10*time.Second),
					// 10 qps, 100 bucket size
					&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
				),
				MaxConcurrentReconciles: 10,
			},
		))
}

func (c *Controller) LivenessProbe(_ *http.Request) error {
	return nil
}
