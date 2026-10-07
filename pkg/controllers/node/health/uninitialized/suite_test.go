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

package uninitialized

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx                 context.Context
	env                 *test.Environment
	cloudProvider       *fake.CloudProvider
	recorder            *test.EventRecorder
	cluster             *state.Cluster
	nodeController      *informer.NodeController
	nodeClaimController *informer.NodeClaimController
	clusterCost         *cost.ClusterCost
	controller          *Controller
)

func TestUninitialized(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Node Health Uninitialized")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(
		test.WithCRDs(apis.CRDs...),
		test.WithCRDs(v1alpha1.CRDs...),
		test.WithFieldIndexers(test.NodeClaimProviderIDFieldIndexer(ctx), test.NodeProviderIDFieldIndexer(ctx)),
	)
	ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true)}}))
	cloudProvider = fake.NewCloudProvider()
	recorder = test.NewEventRecorder()
	clusterCost = cost.NewClusterCost(ctx, cloudProvider, env.Client)
})

// useRepairPolicies installs the provider's repair policies and rebuilds cluster state and the controller around one
// matcher compiled from them, as the operator does once at startup. Specs must call it before any Node reaches cluster
// state, since matches are computed as Nodes enter it.
func useRepairPolicies(policies []cloudprovider.RepairPolicy) {
	cloudProvider.RepairPolicy = policies
	repairPolicyMatcher := lo.Must(health.NewRepairPolicyMatcher(ctx, cloudProvider))
	cluster = state.NewCluster(env.Clock, env.Client, cloudProvider, state.WithRepairPolicyMatcher(repairPolicyMatcher))
	nodeController = informer.NewNodeController(env.Client, cluster)
	nodeClaimController = informer.NewNodeClaimController(env.Client, cloudProvider, cluster, clusterCost)
	controller = NewController(env.Clock, env.Client, cluster, recorder)
}

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})
