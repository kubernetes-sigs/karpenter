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

package readytimeout_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/node/readytimeout"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	karpentertesting "sigs.k8s.io/karpenter/pkg/utils/testing"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var ctx context.Context
var env *test.Environment
var fakeClock *clock.FakeClock
var readyTimeoutController *readytimeout.Controller
var cloudProvider *fake.CloudProvider
var recorder events.Recorder

func TestReadyTimeout(t *testing.T) {
	ctx = karpentertesting.TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "ReadyTimeout")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...))
	ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
		FeatureGates: test.FeatureGates{
			NodeReadyTimeoutRecovery: lo.ToPtr(true),
		},
		NodeReadyTimeout: lo.ToPtr(10 * time.Minute),
	}))
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	recorder = events.NewRecorder(&record.FakeRecorder{})
	readyTimeoutController = readytimeout.NewController(env.Client, cloudProvider, fakeClock, recorder)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
		FeatureGates: test.FeatureGates{
			NodeReadyTimeoutRecovery: lo.ToPtr(true),
		},
		NodeReadyTimeout: lo.ToPtr(10 * time.Minute),
	}))
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("ReadyTimeout", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node

	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim = test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		node = test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
			ReadyStatus: corev1.ConditionFalse,
			ProviderID:  nodeClaim.Status.ProviderID,
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
	})

	Context("Feature Gate", func() {
		It("should not delete nodes when feature gate is disabled", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
				FeatureGates: test.FeatureGates{
					NodeReadyTimeoutRecovery: lo.ToPtr(false),
				},
				NodeReadyTimeout: lo.ToPtr(10 * time.Minute),
			}))
			
			fakeClock.Step(15 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, readyTimeoutController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())
		})

		It("should delete nodes when feature gate is enabled and timeout exceeded", func() {
			fakeClock.Step(15 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, readyTimeoutController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).ToNot(BeNil())
		})
	})

	Context("Timeout Logic", func() {
		It("should not delete nodes before timeout", func() {
			fakeClock.Step(5 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, readyTimeoutController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())
		})

		It("should delete nodes after timeout", func() {
			fakeClock.Step(15 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, readyTimeoutController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).ToNot(BeNil())
		})

		It("should not delete ready nodes", func() {
			node.Status.Conditions = []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			}
			ExpectApplied(ctx, env.Client, node)

			fakeClock.Step(15 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, readyTimeoutController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())
		})
	})

	Context("Metrics", func() {
		It("should fire a metric when node is recovered due to ready timeout", func() {
			fakeClock.Step(15 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, readyTimeoutController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).ToNot(BeNil())

			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
				metrics.ReasonLabel:       metrics.ReadyTimeoutReason,
				metrics.NodePoolLabel:     nodePool.Name,
				metrics.CapacityTypeLabel: nodeClaim.Labels[v1.CapacityTypeLabelKey],
			})
			ExpectMetricCounterValue(readytimeout.NodeReadyTimeoutRecoveredTotal, 1, map[string]string{
				metrics.NodePoolLabel:     nodePool.Name,
				metrics.CapacityTypeLabel: nodeClaim.Labels[v1.CapacityTypeLabelKey],
			})
		})
	})
}) 