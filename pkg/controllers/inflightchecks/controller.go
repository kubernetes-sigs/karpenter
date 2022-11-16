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
	"net/http"
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
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	operatorcontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/injection"
)

const controllerName = "inflightchecks"

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

func NewController(clk clock.Clock, kubeclient client.Client, recorder events.Recorder, provider cloudprovider.CloudProvider, cluster *state.Cluster) *Controller {
	return &Controller{
		clock:       clk,
		kubeClient:  kubeclient,
		recorder:    recorder,
		lastScanned: cache.New(scanPeriod, 1*time.Minute),
		checks: []Check{
			NewFailedInit(clk, provider),
			NewTermination(kubeclient),
			NewNodeShape(provider),
		}}
}

func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := logging.FromContext(ctx).Named(controllerName).With("node", req.Name)
	namespacedName := req.NamespacedName.String()
	if lastTime, ok := c.lastScanned.Get(namespacedName); ok {
		if lastTime, ok := lastTime.(time.Time); ok {
			remaining := scanPeriod - c.clock.Since(lastTime)
			return reconcile.Result{RequeueAfter: remaining}, nil
		}
		// the above should always succeed
		return reconcile.Result{RequeueAfter: scanPeriod}, nil
	}
	c.lastScanned.SetDefault(namespacedName, c.clock.Now())

	ctx = logging.WithLogger(ctx, log)
	ctx = injection.WithControllerName(ctx, controllerName)

	node := &v1.Node{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, node); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

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
			log.Errorf("checking node %s with %T, %s", node.Name, check, err)
		}
		for _, i := range issues {
			uniqueIssues[i.node.Name+i.message] = i
		}
	}

	for _, iss := range uniqueIssues {
		log.Infof("Inflight check failed for node %s, %s", iss.node.Name, iss.message)
		c.recorder.Publish(events.NodeInflightCheck(iss.node, iss.message))
	}
	return reconcile.Result{RequeueAfter: scanPeriod}, nil
}

func (c *Controller) Builder(ctx context.Context, m manager.Manager) operatorcontroller.Builder {
	return controllerruntime.
		NewControllerManagedBy(m).
		Named(controllerName).
		For(&v1.Node{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10})
}

func (c *Controller) LivenessProbe(req *http.Request) error {
	return nil
}
