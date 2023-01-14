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
	"time"

	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

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

var _ corecontroller.FinalizingTypedController[*v1.Node] = (*Controller)(nil)

// Controller for the resource
type Controller struct {
	terminator *Terminator
	kubeClient client.Client
	recorder   events.Recorder
}

// NewController constructs a terminationController instance
func NewController(kubeClient client.Client, terminator *Terminator, recorder events.Recorder) corecontroller.Controller {
	return corecontroller.Typed[*v1.Node](kubeClient, &Controller{
		kubeClient: kubeClient,
		terminator: terminator,
		recorder:   recorder,
	})
}

func (c *Controller) Name() string {
	return "termination"
}

func (c *Controller) Reconcile(_ context.Context, _ *v1.Node) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (c *Controller) Finalize(ctx context.Context, node *v1.Node) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(node, v1alpha5.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	if err := c.terminator.Cordon(ctx, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("cordoning node, %w", err)
	}
	if err := c.terminator.Drain(ctx, node); err != nil {
		if IsNodeDrainError(err) {
			c.recorder.Publish(events.NodeFailedToDrain(node, err))
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, fmt.Errorf("draining node, %w", err)
	}
	return c.terminator.TerminateNode(ctx, node)
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1.Node{}).
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
