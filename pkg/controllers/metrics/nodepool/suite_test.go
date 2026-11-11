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

package nodepool_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/metrics/nodepool"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/node/legacyrepair"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/state/nodepoolbackoff"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var nodePoolController *nodepool.Controller
var ctx context.Context
var env *test.Environment
var cp *fake.CloudProvider
var cc *cost.ClusterCost
var backoff *nodepoolbackoff.State

var errInjected = errors.New("injected error")

// listErrorClient fails every List into the same type as failList while failList is set, so a test can fail an update.
type listErrorClient struct {
	client.Client
	failList client.ObjectList
}

func (c *listErrorClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.failList != nil && reflect.TypeOf(list) == reflect.TypeOf(c.failList) {
		return errInjected
	}
	return c.Client.List(ctx, list, opts...)
}

// failingTrippedAt fails to work out when a trip started, so a test can fail an update at that step.
type failingTrippedAt struct {
	nodepool.Breakers
}

func (failingTrippedAt) NodePoolBreakerTrippedAt(context.Context, string) (time.Time, error) {
	return time.Time{}, errInjected
}

// untrippedOnSecondRead finds no trip when asked when one started, as when the nodes change between the two reads.
type untrippedOnSecondRead struct {
	nodepool.Breakers
}

func (untrippedOnSecondRead) NodePoolBreakerTrippedAt(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodePoolMetrics")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	cp = fake.NewCloudProvider()
	cc = cost.NewClusterCost(ctx, cp, env.Client)
	backoff = nodepoolbackoff.NewState(env.Clock)
	nodePoolController = nodepool.NewController(env.Client, cp, cc, backoff)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Metrics", func() {
	var nodePool *v1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Template: v1.NodeClaimTemplate{
					Spec: v1.NodeClaimTemplateSpec{
						NodeClassRef: &v1.NodeClassReference{
							Group: "karpenter.test.sh",
							Kind:  "TestNodeClass",
							Name:  "default",
						},
					},
				},
			},
		})
	})
	DescribeTable(
		"should update the nodepool limit metrics",
		func(isNodePoolManaged bool) {
			limits := v1.Limits{
				corev1.ResourceCPU:              resource.MustParse("10"),
				corev1.ResourceMemory:           resource.MustParse("10Mi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
			}
			nodePool.Spec.Limits = limits
			if !isNodePoolManaged {
				nodePool.Spec.Template.Spec.NodeClassRef = &v1.NodeClassReference{
					Group: "karpenter.test.sh",
					Kind:  "UnmanagedNodeClass",
					Name:  "default",
				}
			}
			ExpectApplied(ctx, env.Client, nodePool)
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

			for k, v := range limits {
				m, found := FindMetricWithLabelValues("karpenter_nodepools_limit", map[string]string{
					metrics.NodePoolLabel:     nodePool.GetName(),
					metrics.ResourceTypeLabel: strings.ReplaceAll(k.String(), "-", "_"),
				})
				Expect(found).To(Equal(isNodePoolManaged))
				if isNodePoolManaged {
					Expect(m.GetGauge().GetValue()).To(BeNumerically("~", v.AsApproximateFloat64()))
				}
			}
		},
		Entry("should update the nodepool limit metrics", true),
		Entry("should ignore nodepools not managed by this instance of Karpenter", false),
	)
	It("should update the nodepool usage metrics", func() {
		resources := corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("10"),
			corev1.ResourceMemory:           resource.MustParse("10Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		nodePool.Status.Resources = resources

		ExpectApplied(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

		for k, v := range resources {
			m, found := FindMetricWithLabelValues("karpenter_nodepools_usage", map[string]string{
				metrics.NodePoolLabel:     nodePool.GetName(),
				metrics.ResourceTypeLabel: strings.ReplaceAll(k.String(), "-", "_"),
			})
			Expect(found).To(BeTrue())
			Expect(m.GetGauge().GetValue()).To(BeNumerically("~", v.AsApproximateFloat64()))
		}
	})
	It("should delete the nodepool state metrics on nodepool delete", func() {
		expectedMetrics := []string{"karpenter_nodepools_limit", "karpenter_nodepools_usage"}
		nodePool.Spec.Limits = v1.Limits{
			corev1.ResourceCPU:              resource.MustParse("100"),
			corev1.ResourceMemory:           resource.MustParse("100Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1000Gi"),
		}
		nodePool.Status.Resources = corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("10"),
			corev1.ResourceMemory:           resource.MustParse("10Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

		for _, name := range expectedMetrics {
			_, found := FindMetricWithLabelValues(name, map[string]string{
				metrics.NodePoolLabel: nodePool.GetName(),
			})
			Expect(found).To(BeTrue())
		}

		ExpectDeleted(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

		for _, name := range expectedMetrics {
			_, found := FindMetricWithLabelValues(name, map[string]string{
				metrics.NodePoolLabel: nodePool.GetName(),
			})
			Expect(found).To(BeFalse())
		}
	})
	Context("drift backoff metric", func() {
		const metricName = "karpenter_nodepools_drift_backoff_seconds"

		It("should publish the remaining backoff and update it over time", func() {
			ExpectApplied(ctx, env.Client, nodePool)
			Expect(backoff.Fail(nodePool, env.Clock.Now())).To(BeTrue())
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

			ExpectMetricGaugeValue(nodepool.DriftBackoffSeconds, backoff.Remaining(nodePool).Seconds(), map[string]string{metrics.NodePoolLabel: nodePool.Name})

			env.Clock.Step(15 * time.Second)
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
			ExpectMetricGaugeValue(nodepool.DriftBackoffSeconds, backoff.Remaining(nodePool).Seconds(), map[string]string{metrics.NodePoolLabel: nodePool.Name})
		})

		It("should delete the metric when backoff is reset", func() {
			ExpectApplied(ctx, env.Client, nodePool)
			Expect(backoff.Fail(nodePool, env.Clock.Now())).To(BeTrue())
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

			backoff.Reset(nodePool.Name)
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
			_, found := FindMetricWithLabelValues(metricName, map[string]string{metrics.NodePoolLabel: nodePool.Name})
			Expect(found).To(BeFalse())
		})

		It("should delete the metric when the backoff window expires", func() {
			ExpectApplied(ctx, env.Client, nodePool)
			Expect(backoff.Fail(nodePool, env.Clock.Now())).To(BeTrue())
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

			_, until, _ := backoff.GetBackoff(nodePool)
			env.Clock.SetTime(until.Add(time.Second))
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
			_, found := FindMetricWithLabelValues(metricName, map[string]string{metrics.NodePoolLabel: nodePool.Name})
			Expect(found).To(BeFalse())
		})

		It("should delete the metric when the NodePool is deleted", func() {
			ExpectApplied(ctx, env.Client, nodePool)
			Expect(backoff.Fail(nodePool, env.Clock.Now())).To(BeTrue())
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

			ExpectDeleted(ctx, env.Client, nodePool)
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
			_, found := FindMetricWithLabelValues(metricName, map[string]string{metrics.NodePoolLabel: nodePool.Name})
			Expect(found).To(BeFalse())
		})
	})
})

var _ = Describe("Repair Breaker Metrics", func() {
	var nodePool *v1.NodePool
	var matcher *health.RepairPolicyMatcher
	var cluster *state.Cluster
	var nodeStateController *informer.NodeController
	var repairController *nodepool.RepairController
	nodePoolGauges := []opmetrics.GaugeMetric{
		nodepool.RepairBreakerTripped,
		nodepool.RepairBreakerTrippedTimeSeconds,
		nodepool.RepairBreakerTrippedNodesWithUnhealthyCondition,
		nodepool.RepairBreakerNodes,
	}
	allGauges := append([]opmetrics.GaugeMetric{nodepool.RepairBreakerLongestTrippedTimeSeconds}, nodePoolGauges...)

	// applyNode applies the node and tells cluster state about it.
	applyNode := func(node *corev1.Node) {
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
	}
	// deleteNodes deletes the nodes and tells cluster state about it.
	deleteNodes := func(nodes ...*corev1.Node) {
		for _, node := range nodes {
			ExpectDeleted(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		}
	}
	// applyNodes applies count nodes in the NodePool. Node i has BadNode=False when i < unhealthy, since start plus i
	// minutes.
	applyNodes := func(np *v1.NodePool, count, unhealthy int, start time.Time) []*corev1.Node {
		return lo.Times(count, func(i int) *corev1.Node {
			// Cluster state leaves out a managed node without a provider ID or an instance type label, so give it both.
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: np.Name, corev1.LabelInstanceTypeStable: "default-instance-type"}},
				ProviderID: test.RandomProviderID(),
			})
			if i < unhealthy {
				node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
					Type:               "BadNode",
					Status:             corev1.ConditionFalse,
					LastTransitionTime: metav1.NewTime(start.Add(time.Duration(i) * time.Minute)),
				})
			}
			applyNode(node)
			return node
		})
	}
	heal := func(node *corev1.Node) {
		node.Status.Conditions = lo.Reject(node.Status.Conditions, func(c corev1.NodeCondition, _ int) bool { return c.Type == "BadNode" })
		applyNode(node)
	}
	// setBadNode gives the node a BadNode condition with the status, changed at the given time.
	setBadNode := func(node *corev1.Node, status corev1.ConditionStatus, at time.Time) {
		node.Status.Conditions = append(lo.Reject(node.Status.Conditions, func(c corev1.NodeCondition, _ int) bool { return c.Type == "BadNode" }),
			corev1.NodeCondition{Type: "BadNode", Status: status, LastTransitionTime: metav1.NewTime(at)})
		applyNode(node)
	}
	resetGauges := func() {
		for _, gauge := range allGauges {
			gauge.Reset()
		}
	}
	// startController makes c the controller under test, clears the series earlier controllers set, and runs its first
	// update. A trip that begins after that is timed from node data rather than from that first update.
	startController := func(c *nodepool.RepairController) {
		GinkgoHelper()
		resetGauges()
		repairController = c
		ExpectSingletonReconciled(ctx, repairController)
	}
	// newLegacyRepair builds the legacy repair controller, which the metrics read the breakers from in legacy mode.
	newLegacyRepair := func(kubeClient client.Client) *legacyrepair.Controller {
		return legacyrepair.NewController(kubeClient, cp, env.Clock, test.NewEventRecorder())
	}
	expectBreaker := func(np *v1.NodePool, tripped float64, trippedFor time.Duration, trippedNodesWithUnhealthyCondition, nodes float64) {
		GinkgoHelper()
		labels := map[string]string{metrics.NodePool.Name: np.Name}
		ExpectMetricGaugeValue(nodepool.RepairBreakerTripped, tripped, labels)
		ExpectMetricGaugeValue(nodepool.RepairBreakerTrippedTimeSeconds, trippedFor.Seconds(), labels)
		ExpectMetricGaugeValue(nodepool.RepairBreakerTrippedNodesWithUnhealthyCondition, trippedNodesWithUnhealthyCondition, labels)
		ExpectMetricGaugeValue(nodepool.RepairBreakerNodes, nodes, labels)
	}
	expectNoBreaker := func(np *v1.NodePool) {
		GinkgoHelper()
		for _, gauge := range nodePoolGauges {
			_, found := FindMetricWithLabelValues(ExpectMetricName(gauge.(*opmetrics.PrometheusGauge)), map[string]string{metrics.NodePool.Name: np.Name})
			Expect(found).To(BeFalse())
		}
	}
	expectLongestTrip := func(trippedFor time.Duration) {
		GinkgoHelper()
		ExpectMetricGaugeValue(nodepool.RepairBreakerLongestTrippedTimeSeconds, trippedFor.Seconds(), map[string]string{})
	}
	expectNoSeries := func() {
		GinkgoHelper()
		for _, gauge := range allGauges {
			// An empty label set matches every series.
			_, found := FindMetricWithLabelValues(ExpectMetricName(gauge.(*opmetrics.PrometheusGauge)), map[string]string{})
			Expect(found).To(BeFalse())
		}
	}

	BeforeEach(func() {
		// Run the clock an hour ahead of the nodes' creation, so their condition times aren't clamped to it. The API server
		// stores condition times to the second, so start on a whole second.
		env.Clock.SetTime(time.Now().Add(time.Hour).Truncate(time.Second))
		// BadNode=Unknown is unhealthy too, so a node stays unhealthy when its condition flips between False and Unknown.
		cp.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionUnknown, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		}
		matcher = lo.Must(health.NewRepairPolicyMatcher(options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: new(true)}})), cp))
		cluster = state.NewCluster(env.Clock, env.Client, cp, state.WithRepairPolicyMatcher(matcher))
		// The controller only reads cluster state's synced flag, so run the sync check here, as disruption would.
		Expect(cluster.Synced(ctx)).To(BeTrue())
		nodeStateController = informer.NewNodeController(env.Client, cluster)
		nodePool = test.NodePool()
		ExpectApplied(ctx, env.Client, nodePool)
		startController(nodepool.NewRepairController(env.Clock, env.Client, cp, cluster))
	})
	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
		resetGauges()
	})

	It("should report every managed nodepool while its breaker isn't tripped", func() {
		empty := test.NodePool()
		ExpectApplied(ctx, env.Client, empty)
		// 2 of 10 unhealthy is at the limit.
		applyNodes(nodePool, 10, 2, env.Clock.Now())

		// The controller runs once and then on its requeue, so the requeue keeps the metrics fresh.
		Expect(ExpectSingletonReconciled(ctx, repairController).RequeueAfter).To(Equal(10 * time.Second))
		expectBreaker(nodePool, 0, 0, 0, 10)
		expectBreaker(empty, 0, 0, 0, 0)
		expectLongestTrip(0)
	})
	It("should report a tripped nodepool, its nodes with an unhealthy condition, and how long it has been tripped", func() {
		start := env.Clock.Now()
		// The third node to become unhealthy, two minutes in, tripped the breaker.
		applyNodes(nodePool, 10, 3, start)
		healthy := test.NodePool()
		ExpectApplied(ctx, env.Client, healthy)
		applyNodes(healthy, 5, 1, start)
		env.Clock.Step(10 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 8*time.Minute, 3, 10)
		expectBreaker(healthy, 0, 0, 0, 5)
		expectLongestTrip(8 * time.Minute)
	})
	It("should count only the nodes in cluster state", func() {
		applyNodes(nodePool, 5, 1, env.Clock.Now())
		unhealthyNode := func(labels map[string]string, providerID string) {
			node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Labels: lo.Assign(map[string]string{v1.NodePoolLabelKey: nodePool.Name}, labels)}, ProviderID: providerID})
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(env.Clock.Now())})
			applyNode(node)
		}
		// Cluster state leaves out a managed node while it has no provider ID, or no instance type label before it's
		// initialized, as while it registers. Counted, these two would trip the breaker.
		unhealthyNode(map[string]string{corev1.LabelInstanceTypeStable: "default-instance-type"}, "")
		unhealthyNode(nil, test.RandomProviderID())
		// An initialized node counts without an instance type label.
		unhealthyNode(map[string]string{v1.NodeInitializedLabelKey: "true"}, test.RandomProviderID())
		env.Clock.Step(time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 0, 0, 0, 6)
	})
	It("should wait for another controller to mark cluster state synced, without running the sync check itself", func() {
		node := applyNodes(nodePool, 1, 0, env.Clock.Now())[0]
		// This cluster state has never synced, as after a restart, but it has every node, so the sync check would pass.
		fresh := state.NewCluster(env.Clock, env.Client, cp, state.WithRepairPolicyMatcher(matcher))
		Expect(fresh.UpdateNode(ctx, ExpectExists(ctx, env.Client, node))).To(Succeed())
		freshController := nodepool.NewRepairController(env.Clock, env.Client, cp, fresh)
		resetGauges()
		Expect(ExpectSingletonReconciled(ctx, freshController).RequeueAfter).To(Equal(time.Second))
		expectNoSeries()
		Expect(fresh.HasSynced()).To(BeFalse())

		// Another controller, such as disruption, runs the sync check and marks cluster state synced.
		Expect(fresh.Synced(ctx)).To(BeTrue())
		Expect(ExpectSingletonReconciled(ctx, freshController).RequeueAfter).To(Equal(10 * time.Second))
		expectBreaker(nodePool, 0, 0, 0, 1)
	})
	It("should keep updating after cluster state's first sync while a NodeClaim launches", func() {
		nodes := applyNodes(nodePool, 10, 3, env.Clock.Now())
		env.Clock.Step(10 * time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 8*time.Minute, 3, 10)

		// A NodeClaim with no provider ID yet fails the sync check, but the synced flag stays set.
		launching := test.NodeClaim()
		launching.Status.ProviderID = ""
		cluster.UpdateNodeClaim(launching)
		Expect(cluster.Synced(ctx)).To(BeFalse())
		Expect(cluster.HasSynced()).To(BeTrue())
		heal(nodes[0])
		Expect(ExpectSingletonReconciled(ctx, repairController).RequeueAfter).To(Equal(10 * time.Second))
		expectBreaker(nodePool, 0, 0, 0, 10)
	})
	It("should keep the trip start when a node's condition flips between two unhealthy statuses", func() {
		start := env.Clock.Now()
		// The third node to become unhealthy, two minutes in, tripped the breaker.
		nodes := applyNodes(nodePool, 10, 3, start)
		env.Clock.Step(10 * time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 8*time.Minute, 3, 10)

		// Each flip resets the condition's transition time, which alone would move the trip start to the flip.
		setBadNode(nodes[2], corev1.ConditionUnknown, env.Clock.Now())
		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 9*time.Minute, 3, 10)

		setBadNode(nodes[2], corev1.ConditionFalse, env.Clock.Now())
		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 10*time.Minute, 3, 10)
		expectLongestTrip(10 * time.Minute)
	})
	It("should time a trip that was under way at a restart from the first update after it", func() {
		start := env.Clock.Now()
		applyNodes(nodePool, 10, 3, start)
		// 6 nodes allow 2 unhealthy, so shrunk isn't tripped until a healthy node goes.
		shrunk := test.NodePool()
		ExpectApplied(ctx, env.Client, shrunk)
		shrunkNodes := applyNodes(shrunk, 6, 2, start)
		env.Clock.SetTime(start.Add(3 * time.Hour))
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(shrunk, 0, 0, 0, 6)
		deleteNodes(shrunkNodes[5])
		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 3*time.Hour-time.Minute, 3, 10)
		expectBreaker(shrunk, 1, time.Minute, 2, 5)

		// A new controller remembers no trip, as after a restart. Node data would date shrunk's trip back 3 hours, since
		// its nodes were unhealthy long before it shrank.
		startController(nodepool.NewRepairController(env.Clock, env.Client, cp, cluster))
		expectBreaker(nodePool, 1, 0, 3, 10)
		expectBreaker(shrunk, 1, 0, 2, 5)
		expectLongestTrip(0)

		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, time.Minute, 3, 10)
		expectBreaker(shrunk, 1, time.Minute, 2, 5)
		expectLongestTrip(time.Minute)
	})
	It("should go back to 0 once the nodepool recovers, and start a new trip fresh", func() {
		nodes := applyNodes(nodePool, 10, 3, env.Clock.Now())
		env.Clock.Step(10 * time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 8*time.Minute, 3, 10)
		expectLongestTrip(8 * time.Minute)

		heal(nodes[0])
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 0, 0, 0, 10)
		expectLongestTrip(0)

		// Another node trips the breaker again, and the new trip doesn't reuse the old start.
		setBadNode(nodes[3], corev1.ConditionFalse, env.Clock.Now())
		env.Clock.Step(5 * time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 5*time.Minute, 3, 10)
		expectLongestTrip(5 * time.Minute)
	})
	It("should forget the trip start of a deleted nodepool", func() {
		applyNodes(nodePool, 10, 3, env.Clock.Now())
		env.Clock.Step(10 * time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 8*time.Minute, 3, 10)

		ExpectDeleted(ctx, env.Client, nodePool)
		ExpectSingletonReconciled(ctx, repairController)
		expectNoBreaker(nodePool)

		// A nodepool with the same name comes back. No update saw it untripped, so its trip starts now, not at the old start
		// or as far back as node data goes.
		ExpectApplied(ctx, env.Client, test.NodePool(v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: nodePool.Name}}))
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 0, 3, 10)
		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, time.Minute, 3, 10)
	})
	It("should report the longest trip across the cluster's nodepools, from the starts it keeps", func() {
		start := env.Clock.Now()
		other := test.NodePool()
		ExpectApplied(ctx, env.Client, other)
		// An update sees other before it has nodes, as when Karpenter launches them, so node data can date its trip.
		ExpectSingletonReconciled(ctx, repairController)
		nodes := applyNodes(nodePool, 10, 3, start)
		// other has 5 nodes, so it trips when its second node becomes unhealthy, seven minutes in.
		otherNodes := applyNodes(other, 5, 2, start.Add(6*time.Minute))
		env.Clock.Step(10 * time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 8*time.Minute, 3, 10)
		expectBreaker(other, 1, 3*time.Minute, 2, 5)
		expectLongestTrip(8 * time.Minute)

		// Without the kept start, nodePool's trip would show 1 minute, and other's 4 minutes would be the longest.
		setBadNode(nodes[2], corev1.ConditionUnknown, env.Clock.Now())
		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 9*time.Minute, 3, 10)
		expectBreaker(other, 1, 4*time.Minute, 2, 5)
		expectLongestTrip(9 * time.Minute)

		// Once the longest trip ends, the cluster reports the next longest.
		heal(nodes[0])
		ExpectSingletonReconciled(ctx, repairController)
		expectLongestTrip(4 * time.Minute)

		heal(otherNodes[0])
		ExpectSingletonReconciled(ctx, repairController)
		expectLongestTrip(0)
	})
	It("should keep the start of the trip it began with when the nodepool shrinks and grows", func() {
		start := env.Clock.Now()
		// 20 nodes allow 4 unhealthy, so the fifth node to become unhealthy, at 55 minutes, tripped the breaker.
		nodes := applyNodes(nodePool, 20, 4, start)
		setBadNode(nodes[4], corev1.ConditionFalse, start.Add(55*time.Minute))
		env.Clock.SetTime(start.Add(time.Hour))
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 5*time.Minute, 5, 20)

		// Deleting healthy nodes lowers the allowance, so node data alone would date the trip to an older node.
		deleteNodes(nodes[15:]...)
		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 6*time.Minute, 5, 15)

		// The nodepool grows back, and the trip still reads from its own start.
		applyNodes(nodePool, 5, 0, start)
		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 7*time.Minute, 5, 20)
		expectLongestTrip(7 * time.Minute)
	})
	It("should start the trip no earlier than the last update that saw the nodepool untripped", func() {
		start := env.Clock.Now()
		// 6 nodes allow 2 unhealthy, so this nodepool isn't tripped.
		nodes := applyNodes(nodePool, 6, 2, start)
		env.Clock.SetTime(start.Add(3 * time.Hour))
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 0, 0, 0, 6)

		// Deleting a healthy node lowers the allowance to 1, so the breaker trips now, on nodes unhealthy for hours.
		deleteNodes(nodes[5])
		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, time.Minute, 2, 5)
		expectLongestTrip(time.Minute)
	})
	It("should keep a trip start no later than now when a node's clock is ahead", func() {
		// The nodes became unhealthy a minute after the controller's now.
		applyNodes(nodePool, 10, 3, env.Clock.Now().Add(time.Minute))

		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 0, 3, 10)
		expectLongestTrip(0)

		// The trip started at the update that first saw it, not when the nodes' clock says.
		env.Clock.Step(10 * time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 10*time.Minute, 3, 10)
		expectLongestTrip(10 * time.Minute)
	})
	It("should keep a trip start no later than now when the clock steps back", func() {
		applyNodes(nodePool, 10, 3, env.Clock.Now())
		env.Clock.Step(10 * time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 8*time.Minute, 3, 10)

		// The clock steps back to before the kept start, so the start moves back to now.
		env.Clock.SetTime(env.Clock.Now().Add(-9 * time.Minute))
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 0, 3, 10)

		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, time.Minute, 3, 10)
	})
	It("should delete the metrics of a deleted nodepool", func() {
		applyNodes(nodePool, 10, 3, env.Clock.Now())
		env.Clock.Step(10 * time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 8*time.Minute, 3, 10)

		ExpectDeleted(ctx, env.Client, nodePool)
		ExpectSingletonReconciled(ctx, repairController)
		expectNoBreaker(nodePool)
		expectLongestTrip(0)
	})
	DescribeTable("should remove every series and return the error when an update fails, keeping the trip start",
		func(legacy bool, failList client.ObjectList) {
			kubeClient := &listErrorClient{Client: env.Client}
			if legacy {
				startController(nodepool.NewLegacyRepairController(env.Clock, kubeClient, cp, newLegacyRepair(kubeClient)))
			} else {
				startController(nodepool.NewRepairController(env.Clock, kubeClient, cp, cluster))
			}
			nodes := applyNodes(nodePool, 10, 3, env.Clock.Now())
			healthy := test.NodePool()
			ExpectApplied(ctx, env.Client, healthy)
			applyNodes(healthy, 5, 0, env.Clock.Now())
			env.Clock.Step(10 * time.Minute)
			ExpectSingletonReconciled(ctx, repairController)
			expectBreaker(nodePool, 1, 8*time.Minute, 3, 10)
			expectBreaker(healthy, 0, 0, 0, 5)
			expectLongestTrip(8 * time.Minute)

			kubeClient.failList = failList
			Expect(ExpectSingletonReconcileFailed(ctx, repairController)).To(MatchError(errInjected))
			expectNoSeries()

			// Without the kept start, the flip would make the trip read 1 minute.
			setBadNode(nodes[2], corev1.ConditionUnknown, env.Clock.Now())
			kubeClient.failList = nil
			env.Clock.Step(time.Minute)
			ExpectSingletonReconciled(ctx, repairController)
			expectBreaker(nodePool, 1, 9*time.Minute, 3, 10)
			expectBreaker(healthy, 0, 0, 0, 5)
			expectLongestTrip(9 * time.Minute)
		},
		Entry("listing nodepools", false, &v1.NodePoolList{}),
		Entry("listing nodes with --legacy-node-repair", true, &corev1.NodeList{}),
	)
	It("should remove every series and return the error when it can't work out when a trip started", func() {
		startController(nodepool.NewLegacyRepairController(env.Clock, env.Client, cp, failingTrippedAt{newLegacyRepair(env.Client)}))
		expectBreaker(nodePool, 0, 0, 0, 0)
		applyNodes(nodePool, 10, 3, env.Clock.Now())

		Expect(ExpectSingletonReconcileFailed(ctx, repairController)).To(MatchError(errInjected))
		expectNoSeries()
	})
	It("should date a trip to now when the nodes no longer show it as it works out the start", func() {
		startController(nodepool.NewLegacyRepairController(env.Clock, env.Client, cp, untrippedOnSecondRead{newLegacyRepair(env.Client)}))
		applyNodes(nodePool, 10, 3, env.Clock.Now())
		env.Clock.Step(10 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 0, 3, 10)
		env.Clock.Step(time.Minute)
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, time.Minute, 3, 10)
	})
	It("should only report nodepools that Karpenter manages", func() {
		unmanaged := test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
			NodeClassRef: &v1.NodeClassReference{Group: "karpenter.test.sh", Kind: "UnmanagedNodeClass", Name: "default"},
		}}}})
		ExpectApplied(ctx, env.Client, unmanaged)
		applyNodes(unmanaged, 5, 5, env.Clock.Now())
		// Nodes can also name a nodepool that doesn't exist.
		missing := test.NodePool()
		applyNodes(missing, 5, 5, env.Clock.Now())
		applyNodes(nodePool, 5, 0, env.Clock.Now())
		env.Clock.Step(10 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		expectNoBreaker(unmanaged)
		expectNoBreaker(missing)
		expectBreaker(nodePool, 0, 0, 0, 5)
		// Neither of the tripped nodepools counts toward the cluster.
		expectLongestTrip(0)
	})
	It("should count unhealthy nodes the way the legacy repair controller does", func() {
		cp.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Fatal$", TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
			{ConditionType: "KernelFault", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.RebootNode},
		}
		startController(nodepool.NewLegacyRepairController(env.Clock, env.Client, cp, newLegacyRepair(env.Client)))
		start := env.Clock.Now()
		// Of nodes 0 to 3, only the first two count, since the legacy controller ignores reboot policies and reasons that
		// don't match. The legacy controller lists the nodes, so node 5 counts too, though cluster state leaves it out
		// because it has no provider ID.
		nodes := append(applyNodes(nodePool, 5, 0, start), test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}}}))
		for i, condition := range map[int]corev1.NodeCondition{
			0: {Type: "BadNode", Status: corev1.ConditionFalse, Reason: "Fatal"},
			1: {Type: "BadNode", Status: corev1.ConditionFalse, Reason: "Fatal"},
			2: {Type: "BadNode", Status: corev1.ConditionFalse, Reason: "Other"},
			3: {Type: "KernelFault", Status: corev1.ConditionFalse},
			5: {Type: "BadNode", Status: corev1.ConditionFalse, Reason: "Fatal"},
		} {
			condition.LastTransitionTime = metav1.NewTime(start.Add(time.Duration(i) * time.Minute))
			nodes[i].Status.Conditions = append(nodes[i].Status.Conditions, condition)
			applyNode(nodes[i])
		}
		env.Clock.Step(10 * time.Minute)

		// 6 nodes allow 2 unhealthy, so the third that counts, node 5 at five minutes in, tripped the breaker.
		ExpectSingletonReconciled(ctx, repairController)
		expectBreaker(nodePool, 1, 5*time.Minute, 3, 6)
	})
})
