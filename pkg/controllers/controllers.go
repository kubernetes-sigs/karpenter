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
	"github.com/aws/karpenter-core/pkg/controllers/consistency"
	"github.com/aws/karpenter-core/pkg/controllers/counter"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning"
	"github.com/aws/karpenter-core/pkg/controllers/leasegarbagecollection"
	nodeclaimdisruption "github.com/aws/karpenter-core/pkg/controllers/machine/disruption"
	nodeclaimgarbagecollection "github.com/aws/karpenter-core/pkg/controllers/machine/garbagecollection"
	nodeclaimlifecycle "github.com/aws/karpenter-core/pkg/controllers/machine/lifecycle"
	nodeclaimtermination "github.com/aws/karpenter-core/pkg/controllers/machine/termination"
	metricspod "github.com/aws/karpenter-core/pkg/controllers/metrics/pod"
	metricsprovisioner "github.com/aws/karpenter-core/pkg/controllers/metrics/provisioner"
	metricsstate "github.com/aws/karpenter-core/pkg/controllers/metrics/state"
	coreprovisioner "github.com/aws/karpenter-core/pkg/controllers/provisioner"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/controllers/termination"
	"github.com/aws/karpenter-core/pkg/controllers/termination/terminator"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
)

func NewControllers(
	ctx context.Context,
	clock clock.Clock,
	kubeClient client.Client,
	kubernetesInterface kubernetes.Interface,
	cluster *state.Cluster,
	recorder events.Recorder,
	cloudProvider cloudprovider.CloudProvider,
) []controller.Controller {

	provisioner := provisioning.NewProvisioner(kubeClient, kubernetesInterface.CoreV1(), recorder, cloudProvider, cluster)
	terminator := terminator.NewTerminator(clock, kubeClient, terminator.NewEvictionQueue(ctx, kubernetesInterface.CoreV1(), recorder))

	return []controller.Controller{
		provisioner,
		metricsstate.NewController(cluster),
		deprovisioning.NewController(clock, kubeClient, provisioner, cloudProvider, recorder, cluster),
		provisioning.NewController(kubeClient, provisioner, recorder),
		coreprovisioner.NewController(kubeClient),
		informer.NewDaemonSetController(kubeClient, cluster),
		informer.NewNodeController(kubeClient, cluster),
		informer.NewPodController(kubeClient, cluster),
		informer.NewProvisionerController(kubeClient, cluster),
		informer.NewMachineController(kubeClient, cluster),
		termination.NewController(kubeClient, cloudProvider, terminator, recorder),
		metricspod.NewController(kubeClient),
		metricsprovisioner.NewController(kubeClient),
		counter.NewProvisionerController(kubeClient, cluster),
		consistency.NewMachineController(clock, kubeClient, recorder, cloudProvider),
		nodeclaimlifecycle.NewMachineController(clock, kubeClient, cloudProvider, recorder),
		nodeclaimgarbagecollection.NewController(clock, kubeClient, cloudProvider),
		nodeclaimtermination.NewMachineController(kubeClient, cloudProvider),
		nodeclaimdisruption.NewMachineController(clock, kubeClient, cluster, cloudProvider),
		leasegarbagecollection.NewController(kubeClient),
	}
}
