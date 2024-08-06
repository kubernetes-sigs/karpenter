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

package nodepool

import (
	"context"
	"strings"
	"time"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	resourceTypeLabel = "resource_type"
	nodePoolNameLabel = "nodepool"
)

var (
	limit = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "limit",
			Help:      "Limits specified on the nodepool that restrict the quantity of resources provisioned. Labeled by nodepool name and resource type.",
		},
		[]string{
			resourceTypeLabel,
			nodePoolNameLabel,
		},
	)
	usage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "usage",
			Help:      "The amount of resources that have been provisioned for a nodepool. Labeled by nodepool name and resource type.",
		},
		[]string{
			resourceTypeLabel,
			nodePoolNameLabel,
		},
	)
)

func init() {
	crmetrics.Registry.MustRegister(limit, usage)
}

type Controller struct {
	kubeClient  client.Client
	metricStore *metrics.Store
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient:  kubeClient,
		metricStore: metrics.NewStore(),
	}
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "metrics.nodepool")

	nodePool := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, nodePool); err != nil {
		if errors.IsNotFound(err) {
			c.metricStore.Delete(req.NamespacedName.String())
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	c.metricStore.Update(req.NamespacedName.String(), buildMetrics(nodePool))
	// periodically update our metrics per nodepool even if nothing has changed
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

func buildMetrics(nodePool *v1.NodePool) (res []*metrics.StoreMetric) {
	for gaugeVec, resourceList := range map[*prometheus.GaugeVec]corev1.ResourceList{
		usage: nodePool.Status.Resources,
		limit: getLimits(nodePool),
	} {
		for k, v := range resourceList {
			res = append(res, &metrics.StoreMetric{
				GaugeVec: gaugeVec,
				Labels:   makeLabels(nodePool, strings.ReplaceAll(strings.ToLower(string(k)), "-", "_")),
				Value:    lo.Ternary(k == corev1.ResourceCPU, float64(v.MilliValue())/float64(1000), float64(v.Value())),
			})
		}
	}
	return res
}

func getLimits(nodePool *v1.NodePool) corev1.ResourceList {
	if nodePool.Spec.Limits != nil {
		return corev1.ResourceList(nodePool.Spec.Limits)
	}
	return corev1.ResourceList{}
}

func makeLabels(nodePool *v1.NodePool, resourceTypeName string) prometheus.Labels {
	return prometheus.Labels{
		resourceTypeLabel: resourceTypeName,
		nodePoolNameLabel: nodePool.Name,
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("metrics.nodepool").
		For(&v1.NodePool{}).
		Complete(c)
}
