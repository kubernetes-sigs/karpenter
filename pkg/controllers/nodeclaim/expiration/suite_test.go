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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/expiration"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var expirationController *expiration.Controller
var env *test.Environment
var cp *fake.CloudProvider
var cluster *state.Cluster
var clusterCost *cost.ClusterCost
var nodeStateController *informer.NodeController
var nodeClaimStateController *informer.NodeClaimController

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...), test.WithFieldIndexers(test.NodeProviderIDFieldIndexer(ctx)))
	ctx = options.ToContext(ctx, test.Options())
	cp = fake.NewCloudProvider()
	cluster = state.NewCluster(env.Clock, env.Client, cp)
	clusterCost = cost.NewClusterCost(ctx, cp, env.Client)
	nodeStateController = informer.NewNodeController(env.Client, cluster)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cp, cluster, clusterCost)
	expirationController = expiration.NewController(env.Clock, env.Client, cp, cluster, test.NewEventRecorder())
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
				ExpireAfter: v1.MustParseNillableDuration("30s"),
			},
		})
		metrics.NodeClaimsDisruptedTotal.Reset()
	})
	Context("Metrics", func() {
		It("should fire a karpenter_nodeclaims_disrupted_total metric when expired", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			ExpectNotFound(ctx, env.Client, nodeClaim)

			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
				metrics.ReasonLabel:   metrics.ExpiredReason,
				metrics.NodePoolLabel: nodePool.Name,
			})
		})
		It("should fire a karpenter_nodeclaims_disrupted_total metric when expired", func() {
			nodeClaim.Labels[v1.CapacityTypeLabelKey] = v1.CapacityTypeSpot
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			ExpectNotFound(ctx, env.Client, nodeClaim)
			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
				metrics.ReasonLabel:   metrics.ExpiredReason,
				metrics.NodePoolLabel: nodePool.Name,
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
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
			if isNodeClaimManaged {
				// when we see a managed nodeclaim that meets the conditions for expiration and the
				// owning NodePool has disruption budget available, we should remove it
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
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		// step forward to make the node expired
		env.Clock.Step(60 * time.Second)
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
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		// step forward to make the node expired
		env.Clock.Step(60 * time.Second)
		ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
		ExpectExists(ctx, env.Client, nodeClaim)
		ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
			metrics.ReasonLabel:   metrics.ExpiredReason,
			metrics.NodePoolLabel: nodePool.Name,
		})
		ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)
		ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
			metrics.ReasonLabel:   metrics.ExpiredReason,
			metrics.NodePoolLabel: nodePool.Name,
		})
	})
	Context("Disruption Budgets", func() {
		It("should not expire a NodeClaim when the NodePool disruption budget disallows it", func() {
			// A budget of "0" leaves no room to disrupt, so the expired NodeClaim must be deferred
			// rather than forcefully deleted (kubernetes-sigs/karpenter#1750).
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0"}}
			nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("30s")
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			result := ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			// The NodeClaim is expired but the budget is exhausted, so it should be requeued, not deleted.
			ExpectExists(ctx, env.Client, nodeClaim)
			Expect(result.RequeueAfter).To(BeNumerically(">", time.Duration(0)))
		})
		It("should expire a NodeClaim once the NodePool disruption budget allows it", func() {
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "1"}}
			nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("30s")
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			ExpectNotFound(ctx, env.Client, nodeClaim)
			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
				metrics.ReasonLabel:   metrics.ExpiredReason,
				metrics.NodePoolLabel: nodePool.Name,
			})
		})
		It("should not be blocked by a budget scoped to a different disruption reason", func() {
			// Expiration is gated by the Drifted reason, so a budget scoped only to Empty must not
			// block it. The expired NodeClaim should still be removed.
			nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0", Reasons: []v1.DisruptionReason{v1.DisruptionReasonEmpty}}}
			nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("30s")
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			// step forward to make the node expired
			env.Clock.Step(60 * time.Second)
			ExpectObjectReconciled(ctx, env.Client, expirationController, nodeClaim)

			ExpectNotFound(ctx, env.Client, nodeClaim)
		})
	})
})
