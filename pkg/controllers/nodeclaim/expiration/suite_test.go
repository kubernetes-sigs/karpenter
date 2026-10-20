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

package expiration_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/expiration"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var expirationController *expiration.Controller
var env *test.Environment
var cp *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...), test.WithFieldIndexers(test.NodeProviderIDFieldIndexer(ctx)))
	ctx = options.ToContext(ctx, test.Options())
	cp = fake.NewCloudProvider()
	expirationController = expiration.NewController(env.Clock, env.Client, cp)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	ctx = options.ToContext(ctx, test.Options())
	env.Clock.SetTime(time.Now())
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("Expiration", func() {
	// This controller only forcefully expires NodeClaims no disruption budget can pace: those owned by a static
	// NodePool, and standalone ones no NodePool owns. Expiration of a NodeClaim owned by a dynamic NodePool is a
	// voluntary disruption handled by the Expired disruption method, so the fixture NodePool here is static.
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	BeforeEach(func() {
		nodePool = test.StaticNodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{Replicas: lo.ToPtr(int64(1))},
		})
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{},
			Spec: v1.NodeClaimSpec{
				ExpireAfter: v1.MustParseNillableDuration("30s"),
			},
		})
		metrics.NodeClaimsDisruptedTotal.Reset()
		metrics.PodsDisruptionInitiatedTotal.Reset()
	})
	Context("Metrics", func() {
		It("should fire a karpenter_nodeclaims_disrupted_total metric when expired", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			ExpectNotFound(ctx, env.Client, nodeClaim)

			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
				metrics.ReasonLabel:              metrics.ExpiredReason,
				metrics.NodePoolLabel:            "",
				metrics.ConsolidationPolicyLabel: "",
				metrics.TerminationModeLabel:     metrics.TerminationModeGraceful,
			})
		})
		It("should fire a karpenter_nodeclaims_disrupted_total metric when expired", func() {
			nodeClaim.Labels[v1.CapacityTypeLabelKey] = v1.CapacityTypeSpot
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			ExpectNotFound(ctx, env.Client, nodeClaim)
			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
				metrics.ReasonLabel:              metrics.ExpiredReason,
				metrics.NodePoolLabel:            "",
				metrics.ConsolidationPolicyLabel: "",
				metrics.TerminationModeLabel:     metrics.TerminationModeGraceful,
			})
		})
		It("should fire karpenter_pods_disruption_initiated_total by the count of reschedulable pods on the node when expired", func() {
			// Bind 2 reschedulable pods to the node and 1 DaemonSet-owned pod (excluded).
			nodeClaim.Status.NodeName = node.Name
			isController := true
			pod1 := test.Pod(test.PodOptions{NodeName: node.Name})
			pod2 := test.Pod(test.PodOptions{NodeName: node.Name})
			daemonsetPod := test.Pod(test.PodOptions{
				NodeName: node.Name,
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "apps/v1",
						Kind:       "DaemonSet",
						Name:       "ds",
						UID:        "ds-uid",
						Controller: &isController,
					}},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, node, nodeClaim, pod1, pod2, daemonsetPod)

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			ExpectNotFound(ctx, env.Client, nodeClaim)
			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
				metrics.ReasonLabel:              metrics.ExpiredReason,
				metrics.NodePoolLabel:            "",
				metrics.ConsolidationPolicyLabel: "",
				metrics.TerminationModeLabel:     metrics.TerminationModeGraceful,
			})
			ExpectMetricCounterValue(metrics.PodsDisruptionInitiatedTotal, 2, map[string]string{
				metrics.ReasonLabel:              metrics.ExpiredReason,
				metrics.NodePoolLabel:            "",
				metrics.ConsolidationPolicyLabel: "",
				metrics.TerminationModeLabel:     metrics.TerminationModeGraceful,
			})
		})
	})
	DescribeTable(
		"Expiration",
		func(isNodeClaimManaged bool) {
			nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("30s")
			if !isNodeClaimManaged {
				nodeClaim.Spec.NodeClassRef = &v1.NodeClassReference{
					Group: "karpenter.test.sh",
					Kind:  "UnmanagedNodeClass",
					Name:  "default",
				}
			}
			ExpectApplied(ctx, env.Client, nodeClaim)

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
			if isNodeClaimManaged {
				// with forceful termination, when we see a nodeclaim meets the conditions for expiration
				// we should remove it
				ExpectNotFound(ctx, env.Client, nodeClaim)
			} else {
				ExpectExists(ctx, env.Client, nodeClaim)
			}
		},
		Entry("should remove nodeclaims that are expired", true),
		Entry("should ignore expired NodeClaims that are not managed by this Karpenter instance", false),
	)

	It("should not remove the NodeClaims when expiration is disabled", func() {
		nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("Never")
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
	})
	It("should not remove non-expired NodeClaims", func() {
		nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("200s")
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
	})
	It("should delete NodeClaims if the nodeClaim is expired but the node isn't", func() {
		nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("30s")
		ExpectApplied(ctx, env.Client, nodeClaim)

		// step forward to make the node expired
		env.Clock.Step(60 * time.Second)
		ExpectApplied(ctx, env.Client, node) // node shouldn't be expired, but nodeClaim will be
		ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

		ExpectNotFound(ctx, env.Client, nodeClaim)
	})
	It("should return the requeue interval for the time between now and when the nodeClaim expires", func() {
		nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("200s")
		ExpectApplied(ctx, env.Client, nodeClaim, node)

		env.Clock.SetTime(nodeClaim.CreationTimestamp.Add(time.Second * 100))

		result := ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
		Expect(result.RequeueAfter).To(BeNumerically("~", time.Second*100, time.Second))
	})
	It("shouldn't expire the same NodeClaim multiple times", func() {
		nodeClaim.Finalizers = append(nodeClaim.Finalizers, "test-finalizer")
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		// step forward to make the node expired
		env.Clock.Step(60 * time.Second)
		ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
		ExpectExists(ctx, env.Client, nodeClaim)
		ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
			metrics.ReasonLabel:              metrics.ExpiredReason,
			metrics.NodePoolLabel:            nodePool.Name,
			metrics.ConsolidationPolicyLabel: "",
			metrics.TerminationModeLabel:     metrics.TerminationModeGraceful,
		})
		ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
		ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
			metrics.ReasonLabel:              metrics.ExpiredReason,
			metrics.NodePoolLabel:            nodePool.Name,
			metrics.ConsolidationPolicyLabel: "",
			metrics.TerminationModeLabel:     metrics.TerminationModeGraceful,
		})
	})
	Context("Deferring to budget-paced expiration", func() {
		It("shouldn't forcefully expire an expired NodeClaim owned by a dynamic NodePool", func() {
			dynamicNodePool := test.NodePool()
			nodeClaim.Labels[v1.NodePoolLabelKey] = dynamicNodePool.Name
			ExpectApplied(ctx, env.Client, dynamicNodePool, nodeClaim)

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			// The Expired disruption method owns this NodeClaim so that the NodePool's budgets pace it. Deleting it
			// here would race that and force the node out at its deadline regardless of the budget.
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("shouldn't forcefully expire an expired NodeClaim owned by a static NodePool", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			nodeClaim.Labels[v1.NodePoolLabelKey] = nodePool.Name
			ExpectApplied(ctx, env.Client, nodeClaim)

			env.Clock.Step(60 * time.Second)
			result := ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
			Expect(result.RequeueAfter).To(Equal(time.Minute))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("should forcefully expire an expired NodeClaim after its dynamic NodePool is deleted", func() {
			dynamicNodePool := test.NodePool()
			nodeClaim.Labels[v1.NodePoolLabelKey] = dynamicNodePool.Name
			ExpectApplied(ctx, env.Client, dynamicNodePool, nodeClaim)

			// The first reconciliation defers to the budget-paced expiration method.
			env.Clock.Step(60 * time.Second)
			result := ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
			Expect(result.RequeueAfter).To(Equal(time.Minute))
			ExpectExists(ctx, env.Client, nodeClaim)

			// Once the owner disappears, the next bounded retry must take the forceful cleanup path.
			Expect(env.Client.Delete(ctx, dynamicNodePool)).To(Succeed())
			env.Clock.Step(time.Minute)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
			ExpectNotFound(ctx, env.Client, nodeClaim)
		})
		It("should continue budget-paced expiration when a NodePool is recreated with the same name", func() {
			dynamicNodePool := test.NodePool()
			nodeClaim.Labels[v1.NodePoolLabelKey] = dynamicNodePool.Name
			ExpectApplied(ctx, env.Client, dynamicNodePool, nodeClaim)

			env.Clock.Step(60 * time.Second)
			result := ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			Expect(env.Client.Delete(ctx, dynamicNodePool)).To(Succeed())
			ExpectNotFound(ctx, env.Client, dynamicNodePool)
			recreatedNodePool := test.NodePool()
			recreatedNodePool.Name = dynamicNodePool.Name
			ExpectApplied(ctx, env.Client, recreatedNodePool)

			env.Clock.Step(time.Minute)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("should forcefully expire an expired standalone NodeClaim that no NodePool owns", func() {
			delete(nodeClaim.Labels, v1.NodePoolLabelKey)
			ExpectApplied(ctx, env.Client, nodeClaim)

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			// No NodePool means no budget to pace against, so this controller remains responsible.
			ExpectNotFound(ctx, env.Client, nodeClaim)
		})
		It("should forcefully expire an expired NodeClaim whose owning NodePool is gone", func() {
			nodeClaim.Labels[v1.NodePoolLabelKey] = nodePool.Name
			// nodePool is intentionally not applied, so the NodePool label dangles.
			ExpectApplied(ctx, env.Client, nodeClaim)

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			ExpectNotFound(ctx, env.Client, nodeClaim)
		})
	})
})
