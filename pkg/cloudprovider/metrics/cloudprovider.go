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

package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/operator/injection"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/metrics"
)

const (
	metricLabelController = "controller"
	metricLabelMethod     = "method"
	metricLabelProvider   = "provider"
)

// decorator implements CloudProvider
var _ cloudprovider.CloudProvider = (*decorator)(nil)

var methodDurationHistogramVec = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Subsystem: "cloudprovider",
		Name:      "duration_seconds",
		Help:      "Duration of cloud provider method calls. Labeled by the controller, method name and provider.",
	},
	[]string{
		metricLabelController,
		metricLabelMethod,
		metricLabelProvider,
	},
)

func init() {
	crmetrics.Registry.MustRegister(methodDurationHistogramVec)
}

type decorator struct {
	cloudprovider.CloudProvider
}

// Decorate returns a new `CloudProvider` instance that will delegate all method
// calls to the argument, `cloudProvider`, and publish aggregated latency metrics. The
// value used for the metric label, "controller", is taken from the `Context` object
// passed to the methods of `CloudProvider`.
//
// Do not decorate a `CloudProvider` multiple times or published metrics will contain
// duplicated method call counts and latencies.
func Decorate(cloudProvider cloudprovider.CloudProvider) cloudprovider.CloudProvider {
	return &decorator{cloudProvider}
}

func (d *decorator) Create(ctx context.Context, machine *v1alpha5.Machine) (*v1alpha5.Machine, error) {
	defer metrics.Measure(methodDurationHistogramVec.WithLabelValues(injection.GetControllerName(ctx), "Create", d.Name()))()
	return d.CloudProvider.Create(ctx, machine)
}

func (d *decorator) Delete(ctx context.Context, machine *v1alpha5.Machine) error {
	defer metrics.Measure(methodDurationHistogramVec.WithLabelValues(injection.GetControllerName(ctx), "Delete", d.Name()))()
	return d.CloudProvider.Delete(ctx, machine)
}

func (d *decorator) Get(ctx context.Context, id string) (*v1alpha5.Machine, error) {
	defer metrics.Measure(methodDurationHistogramVec.WithLabelValues(injection.GetControllerName(ctx), "Get", d.Name()))()
	return d.CloudProvider.Get(ctx, id)
}

func (d *decorator) List(ctx context.Context) ([]*v1alpha5.Machine, error) {
	defer metrics.Measure(methodDurationHistogramVec.WithLabelValues(injection.GetControllerName(ctx), "List", d.Name()))()
	return d.CloudProvider.List(ctx)
}

func (d *decorator) GetInstanceTypes(ctx context.Context, provisioner *v1alpha5.Provisioner) ([]*cloudprovider.InstanceType, error) {
	defer metrics.Measure(methodDurationHistogramVec.WithLabelValues(injection.GetControllerName(ctx), "GetInstanceTypes", d.Name()))()
	return d.CloudProvider.GetInstanceTypes(ctx, provisioner)
}

func (d *decorator) IsMachineDrifted(ctx context.Context, machine *v1alpha5.Machine) (bool, error) {
	defer metrics.Measure(methodDurationHistogramVec.WithLabelValues(injection.GetControllerName(ctx), "IsMachineDrifted", d.Name()))()
	return d.CloudProvider.IsMachineDrifted(ctx, machine)
}
