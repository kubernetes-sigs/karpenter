/*
Copyright The Kubernetes Authors.

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

package cost

import (
	"context"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"

	"github.com/prometheus/client_golang/prometheus"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// These are alpha metrics, they may not stay. Do not rely on them.
var (
	ClusterCost = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "cost_total",
			Help:      "ALPHA METRIC. Total cost of the nodepool from Karpenter's perspective. Units are determined by the cloud provider. Not an authoritative source for billing. Includes modifications due to NodeOverlays",
		},
		[]string{},
	)
)

type Controller struct {
	client      client.Client
	clusterCost *state.ClusterCost
}

func NewController(client client.Client, clusterCost *state.ClusterCost) *Controller {
	return &Controller{
		client:      client,
		clusterCost: clusterCost,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	// List all nodepools in the cluster
	var nodepools v1.NodePoolList
	if err := c.client.List(ctx, &nodepools); err != nil {
		return reconciler.Result{}, err
	}
	sum := 0.0
	// Update cost metrics for each nodepool
	for _, np := range nodepools.Items {
		cost := c.clusterCost.GetNodepoolCost(&np)
		ClusterCost.Set(cost, map[string]string{
			metrics.NodePoolLabel: np.Name,
		})
		sum += cost
	}
	ClusterCost.Set(sum, nil)

	return reconciler.Result{RequeueAfter: time.Second * 10}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("metrics.cost").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
