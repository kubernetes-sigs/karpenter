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
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/logging"

	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
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
	kubeClient client.Client

	// labelCollection keeps track of gauges for gauge deletion
	// provisionerKey (types.NamespacedName) -> []prometheus.Labels
	labelCollection sync.Map
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client) corecontroller.Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

func (c *Controller) Name() string {
	return "provisionermetrics"
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(c.Name()).With("provisioner", req.Name))

	// Remove the previous gauge on CREATE/UPDATE/DELETE
	c.cleanup(req.NamespacedName)

	// Retrieve provisioner from reconcile request
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, provisioner); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	c.record(ctx, provisioner)
	// periodically update our metrics per provisioner even if nothing has changed
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (c *Controller) cleanup(provisionerKey types.NamespacedName) {
	if labelSet, ok := c.labelCollection.Load(provisionerKey); ok {
		for _, labels := range labelSet.([]prometheus.Labels) {
			limitGaugeVec.Delete(labels)
			usageGaugeVec.Delete(labels)
			usagePctGaugeVec.Delete(labels)
		}
	}
}

func (c *Controller) record(ctx context.Context, provisioner *v1alpha5.Provisioner) {
	if err := c.set(provisioner, provisioner.Status.Resources, usageGaugeVec); err != nil {
		logging.FromContext(ctx).Errorf("generating gauge: %s", err)
	}
	if provisioner.Spec.Limits == nil {
		// can't generate our limits or usagePct gauges if there are no limits
		return
	}
	if err := c.set(provisioner, provisioner.Spec.Limits.Resources, limitGaugeVec); err != nil {
		logging.FromContext(ctx).Errorf("generating gauge: %s", err)
	}
	usage := v1.ResourceList{}
	for k, v := range provisioner.Spec.Limits.Resources {
		limitValue := v.AsApproximateFloat64()
		usedValue := provisioner.Status.Resources[k]
		if limitValue == 0 {
			usage[k] = *resource.NewQuantity(100, resource.DecimalSI)
		} else {
			usage[k] = *resource.NewQuantity(int64(usedValue.AsApproximateFloat64()/limitValue*100), resource.DecimalSI)
		}
	}
	if err := c.set(provisioner, usage, usagePctGaugeVec); err != nil {
		logging.FromContext(ctx).Errorf("generating gauge: %s", err)
	}
}

// set updates the value for the node gauge for each resource type passed through the resource list
func (c *Controller) set(provisioner *v1alpha5.Provisioner, resourceList v1.ResourceList, gaugeVec *prometheus.GaugeVec) error {
	provisionerKey := client.ObjectKeyFromObject(provisioner)

	for resourceName, quantity := range resourceList {
		resourceTypeName := strings.ReplaceAll(strings.ToLower(string(resourceName)), "-", "_")

		labels := c.makeLabels(provisioner, resourceTypeName)
		existingLabels, _ := c.labelCollection.LoadOrStore(provisionerKey, []prometheus.Labels{})
		c.labelCollection.Store(provisionerKey, append(existingLabels.([]prometheus.Labels), labels))

		// gets existing gauge or gets a new one if it doesn't exist
		gauge, err := gaugeVec.GetMetricWith(labels)
		if err != nil {
			return fmt.Errorf("creating or getting gauge: %w", err)
		}
		if resourceName == v1.ResourceCPU {
			gauge.Set(float64(quantity.MilliValue()) / float64(1000))
		} else {
			gauge.Set(float64(quantity.Value()))
		}
	}
	return nil
}

func (c *Controller) makeLabels(provisioner *v1alpha5.Provisioner, resourceTypeName string) prometheus.Labels {
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
