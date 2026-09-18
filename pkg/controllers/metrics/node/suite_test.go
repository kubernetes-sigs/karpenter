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

package node_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/metrics/node"
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
var env *test.Environment
var cluster *state.Cluster
var nodeController *informer.NodeController
var nodeClaimController *informer.NodeClaimController
var metricsStateController *node.Controller
var cloudProvider *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodeMetrics")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))

	ctx = options.ToContext(ctx, test.Options())
	cloudProvider = fake.NewCloudProvider()
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
	cluster = state.NewCluster(env.Clock, env.Client, cloudProvider)
	clusterCost := cost.NewClusterCost(ctx, cloudProvider, env.Client)
	nodeController = informer.NewNodeController(env.Client, cluster)
	nodeClaimController = informer.NewNodeClaimController(env.Client, cloudProvider, cluster, clusterCost)
	metricsStateController = node.NewController(env.Clock, cluster)
})

var _ = AfterSuite(func() {
	ExpectCleanedUp(ctx, env.Client)
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Node Metrics", func() {
	var node *corev1.Node
	var resources corev1.ResourceList

	BeforeEach(func() {
		env.Clock.SetTime(time.Now())
		resources = corev1.ResourceList{
			corev1.ResourcePods:   resource.MustParse("100"),
			corev1.ResourceCPU:    resource.MustParse("5000"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
		}
		node = test.Node(test.NodeOptions{Allocatable: resources})
	})
	It("should update the allocatable metric", func() {
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectSingletonReconciled(ctx, metricsStateController)

		for k, v := range resources {
			// A plain node with no NodeClaim is not managed by Karpenter.
			metric, found := FindMetricWithLabelValues("karpenter_nodes_allocatable", map[string]string{
				"node_name":               node.GetName(),
				metrics.ResourceTypeLabel: k.String(),
				"managed":                 "false",
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetGauge().GetValue()).To(BeNumerically("~", v.AsApproximateFloat64()))
		}
	})
	It("should set the managed label on per-node metrics for Karpenter-managed nodes", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			Status: v1.NodeClaimStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: resources,
			},
		})
		managedNode := test.Node(test.NodeOptions{
			ProviderID:  nodeClaim.Status.ProviderID,
			Allocatable: resources,
		})

		ExpectApplied(ctx, env.Client, managedNode, nodeClaim)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeController, nodeClaimController, []*corev1.Node{managedNode}, []*v1.NodeClaim{nodeClaim})
		ExpectSingletonReconciled(ctx, metricsStateController)

		for k, v := range resources {
			metric, found := FindMetricWithLabelValues("karpenter_nodes_allocatable", map[string]string{
				"node_name":               managedNode.GetName(),
				metrics.ResourceTypeLabel: k.String(),
				"managed":                 "true",
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetGauge().GetValue()).To(BeNumerically("~", v.AsApproximateFloat64()))
		}
	})
	It("should update the node lifetime and cluster utilization metrics", func() {

		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectSingletonReconciled(ctx, metricsStateController)

		metric, found := FindMetricWithLabelValues("karpenter_nodes_current_lifetime_seconds", map[string]string{
			"node_name": node.GetName(),
			"managed":   "false",
		})
		Expect(found).To(BeTrue())
		Expect(metric.GetGauge().GetValue()).To(BeNumerically(">=", 0))

		for resourceName := range resources {
			metric, found := FindMetricWithLabelValues("karpenter_cluster_utilization_percent", map[string]string{
				metrics.ResourceTypeLabel: resourceName.String(),
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetGauge().GetValue()).To(BeNumerically("==", 0))
		}
	})
	Context("Expiry", func() {
		const expireAfter = 30 * time.Minute
		const terminationGracePeriod = 2 * time.Hour
		const expirationMetric = "karpenter_nodes_time_until_expiration_seconds"
		const forcedTerminationMetric = "karpenter_nodes_time_until_forced_termination_seconds"

		var nodeClaim *v1.NodeClaim
		var expiringNode *corev1.Node

		BeforeEach(func() {
			nodeClaim = test.NodeClaim(v1.NodeClaim{
				Spec: v1.NodeClaimSpec{
					ExpireAfter:            v1.MustParseNillableDuration(expireAfter.String()),
					TerminationGracePeriod: &metav1.Duration{Duration: terminationGracePeriod},
				},
				Status: v1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: resources,
				},
			})
			expiringNode = test.Node(test.NodeOptions{
				ProviderID:  nodeClaim.Status.ProviderID,
				Allocatable: resources,
			})
		})

		applyAndUpdateState := func() {
			GinkgoHelper()
			ExpectApplied(ctx, env.Client, expiringNode, nodeClaim)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeController, nodeClaimController, []*corev1.Node{expiringNode}, []*v1.NodeClaim{nodeClaim})
		}
		gaugeValue := func(name string) (float64, bool) {
			GinkgoHelper()
			metric, found := FindMetricWithLabelValues(name, map[string]string{"node_name": expiringNode.GetName()})
			if !found {
				return 0, false
			}
			return metric.GetGauge().GetValue(), true
		}

		It("should count down to the expiration and forced termination deadlines", func() {
			applyAndUpdateState()
			// The NodeClaim creationTimestamp is assigned by the API server, so the countdown is aged by
			// advancing the injected clock rather than by backdating the object.
			env.Clock.Step(10 * time.Minute)
			ExpectSingletonReconciled(ctx, metricsStateController)

			expiration, found := gaugeValue(expirationMetric)
			Expect(found).To(BeTrue())
			Expect(expiration).To(BeNumerically("~", (expireAfter - 10*time.Minute).Seconds(), 5))
			forcedTermination, found := gaugeValue(forcedTerminationMetric)
			Expect(found).To(BeTrue())
			Expect(forcedTermination).To(BeNumerically("~", (expireAfter + terminationGracePeriod - 10*time.Minute).Seconds(), 5))
		})
		It("should report a negative countdown once the expiration deadline has passed", func() {
			applyAndUpdateState()
			env.Clock.Step(45 * time.Minute)
			ExpectSingletonReconciled(ctx, metricsStateController)

			expiration, found := gaugeValue(expirationMetric)
			Expect(found).To(BeTrue())
			Expect(expiration).To(BeNumerically("~", -(15 * time.Minute).Seconds(), 5))
			forcedTermination, found := gaugeValue(forcedTerminationMetric)
			Expect(found).To(BeTrue())
			Expect(forcedTermination).To(BeNumerically("~", (expireAfter + terminationGracePeriod - 45*time.Minute).Seconds(), 5))
		})
		It("should measure forced termination from the deletion timestamp once the NodeClaim is terminating", func() {
			applyAndUpdateState()
			ExpectDeletionTimestampSet(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectSingletonReconciled(ctx, metricsStateController)

			// Deletion happened well before the expiration deadline, so the countdown must not include expireAfter.
			forcedTermination, found := gaugeValue(forcedTerminationMetric)
			Expect(found).To(BeTrue())
			Expect(forcedTermination).To(BeNumerically("~", terminationGracePeriod.Seconds(), 5))
		})
		It("should measure forced termination from the termination timestamp annotation when it is set", func() {
			// Node health brings the deadline forward, so the grace period alone overstates the headroom.
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{
				v1.NodeClaimTerminationTimestampAnnotationKey: env.Clock.Now().Add(5 * time.Minute).Format(time.RFC3339),
			})
			applyAndUpdateState()
			ExpectSingletonReconciled(ctx, metricsStateController)

			forcedTermination, found := gaugeValue(forcedTerminationMetric)
			Expect(found).To(BeTrue())
			Expect(forcedTermination).To(BeNumerically("~", (5 * time.Minute).Seconds(), 5))
		})
		It("should not emit either metric when the NodeClaim has neither expireAfter nor a termination grace period", func() {
			nodeClaim.Spec.ExpireAfter = v1.MustParseNillableDuration("Never")
			nodeClaim.Spec.TerminationGracePeriod = nil
			applyAndUpdateState()
			ExpectSingletonReconciled(ctx, metricsStateController)

			_, found := gaugeValue(expirationMetric)
			Expect(found).To(BeFalse())
			_, found = gaugeValue(forcedTerminationMetric)
			Expect(found).To(BeFalse())
		})
		It("should not emit the forced termination metric when the NodeClaim has no termination grace period", func() {
			nodeClaim.Spec.TerminationGracePeriod = nil
			applyAndUpdateState()
			ExpectSingletonReconciled(ctx, metricsStateController)

			_, found := gaugeValue(expirationMetric)
			Expect(found).To(BeTrue())
			_, found = gaugeValue(forcedTerminationMetric)
			Expect(found).To(BeFalse())
		})
	})
	It("should remove the node metric gauge when the node is deleted", func() {
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectSingletonReconciled(ctx, metricsStateController)

		_, found := FindMetricWithLabelValues("karpenter_nodes_allocatable", map[string]string{
			"node_name": node.GetName(),
		})
		Expect(found).To(BeTrue())

		ExpectDeleted(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectSingletonReconciled(ctx, metricsStateController)

		_, found = FindMetricWithLabelValues("karpenter_nodes_allocatable", map[string]string{
			"node_name": node.GetName(),
		})
		Expect(found).To(BeFalse())
	})
})
