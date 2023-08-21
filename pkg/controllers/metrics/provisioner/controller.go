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

package provisioner

import (
	"context"
	"strings"
	"time"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"knative.dev/pkg/logging"

	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/metrics"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
)

const (
	provisionerResourceType = "resource_type"
	provisionerName         = "provisioner"
)

var (
	limitGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "provisioner",
			Name:      "limit",
			Help:      "The Provisioner Limits are the limits specified on the provisioner that restrict the quantity of resources provisioned. Labeled by provisioner name and resource type.",
		},
		labelNames(),
	)
	usageGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "provisioner",
			Name:      "usage",
			Help:      "The Provisioner Usage is the amount of resources that have been provisioned by a particular provisioner. Labeled by provisioner name and resource type.",
		},
		labelNames(),
	)
	usagePctGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "provisioner",
			Name:      "usage_pct",
			Help:      "The Provisioner Usage Percentage is the percentage of each resource used based on the resources provisioned and the limits that have been configured in the range [0,100].  Labeled by provisioner name and resource type.",
		},
		labelNames(),
	)
)

func init() {
	crmetrics.Registry.MustRegister(limitGaugeVec)
	crmetrics.Registry.MustRegister(usageGaugeVec)
	crmetrics.Registry.MustRegister(usagePctGaugeVec)
}

func labelNames() []string {
	return []string{
		provisionerResourceType,
		provisionerName,
	}
}

type Controller struct {
	kubeClient  client.Client
	metricStore *metrics.Store
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client) corecontroller.Controller {
	return &Controller{
		kubeClient:  kubeClient,
		metricStore: metrics.NewStore(),
	}
}

func (c *Controller) Name() string {
	return "provisioner_metrics"
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(c.Name()).With("provisioner", req.Name))
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, provisioner); err != nil {
		if errors.IsNotFound(err) {
			c.metricStore.Delete(req.NamespacedName.String())
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	c.metricStore.Update(req.NamespacedName.String(), buildMetrics(provisioner))
	// periodically update our metrics per provisioner even if nothing has changed
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

func buildMetrics(provisioner *v1alpha5.Provisioner) (res []*metrics.StoreMetric) {
	for gaugeVec, resourceList := range map[*prometheus.GaugeVec]v1.ResourceList{
		usageGaugeVec:    provisioner.Status.Resources,
		limitGaugeVec:    getLimits(provisioner),
		usagePctGaugeVec: getUsagePercentage(provisioner),
	} {
		for k, v := range resourceList {
			res = append(res, &metrics.StoreMetric{
				GaugeVec: gaugeVec,
				Labels:   makeLabels(provisioner, strings.ReplaceAll(strings.ToLower(string(k)), "-", "_")),
				Value:    lo.Ternary(k == v1.ResourceCPU, float64(v.MilliValue())/float64(1000), float64(v.Value())),
			})
		}
	}
	return res
}

func getLimits(provisioner *v1alpha5.Provisioner) v1.ResourceList {
	if provisioner.Spec.Limits != nil {
		return provisioner.Spec.Limits.Resources
	}
	return v1.ResourceList{}
}

func getUsagePercentage(provisioner *v1alpha5.Provisioner) v1.ResourceList {
	usage := v1.ResourceList{}
	for k, v := range getLimits(provisioner) {
		limitValue := v.AsApproximateFloat64()
		usedValue := provisioner.Status.Resources[k]
		if limitValue == 0 {
			usage[k] = *resource.NewQuantity(100, resource.DecimalSI)
		} else {
			usage[k] = *resource.NewQuantity(int64(usedValue.AsApproximateFloat64()/limitValue*100), resource.DecimalSI)
		}
	}
	return usage
}

func makeLabels(provisioner *v1alpha5.Provisioner, resourceTypeName string) prometheus.Labels {
	return prometheus.Labels{
		provisionerResourceType: resourceTypeName,
		provisionerName:         provisioner.Name,
	}
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(
		controllerruntime.
			NewControllerManagedBy(m).
			For(&v1alpha5.Provisioner{}),
	)
}
