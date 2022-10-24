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

package controllers

import (
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/consolidation"
	"github.com/aws/karpenter-core/pkg/controllers/counter"
	metricspod "github.com/aws/karpenter-core/pkg/controllers/metrics/pod"
	metricsprovisioner "github.com/aws/karpenter-core/pkg/controllers/metrics/provisioner"
	metricsstate "github.com/aws/karpenter-core/pkg/controllers/metrics/state"
	"github.com/aws/karpenter-core/pkg/controllers/node"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/termination"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/operator"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/settingsstore"
)

func init() {
	metrics.MustRegister() // Registers cross-controller metrics
}

func GetControllers(ctx operator.Context, cluster *state.Cluster, settingsStore settingsstore.Store, cloudProvider cloudprovider.CloudProvider) []controller.Controller {
	provisioner := provisioning.NewProvisioner(ctx, ctx.KubeClient, ctx.Clientset.CoreV1(), ctx.EventRecorder, cloudProvider, cluster, settingsStore)

	metricsstate.StartMetricScraper(ctx, cluster)
	return []controller.Controller{
		provisioning.NewController(ctx.KubeClient, provisioner, ctx.EventRecorder),
		state.NewNodeController(ctx.KubeClient, cluster),
		state.NewPodController(ctx.KubeClient, cluster),
		state.NewProvisionerController(ctx.KubeClient, cluster),
		node.NewController(ctx.Clock, ctx.KubeClient, cloudProvider, cluster),
		termination.NewController(ctx, ctx.Clock, ctx.KubeClient, ctx.Clientset.CoreV1(), ctx.EventRecorder, cloudProvider),
		metricspod.NewController(ctx.KubeClient),
		metricsprovisioner.NewController(ctx.KubeClient),
		counter.NewController(ctx.KubeClient, cluster),
		consolidation.NewController(ctx.Clock, ctx.KubeClient, provisioner, cloudProvider, ctx.EventRecorder, cluster),
	}
}
