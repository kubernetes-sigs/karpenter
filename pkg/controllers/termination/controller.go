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

	"github.com/samber/lo"
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/injection"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
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

// Controller for the resource
type Controller struct {
	Terminator *Terminator
	KubeClient client.Client
	Recorder   events.Recorder
}

// NewController constructs a terminationController instance
func NewController(clk clock.Clock, kubeClient client.Client, evictionQueue *EvictionQueue,
	recorder events.Recorder, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {

	return &Controller{
		KubeClient: kubeClient,
		Terminator: &Terminator{
			KubeClient:    kubeClient,
			CloudProvider: cloudProvider,
			EvictionQueue: evictionQueue,
			Clock:         clk,
		},
		Recorder: recorder,
	}
}

func (c *Controller) Name() string {
	return "termination"
}

func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(c.Name()).With("node", req.Name))
	ctx = injection.WithControllerName(ctx, c.Name())

	node := &v1.Node{}
	if err := c.KubeClient.Get(ctx, req.NamespacedName, node); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if err := c.Terminator.cordon(ctx, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("cordoning node, %w", err)
	}
	if err := c.Terminator.drain(ctx, node); err != nil {
		if IsNodeDrainErr(err) {
			c.Recorder.Publish(events.NodeFailedToDrain(node, err))
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, fmt.Errorf("draining node, %w", err)
	}
	if err := c.Terminator.terminate(ctx, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("terminating node, %w", err)
	}
	terminationSummary.Observe(time.Since(node.DeletionTimestamp.Time).Seconds())
	return reconcile.Result{}, nil
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
		).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return !obj.GetDeletionTimestamp().IsZero()
		})).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return lo.Contains(obj.GetFinalizers(), v1alpha5.TerminationFinalizer)
		})))
}
