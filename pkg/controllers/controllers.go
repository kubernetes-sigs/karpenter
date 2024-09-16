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

package controllers

import (
	"time"

	"github.com/awslabs/operatorpkg/controller"
	"github.com/awslabs/operatorpkg/status"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/patrickmn/go-cache"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption/orchestration"
	metricsnode "sigs.k8s.io/karpenter/pkg/controllers/metrics/node"
	metricsnodepool "sigs.k8s.io/karpenter/pkg/controllers/metrics/nodepool"
	metricspod "sigs.k8s.io/karpenter/pkg/controllers/metrics/pod"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	nodeclaimconsistency "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/consistency"
	nodeclaimdisruption "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/expiration"
	nodeclaimgarbagecollection "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/garbagecollection"
	nodeclaimlifecycle "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	podevents "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/podevents"
	nodeclaimtermination "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/termination"
	nodepoolcounter "sigs.k8s.io/karpenter/pkg/controllers/nodepool/counter"
	nodepoolhash "sigs.k8s.io/karpenter/pkg/controllers/nodepool/hash"
	nodepoolreadiness "sigs.k8s.io/karpenter/pkg/controllers/nodepool/readiness"
	nodepoolvalidation "sigs.k8s.io/karpenter/pkg/controllers/nodepool/validation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/events"
)

func NewControllers(
	mgr manager.Manager,
	clock clock.Clock,
	kubeClient client.Client,
	recorder events.Recorder,
	cloudProvider cloudprovider.CloudProvider,
) []controller.Controller {

	cluster := state.NewCluster(clock, kubeClient)
	sharedCache := cache.New(time.Hour*24, time.Hour)

	p := provisioning.NewProvisioner(kubeClient, recorder, cloudProvider, cluster, sharedCache)
	evictionQueue := terminator.NewQueue(kubeClient, recorder)
	disruptionQueue := orchestration.NewQueue(kubeClient, recorder, cluster, clock, p)

	return []controller.Controller{
		p, evictionQueue, disruptionQueue,
		disruption.NewController(clock, kubeClient, p, cloudProvider, recorder, cluster, disruptionQueue),
		provisioning.NewPodController(kubeClient, p),
		provisioning.NewNodeController(kubeClient, p),
		nodepoolhash.NewController(kubeClient, sharedCache),
		expiration.NewController(clock, kubeClient),
		informer.NewDaemonSetController(kubeClient, cluster),
		informer.NewNodeController(kubeClient, cluster),
		informer.NewPodController(kubeClient, cluster),
		informer.NewNodePoolController(kubeClient, cluster),
		informer.NewNodeClaimController(kubeClient, cluster),
		termination.NewController(clock, kubeClient, cloudProvider, terminator.NewTerminator(clock, kubeClient, evictionQueue, recorder), recorder),
		metricspod.NewController(kubeClient),
		metricsnodepool.NewController(kubeClient),
		metricsnode.NewController(cluster),
		nodepoolreadiness.NewController(kubeClient, cloudProvider),
		nodepoolcounter.NewController(kubeClient, cluster),
		nodepoolvalidation.NewController(kubeClient),
		podevents.NewController(clock, kubeClient),
		nodeclaimconsistency.NewController(clock, kubeClient, recorder),
		nodeclaimlifecycle.NewController(clock, kubeClient, cloudProvider, recorder, sharedCache),
		nodeclaimgarbagecollection.NewController(clock, kubeClient, cloudProvider),
		nodeclaimtermination.NewController(kubeClient, cloudProvider, recorder),
		nodeclaimdisruption.NewController(clock, kubeClient, cloudProvider),
		status.NewController[*v1.NodeClaim](kubeClient, mgr.GetEventRecorderFor("karpenter")),
		status.NewController[*v1.NodePool](kubeClient, mgr.GetEventRecorderFor("karpenter")),
	}
}
