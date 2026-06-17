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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clock "k8s.io/utils/clock/testing"

	coreapis "sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var cloudProvider *fake.CloudProvider
var fakeClock *clock.FakeClock
var nodeStateController *informer.NodeController
var nodeClaimStateController *informer.NodeClaimController
var recorder *test.EventRecorder

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "DeletionCost")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(coreapis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	// PodDeletionCostManagement defaults to false, but every test in this suite
	// is exercising the controller's enabled-path behavior. The "feature gate
	// disabled" test creates its own context with the gate flipped off.
	opts := test.Options(test.OptionsFields{
		FeatureGates: test.FeatureGates{PodDeletionCostManagement: lo.ToPtr(true)},
	})

	ctx = options.ToContext(ctx, opts)
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeStateController = informer.NewNodeController(env.Client, cluster)
	clusterCost := cost.NewClusterCost(ctx, cloudProvider, env.Client)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cloudProvider, cluster, clusterCost)
	recorder = test.NewEventRecorder()
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	cloudProvider.Reset()
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
	recorder.Reset()
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cluster.Reset()
})

// rsOwnedPod returns a test pod with a synthetic ReplicaSet owner reference so
// the deletion-cost partition logic does not classify it as a non-RS-owned
// pod (Group A). Tests that care about the non-RS-owned classification
// construct pods directly with `test.Pod`.
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
	// Inject the owner reference into the first option block so it merges
	// with any user-supplied ObjectMeta fields.
	opts[0].OwnerReferences = append(opts[0].OwnerReferences, rsOwner)
	return test.Pod(opts...)
}
