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
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption/orchestration"
	"sigs.k8s.io/karpenter/pkg/controllers/leasegarbagecollection"
	metricsnode "sigs.k8s.io/karpenter/pkg/controllers/metrics/node"
	metricsnodepool "sigs.k8s.io/karpenter/pkg/controllers/metrics/nodepool"
	metricspod "sigs.k8s.io/karpenter/pkg/controllers/metrics/pod"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	nodeclaimconsistency "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/consistency"
	nodeclaimdisruption "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/disruption"
	nodeclaimgarbagecollection "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/garbagecollection"
	nodeclaimlifecycle "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	nodeclaimtermination "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/termination"
	nodepoolcounter "sigs.k8s.io/karpenter/pkg/controllers/nodepool/counter"
	nodepoolhash "sigs.k8s.io/karpenter/pkg/controllers/nodepool/hash"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/controller"
)

func NewControllers(
	clock clock.Clock,
	kubeClient client.Client,
	kubernetesInterface kubernetes.Interface,
	cluster *state.Cluster,
	recorder events.Recorder,
	cloudProvider cloudprovider.CloudProvider,
) []controller.Controller {

	p := provisioning.NewProvisioner(kubeClient, kubernetesInterface.CoreV1(), recorder, cloudProvider, cluster)
	evictionQueue := terminator.NewQueue(kubernetesInterface.CoreV1(), recorder)
	disruptionQueue := orchestration.NewQueue(kubeClient, recorder, cluster, clock, p)

	return []controller.Controller{
		p, evictionQueue, disruptionQueue,
		disruption.NewController(clock, kubeClient, p, cloudProvider, recorder, cluster, disruptionQueue),
		provisioning.NewController(kubeClient, p, recorder),
		nodepoolhash.NewController(kubeClient),
		informer.NewDaemonSetController(kubeClient, cluster),
		informer.NewNodeController(kubeClient, cluster),
		informer.NewPodController(kubeClient, cluster),
		informer.NewNodePoolController(kubeClient, cluster),
		informer.NewNodeClaimController(kubeClient, cluster),
		termination.NewController(kubeClient, cloudProvider, terminator.NewTerminator(clock, kubeClient, evictionQueue), recorder),
		metricspod.NewController(kubeClient),
		metricsnodepool.NewController(kubeClient),
		metricsnode.NewController(cluster),
		nodepoolcounter.NewController(kubeClient, cluster),
		nodeclaimconsistency.NewController(clock, kubeClient, recorder, cloudProvider),
		nodeclaimlifecycle.NewController(clock, kubeClient, cloudProvider, recorder),
		nodeclaimgarbagecollection.NewController(clock, kubeClient, cloudProvider),
		nodeclaimtermination.NewController(kubeClient, cloudProvider),
		nodeclaimdisruption.NewController(clock, kubeClient, cluster, cloudProvider),
		leasegarbagecollection.NewController(kubeClient),
	}
}
