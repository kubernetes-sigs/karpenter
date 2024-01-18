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
	"knative.dev/pkg/logging"

	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/metrics"
	operatorcontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
)

const (
	resourceTypeLabel = "resource_type"
	nodePoolNameLabel = "nodepool"
	nodePoolSubsystem = "nodepool"
)

var (
	limitGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: nodePoolSubsystem,
			Name:      "limit",
			Help:      "The nodepool limits are the limits specified on the nodepool that restrict the quantity of resources provisioned. Labeled by nodepool name and resource type.",
		},
		[]string{
			resourceTypeLabel,
			nodePoolNameLabel,
		},
	)
	usageGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: nodePoolSubsystem,
			Name:      "usage",
			Help:      "The nodepool usage is the amount of resources that have been provisioned by a particular nodepool. Labeled by nodepool name and resource type.",
		},
		[]string{
			resourceTypeLabel,
			nodePoolNameLabel,
		},
	)
)

func init() {
	crmetrics.Registry.MustRegister(limitGaugeVec, usageGaugeVec)
}

type Controller struct {
	kubeClient  client.Client
	metricStore *metrics.Store
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client) operatorcontroller.Controller {
	return &Controller{
		kubeClient:  kubeClient,
		metricStore: metrics.NewStore(),
	}
}

func (c *Controller) Name() string {
	return "metrics.nodepool"
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(c.Name()).With("nodepool", req.Name))
	nodePool := &v1beta1.NodePool{}
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

func buildMetrics(nodePool *v1beta1.NodePool) (res []*metrics.StoreMetric) {
	for gaugeVec, resourceList := range map[*prometheus.GaugeVec]v1.ResourceList{
		usageGaugeVec: nodePool.Status.Resources,
		limitGaugeVec: getLimits(nodePool),
	} {
		for k, v := range resourceList {
			res = append(res, &metrics.StoreMetric{
				GaugeVec: gaugeVec,
				Labels:   makeLabels(nodePool, strings.ReplaceAll(strings.ToLower(string(k)), "-", "_")),
				Value:    lo.Ternary(k == v1.ResourceCPU, float64(v.MilliValue())/float64(1000), float64(v.Value())),
			})
		}
	}
	return res
}

func getLimits(nodePool *v1beta1.NodePool) v1.ResourceList {
	if nodePool.Spec.Limits != nil {
		return v1.ResourceList(nodePool.Spec.Limits)
	}
	return v1.ResourceList{}
}

func makeLabels(nodePool *v1beta1.NodePool, resourceTypeName string) prometheus.Labels {
	return prometheus.Labels{
		resourceTypeLabel: resourceTypeName,
		nodePoolNameLabel: nodePool.Name,
	}
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return operatorcontroller.Adapt(
		controllerruntime.
			NewControllerManagedBy(m).
			For(&v1beta1.NodePool{}),
	)
}
