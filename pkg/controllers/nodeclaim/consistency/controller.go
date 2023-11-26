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

package consistency

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/events"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

type Controller struct {
	clock       clock.Clock
	kubeClient  client.Client
	checks      []Check
	recorder    events.Recorder
	lastScanned *cache.Cache
}

type Issue string

type Check interface {
	// Check performs the consistency check, this should return a list of slice discovered, or an empty
	// slice if no issues were found
	Check(context.Context, *v1.Node, *v1beta1.NodeClaim) ([]Issue, error)
}

// scanPeriod is how often we inspect and report issues that are found.
const scanPeriod = 10 * time.Minute

func NewController(clk clock.Clock, kubeClient client.Client, recorder events.Recorder,
	provider cloudprovider.CloudProvider) *Controller {

	return &Controller{
		clock:       clk,
		kubeClient:  kubeClient,
		recorder:    recorder,
		lastScanned: cache.New(scanPeriod, 1*time.Minute),
		checks: []Check{
			NewTermination(kubeClient),
			NewNodeShape(provider),
		},
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	if nodeClaim.Status.ProviderID == "" {
		return reconcile.Result{}, nil
	}
	// If we get an event before we should check for consistency checks, we ignore and wait
	if lastTime, ok := c.lastScanned.Get(string(nodeClaim.UID)); ok {
		if lastTime, ok := lastTime.(time.Time); ok {
			remaining := scanPeriod - c.clock.Since(lastTime)
			return reconcile.Result{RequeueAfter: remaining}, nil
		}
		// the above should always succeed
		return reconcile.Result{RequeueAfter: scanPeriod}, nil
	}
	c.lastScanned.SetDefault(string(nodeClaim.UID), c.clock.Now())

	// We assume the invariant that there is a single node for a single nodeClaim. If this invariant is violated,
	// then we assume this is bubbled up through the nodeClaim lifecycle controller and don't perform consistency checks
	node, err := nodeclaimutil.NodeForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {
		return reconcile.Result{}, nodeclaimutil.IgnoreDuplicateNodeError(nodeclaimutil.IgnoreNodeNotFoundError(err))
	}
	for _, check := range c.checks {
		issues, err := check.Check(ctx, node, nodeClaim)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("checking node with %T, %w", check, err)
		}
		for _, issue := range issues {
			logging.FromContext(ctx).Errorf("check failed, %s", issue)
			consistencyErrors.With(prometheus.Labels{checkLabel: reflect.TypeOf(check).Elem().Name()}).Inc()
			c.recorder.Publish(FailedConsistencyCheckEvent(nodeClaim, string(issue)))
		}
	}
	return reconcile.Result{RequeueAfter: scanPeriod}, nil
}

type NodeClaimController struct {
	*Controller
}

func NewNodeClaimController(clk clock.Clock, kubeClient client.Client, recorder events.Recorder,
	provider cloudprovider.CloudProvider) corecontroller.Controller {

	return corecontroller.Typed[*v1beta1.NodeClaim](kubeClient, &NodeClaimController{
		Controller: NewController(clk, kubeClient, recorder, provider),
	})
}

func (c *NodeClaimController) Name() string {
	return "nodeclaim.consistency"
}

func (c *NodeClaimController) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	return c.Controller.Reconcile(ctx, nodeClaim)
}

func (c *NodeClaimController) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1beta1.NodeClaim{}).
		Watches(
			&v1.Node{},
			nodeclaimutil.NodeEventHandler(c.kubeClient),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}
