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
	"context"

	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/settingsstore"
)

func init() {
	metrics.MustRegister() // Registers cross-controller metrics
}

func NewControllers(
	ctx context.Context,
	clock clock.Clock,
	kubeClient client.Client,
	kubernetesInterface kubernetes.Interface,
	cluster *state.Cluster,
	eventRecorder events.Recorder,
	settingsStore settingsstore.Store,
	cloudProvider cloudprovider.CloudProvider,
) []controller.Controller {
	provisioner := provisioning.NewProvisioner(ctx, kubeClient, kubernetesInterface.CoreV1(), eventRecorder, cloudProvider, cluster, settingsStore)

	metricsstate.StartMetricScraper(ctx, cluster)

	return []controller.Controller{
		provisioning.NewController(kubeClient, provisioner, eventRecorder),
		state.NewNodeController(kubeClient, cluster),
		state.NewPodController(kubeClient, cluster),
		state.NewProvisionerController(kubeClient, cluster),
		node.NewController(clock, kubeClient, cloudProvider, cluster),
		termination.NewController(ctx, clock, kubeClient, kubernetesInterface.CoreV1(), eventRecorder, cloudProvider),
		metricspod.NewController(kubeClient),
		metricsprovisioner.NewController(kubeClient),
		counter.NewController(kubeClient, cluster),
		consolidation.NewController(clock, kubeClient, provisioner, cloudProvider, eventRecorder, cluster),
	}
}
