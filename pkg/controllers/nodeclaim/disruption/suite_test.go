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

package disruption_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	nodeclaimdisruption "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	"sigs.k8s.io/karpenter/pkg/test"
)

var ctx context.Context
var nodeClaimDisruptionController *nodeclaimdisruption.Controller
var env *test.Environment
var fakeClock *clock.FakeClock
var cluster *state.Cluster
var cp *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...), test.WithFieldIndexers(func(c cache.Cache) error {
		return c.IndexField(ctx, &v1.Node{}, "spec.providerID", func(obj client.Object) []string {
			return []string{obj.(*v1.Node).Spec.ProviderID}
		})
	}))
	ctx = options.ToContext(ctx, test.Options())
	cp = fake.NewCloudProvider()
	cluster = state.NewCluster(fakeClock, env.Client, cp)
	nodeClaimDisruptionController = nodeclaimdisruption.NewController(fakeClock, env.Client, cluster, cp)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	ctx = options.ToContext(ctx, test.Options())
	fakeClock.SetTime(time.Now())
})

var _ = AfterEach(func() {
	cp.Reset()
	cluster.Reset()
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("Disruption", func() {
	var nodePool *v1beta1.NodePool
	var nodeClaim *v1beta1.NodeClaim
	var node *v1.Node

	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1beta1.NodePoolLabelKey: nodePool.Name},
			},
		})
	})
	It("should set multiple disruption conditions simultaneously", func() {
		cp.Drifted = "drifted"
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenEmpty
		nodePool.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 30)}
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)

		// step forward to make the node empty
		fakeClock.Step(60 * time.Second)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1beta1.ConditionTypeDrifted).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1beta1.ConditionTypeEmpty).IsTrue()).To(BeTrue())
	})
	It("should remove multiple disruption conditions simultaneously", func() {
		nodePool.Spec.Disruption.ExpireAfter.Duration = nil
		nodePool.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{Duration: nil}

		nodeClaim.StatusConditions().SetTrue(v1beta1.ConditionTypeDrifted)
		nodeClaim.StatusConditions().SetTrue(v1beta1.ConditionTypeEmpty)

		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1beta1.ConditionTypeDrifted).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1beta1.ConditionTypeEmpty)).To(BeNil())
	})
})
