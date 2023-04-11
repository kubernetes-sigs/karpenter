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
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

const pollingPeriod = 5 * time.Second

type Controller struct {
	cluster *state.Cluster
}

func NewController(cluster *state.Cluster) *Controller {
	return &Controller{
		cluster: cluster,
	}
}

func (c *Controller) Name() string {
	return "metric_scraper"
}

func (c *Controller) Builder(_ context.Context, mgr manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(mgr)
}

func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// Reset all the gauges ahead of setting the new gauges with the new labels
	for _, v := range gauges {
		v.Reset()
	}
	// Populate metrics
	c.cluster.ForEachNode(func(n *state.StateNode) bool {
		if n.Node == nil {
			return true
		}
		for gaugeVec, resourceList := range map[*prometheus.GaugeVec]v1.ResourceList{
			overheadGauge:       resources.Subtract(n.Node.Status.Capacity, n.Node.Status.Allocatable),
			podRequestsGauge:    resources.Subtract(n.PodRequests(), n.DaemonSetRequests()),
			podLimitsGauge:      resources.Subtract(n.PodLimits(), n.DaemonSetLimits()),
			daemonRequestsGauge: n.DaemonSetRequests(),
			daemonLimitsGauge:   n.DaemonSetLimits(),
			allocatableGauge:    n.Node.Status.Allocatable,
		} {
			c.set(gaugeVec, n.Node, resourceList)
		}
		return true
	})
	return reconcile.Result{RequeueAfter: pollingPeriod}, nil
}

// set the value for the node gauge and returns a slice of the labels for the gauges set
func (c *Controller) set(gaugeVec *prometheus.GaugeVec, node *v1.Node, resourceList v1.ResourceList) {
	for resourceName, quantity := range resourceList {
		// Reformat resource type to be consistent with Prometheus naming conventions (snake_case)
		resourceLabels := getNodeLabels(node, strings.ReplaceAll(strings.ToLower(string(resourceName)), "-", "_"))
		if resourceName == v1.ResourceCPU {
			gaugeVec.With(resourceLabels).Set(float64(quantity.MilliValue()) / float64(1000))
		} else {
			gaugeVec.With(resourceLabels).Set(float64(quantity.Value()))
		}
	}
}
