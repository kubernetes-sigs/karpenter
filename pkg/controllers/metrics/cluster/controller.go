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

package cluster

import (
	"context"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"

	"github.com/prometheus/client_golang/prometheus"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/state/cost"
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
		[]string{metrics.NodePoolLabel},
	)
)

type Controller struct {
	client      client.Client
	clusterCost *cost.ClusterCost
	npMap       map[string]*v1.NodePool
}

func NewController(client client.Client, clusterCost *cost.ClusterCost) *Controller {
	return &Controller{
		client:      client,
		clusterCost: clusterCost,
		npMap:       make(map[string]*v1.NodePool),
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	// List all nodepools in the cluster
	var nodepools v1.NodePoolList
	if err := c.client.List(ctx, &nodepools); err != nil {
		return reconciler.Result{}, err
	}

	for _, np := range c.npMap {
		if !lo.ContainsBy(nodepools.Items, func(np2 v1.NodePool) bool {
			return np.UID == np2.UID
		}) {
			ClusterCost.Delete(map[string]string{
				metrics.NodePoolLabel: np.Name,
			})
			delete(c.npMap, string(np.UID))
		}

	}

	// Update cost metrics for each nodepool
	for _, np := range nodepools.Items {
		cost := c.clusterCost.GetNodepoolCost(&np)
		ClusterCost.Set(cost, map[string]string{
			metrics.NodePoolLabel: np.Name,
		})
		c.npMap[string(np.UID)] = &np
	}

	return reconciler.Result{RequeueAfter: time.Second * 10}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("metrics.cost").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
