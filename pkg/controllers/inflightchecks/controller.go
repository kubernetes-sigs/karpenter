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

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning"
	"github.com/aws/karpenter-core/pkg/events"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
)

var _ corecontroller.TypedController[*v1alpha5.Machine] = (*Controller)(nil)

type Controller struct {
	clock       clock.Clock
	kubeClient  client.Client
	checks      []Check
	recorder    events.Recorder
	lastScanned *cache.Cache
}

type Issue string

type Check interface {
	// Check performs the inflight check, this should return a list of slice discovered, or an empty
	// slice if no issues were found
	Check(ctx context.Context, node *v1.Node, machine *v1alpha5.Machine, provisioner *v1alpha5.Provisioner, pdbs *deprovisioning.PDBLimits) ([]Issue, error)
}

// scanPeriod is how often we inspect and report issues that are found.
const scanPeriod = 10 * time.Minute

func NewController(clk clock.Clock, kubeClient client.Client, recorder events.Recorder,
	provider cloudprovider.CloudProvider) corecontroller.Controller {

	return corecontroller.Typed[*v1alpha5.Machine](kubeClient, &Controller{
		clock:       clk,
		kubeClient:  kubeClient,
		recorder:    recorder,
		lastScanned: cache.New(scanPeriod, 1*time.Minute),
		checks: []Check{
			NewFailedInit(clk, provider),
			NewTermination(kubeClient),
			NewNodeShape(provider),
		}},
	)
}

func (c *Controller) Name() string {
	return "inflightchecks"
}

func (c *Controller) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	// If we get an event before we should check for inflight checks, we ignore and wait
	if lastTime, ok := c.lastScanned.Get(client.ObjectKeyFromObject(machine).String()); ok {
		if lastTime, ok := lastTime.(time.Time); ok {
			remaining := scanPeriod - c.clock.Since(lastTime)
			return reconcile.Result{RequeueAfter: remaining}, nil
		}
		// the above should always succeed
		return reconcile.Result{RequeueAfter: scanPeriod}, nil
	}
	c.lastScanned.SetDefault(client.ObjectKeyFromObject(machine).String(), c.clock.Now())

	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: machine.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		// provisioner is missing, node should be removed soon
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	node, err := machineutil.NodeForMachine(ctx, c.kubeClient, machine)
	if err != nil {
		// TODO @joinnis: Figure out how to handle this error better
		return reconcile.Result{}, err
	}

	pdbs, err := deprovisioning.NewPDBLimits(ctx, c.kubeClient)
	if err != nil {
		return reconcile.Result{}, err
	}
	var allIssues []Issue
	for _, check := range c.checks {
		issues, err := check.Check(ctx, node, machine, provisioner, pdbs)
		if err != nil {
			logging.FromContext(ctx).Errorf("checking node with %T, %s", check, err)
		}
		allIssues = append(allIssues, issues...)
	}
	for _, iss := range allIssues {
		logging.FromContext(ctx).Infof("Inflight check failed for node, %s", iss)
		c.recorder.Publish(events.NodeInflightCheck(node, string(iss)))
	}
	return reconcile.Result{RequeueAfter: scanPeriod}, nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha5.Machine{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}
