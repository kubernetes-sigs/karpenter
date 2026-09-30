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

package deletioncost_test

import (
	"context"
	"math"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	coreapis "sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

type nodeRankInfo struct {
	rank    int
	cleanup bool
	found   bool
}

// rankInfoFor folds the three RankNodes slices into a per-node lookup: RankForBC
// for B/C, math.MinInt32 for A, cleanup=true for D.
func rankInfoFor(name string, groupA, groupBC, groupD []*state.StateNode) nodeRankInfo {
	for _, n := range groupA {
		if n.Node != nil && n.Node.Name == name {
			return nodeRankInfo{rank: math.MinInt32, cleanup: false, found: true}
		}
	}
	for i, n := range groupBC {
		if n.Node != nil && n.Node.Name == name {
			return nodeRankInfo{rank: deletioncost.RankForBC(i, len(groupBC)), cleanup: false, found: true}
		}
	}
	for _, n := range groupD {
		if n.Node != nil && n.Node.Name == name {
			return nodeRankInfo{cleanup: true, found: true}
		}
	}
	return nodeRankInfo{}
}

func totalRanked(groupA, groupBC, groupD []*state.StateNode) int {
	return len(groupA) + len(groupBC) + len(groupD)
}

var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var cloudProvider *fake.CloudProvider
var nodeStateController *informer.NodeController
var nodeClaimStateController *informer.NodeClaimController
var queue *deletioncost.Queue

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "DeletionCost")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(coreapis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	// Every spec here exercises the enabled path; the gate-disabled spec builds its own ctx.
	opts := test.Options(test.OptionsFields{
		FeatureGates: test.FeatureGates{PodDeletionCostManagement: lo.ToPtr(true)},
	})

	ctx = options.ToContext(ctx, opts)
	cloudProvider = fake.NewCloudProvider()
	cluster = state.NewCluster(env.Clock, env.Client, cloudProvider)
	nodeStateController = informer.NewNodeController(env.Client, cluster)
	clusterCost := cost.NewClusterCost(ctx, cloudProvider, env.Client)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cloudProvider, cluster, clusterCost)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	cloudProvider.Reset()
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
	queue = deletioncost.NewQueue(env.Client)
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cluster.Reset()
})

func rsOwnedPod(opts ...test.PodOptions) *corev1.Pod {
	rsOwner := metav1.OwnerReference{
		APIVersion:         "apps/v1",
		Kind:               "ReplicaSet",
		Name:               "test-rs",
		UID:                types.UID("test-rs-uid"),
		Controller:         lo.ToPtr(true),
		BlockOwnerDeletion: lo.ToPtr(true),
	}
	if len(opts) == 0 {
		opts = []test.PodOptions{{}}
	}
	opts[0].OwnerReferences = append(opts[0].OwnerReferences, rsOwner)
	return test.Pod(opts...)
}
