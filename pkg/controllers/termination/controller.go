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
	"sync"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"

	provisioning "github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	operatorcontroller "github.com/aws/karpenter-core/pkg/operator/controller"
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

// Controller for the resource
type Controller struct {
	Terminator        *Terminator
	KubeClient        client.Client
	Recorder          events.Recorder
	mu                sync.Mutex
	TerminationRecord sets.String
}

// NewController constructs a controller instance
func NewController(ctx context.Context, clk clock.Clock, kubeClient client.Client, coreV1Client corev1.CoreV1Interface, recorder events.Recorder, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		KubeClient: kubeClient,
		Terminator: &Terminator{
			KubeClient:    kubeClient,
			CoreV1Client:  coreV1Client,
			CloudProvider: cloudProvider,
			EvictionQueue: NewEvictionQueue(ctx, coreV1Client, recorder),
			Clock:         clk,
		},
		Recorder:          recorder,
		TerminationRecord: sets.NewString(),
	}
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(controllerName).With("node", req.Name))
	ctx = injection.WithControllerName(ctx, controllerName)

	// 1. Retrieve node from reconcile request
	node := &v1.Node{}
	if err := c.KubeClient.Get(ctx, req.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			c.mu.Lock()
			c.TerminationRecord.Delete(req.String())
			c.mu.Unlock()
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// 2. Check if node is terminable
	if node.DeletionTimestamp.IsZero() || !lo.Contains(node.Finalizers, provisioning.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}

	// 3. Cordon node
	if err := c.Terminator.cordon(ctx, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("cordoning node %s, %w", node.Name, err)
	}

	// 4. Drain node
	drained, err := c.Terminator.drain(ctx, node)
	if err != nil {
		if !IsNodeDrainErr(err) {
			return reconcile.Result{}, err
		}
		c.Recorder.Publish(events.NodeFailedToDrain(node, err))
	}
	if !drained && !c.IsDrainTimedOut(node) {
		return reconcile.Result{Requeue: true}, nil
	}

	// 5. If fully drained, terminate the node
	if err := c.Terminator.terminate(ctx, node); err != nil {
		return reconcile.Result{}, fmt.Errorf("terminating node %s, %w", node.Name, err)
	}

	// 6. Record termination duration (time between deletion timestamp and finalizer removal)
	c.mu.Lock()
	if !c.TerminationRecord.Has(req.String()) {
		c.TerminationRecord.Insert(req.String())
		terminationSummary.Observe(time.Since(node.DeletionTimestamp.Time).Seconds())
	}
	c.mu.Unlock()

	return reconcile.Result{}, nil
}

// IsDrainTimedOut executes node drain when timeout
func (c *Controller) IsDrainTimedOut(node *v1.Node) bool {
	durationString := node.Labels["karpenter-drain-timeout"]
	if durationString == "" {
		return false
	}
	timeout, err := time.ParseDuration(durationString)
	if err != nil {
		return false
	}
	return time.Now().Sub(node.DeletionTimestamp.Time) >= timeout
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return controllerruntime.
		NewControllerManagedBy(m).
		Named(controllerName).
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
		)
}

func (c *Controller) LivenessProbe(_ *http.Request) error {
	return nil
}
