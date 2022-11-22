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

package inflightchecks

import (
	"context"
	"time"

	"github.com/patrickmn/go-cache"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning"
	"github.com/aws/karpenter-core/pkg/events"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

const controllerName = "inflightchecks"

var _ corecontroller.TypedController[*v1.Node] = (*Controller)(nil)

type Controller struct {
	clock       clock.Clock
	kubeClient  client.Client
	checks      []Check
	recorder    events.Recorder
	lastScanned *cache.Cache
}

type Check interface {
	// Check performs the inflight check, this should return a list of slice discovered, or an empty
	// slice if no issues were found
	Check(ctx context.Context, node *v1.Node, provisioner *v1alpha5.Provisioner, pdbs *deprovisioning.PDBLimits) ([]Issue, error)
}

type Issue struct {
	node    *v1.Node
	message string
}

// scanPeriod is how often we inspect and report issues that are found.
const scanPeriod = 10 * time.Minute

func NewController(clk clock.Clock, kubeClient client.Client, recorder events.Recorder,
	provider cloudprovider.CloudProvider) corecontroller.Controller {

	return corecontroller.For[*v1.Node](kubeClient, &Controller{
		clock:       clk,
		kubeClient:  kubeClient,
		recorder:    recorder,
		lastScanned: cache.New(scanPeriod, 1*time.Minute),
		checks: []Check{
			NewFailedInit(clk, provider),
			NewTermination(kubeClient),
			NewNodeShape(provider),
		}},
	).Named(controllerName)
}

func (c *Controller) Reconcile(ctx context.Context, node *v1.Node) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("node", node.Name))

	// If we get an event before we should check for inflight checks, we ignore and wait
	if lastTime, ok := c.lastScanned.Get(client.ObjectKeyFromObject(node).String()); ok {
		if lastTime, ok := lastTime.(time.Time); ok {
			remaining := scanPeriod - c.clock.Since(lastTime)
			return reconcile.Result{RequeueAfter: remaining}, nil
		}
		// the above should always succeed
		return reconcile.Result{RequeueAfter: scanPeriod}, nil
	}
	c.lastScanned.SetDefault(client.ObjectKeyFromObject(node).String(), c.clock.Now())

	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: node.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		// provisioner is missing, node should be removed soon
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	pdbs, err := deprovisioning.NewPDBLimits(ctx, c.kubeClient)
	if err != nil {
		return reconcile.Result{}, err
	}
	uniqueIssues := map[string]Issue{}
	for _, check := range c.checks {
		issues, err := check.Check(ctx, node, provisioner, pdbs)
		if err != nil {
			logging.FromContext(ctx).Errorf("checking node with %T, %s", check, err)
		}
		for _, i := range issues {
			uniqueIssues[i.node.Name+i.message] = i
		}
	}
	for _, iss := range uniqueIssues {
		logging.FromContext(ctx).Infof("Inflight check failed for node, %s", iss.message)
		c.recorder.Publish(events.NodeInflightCheck(iss.node, iss.message))
	}
	return reconcile.Result{RequeueAfter: scanPeriod}, nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1.Node{}).
		Named(controllerName).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}
