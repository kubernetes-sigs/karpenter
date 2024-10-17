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

package notready_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/notready"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var notReadyController *notready.Controller
var env *test.Environment
var fakeClock *clock.FakeClock

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NotReady Controller Suite")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...), test.WithFieldIndexers(func(c cache.Cache) error {
		return c.IndexField(ctx, &corev1.Node{}, "spec.providerID", func(obj client.Object) []string {
			return []string{obj.(*corev1.Node).Spec.ProviderID}
		})
	}))
	ctx = options.ToContext(ctx, test.Options())
	notReadyController = notready.NewController(env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	ctx = options.ToContext(ctx, test.Options())
	fakeClock.SetTime(time.Now())
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("NotReady", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name},
			},
			Spec: v1.NodeClaimSpec{
				UnreachableTimeout: v1.MustParseNillableDuration("10m"),
			},
		})
		metrics.NodeClaimsDisruptedTotal.Reset()
	})
	It("should remove NodeClaim when the node has an unreachable taint for over the UnreachableTimeout duration", func() {
		node.Spec.Taints = []corev1.Taint{
			{
				Key:       corev1.TaintNodeUnreachable,
				Effect:    corev1.TaintEffectNoSchedule,
				TimeAdded: &metav1.Time{Time: fakeClock.Now().Add(-12 * time.Minute)},
			},
		}
		ExpectApplied(ctx, env.Client, nodeClaim, node)
		ExpectObjectReconciled(ctx, env.Client, notReadyController, nodeClaim)
		ExpectNotFound(ctx, env.Client, nodeClaim)
	})
	It("should not remove NodeClaim if unreachable taint is less than the UnreachableTimeout duration", func() {
		node.Spec.Taints = []corev1.Taint{
			{
				Key:       corev1.TaintNodeUnreachable,
				Effect:    corev1.TaintEffectNoSchedule,
				TimeAdded: &metav1.Time{Time: fakeClock.Now().Add(-7 * time.Minute)},
			},
		}
		ExpectApplied(ctx, env.Client, nodeClaim, node)
		ExpectObjectReconciled(ctx, env.Client, notReadyController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
	})
})
