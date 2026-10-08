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

package state_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Unhealthy Nodes", func() {
	badNode := func() corev1.NodeCondition {
		return corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(env.Clock.Now())}
	}
	managedNode := func(conditions ...corev1.NodeCondition) (*v1.NodeClaim, *corev1.Node) {
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			v1.NodePoolLabelKey:            nodePool.Name,
			corev1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[0].Name,
		}}})
		node.Status.Conditions = append(node.Status.Conditions, conditions...)
		ExpectApplied(ctx, env.Client, nodeClaim, node)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		return nodeClaim, node
	}
	updateNode := func(node *corev1.Node, mutate func(*corev1.Node)) {
		node = ExpectExists(ctx, env.Client, node)
		mutate(node)
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
	}
	heartbeat := func(node *corev1.Node) {
		updateNode(node, func(n *corev1.Node) {
			for i := range n.Status.Conditions {
				n.Status.Conditions[i].LastHeartbeatTime = metav1.NewTime(env.Clock.Now().Add(time.Minute))
			}
		})
	}
	stateNode := func(node *corev1.Node) *state.StateNode {
		for n := range cluster.Nodes() {
			if n.Node != nil && n.Node.Name == node.Name {
				return n.DeepCopy()
			}
		}
		Fail("node not in cluster state")
		return nil
	}
	unhealthyNames := func() []string {
		var names []string
		for n := range cluster.Nodes() {
			// Past every policy's toleration, so this lists Nodes that match any policy.
			if n.GetRepairResult(env.Clock.Now().Add(24*time.Hour)).Action != "" {
				names = append(names, n.Node.Name)
			}
		}
		return names
	}

	var suiteCluster *state.Cluster
	var suiteNodeController *informer.NodeController
	var suiteNodeClaimController *informer.NodeClaimController
	var matcher *health.RepairPolicyMatcher
	// useMatcher builds cluster state around a matcher, as the operator does once at startup.
	useMatcher := func(m *health.RepairPolicyMatcher) {
		cluster = state.NewCluster(env.Clock, env.Client, cloudProvider, state.WithRepairPolicyMatcher(m))
		nodeController = informer.NewNodeController(env.Client, cluster)
		nodeClaimController = informer.NewNodeClaimController(env.Client, cloudProvider, cluster, clusterCost)
	}

	BeforeEach(func() {
		suiteCluster, suiteNodeController, suiteNodeClaimController = cluster, nodeController, nodeClaimController
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Kernel$", TolerationDuration: time.Minute, Action: cloudprovider.ReplaceNode},
			{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionUnknown, ReasonRegex: ".*", TolerationDuration: 10 * time.Minute, Action: cloudprovider.ReplaceNode},
		}
		matcher = lo.Must(health.NewRepairPolicyMatcher(
			options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true)}})), cloudProvider))
		useMatcher(matcher)
	})
	AfterEach(func() {
		cluster, nodeController, nodeClaimController = suiteCluster, suiteNodeController, suiteNodeClaimController
	})

	It("should mark a node unhealthy as soon as a condition matches a policy, before its toleration elapses", func() {
		_, unhealthy := managedNode(badNode())
		managedNode()

		Expect(unhealthyNames()).To(ConsistOf(unhealthy.Name))
		Expect(stateNode(unhealthy).GetRepairResult(env.Clock.Now()).Action).To(BeEmpty())
	})
	It("should match on condition type and status", func() {
		_, unknown := managedNode(corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionUnknown})
		managedNode(corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse})
		managedNode(corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionTrue})

		Expect(unhealthyNames()).To(ConsistOf(unknown.Name))
	})
	It("should match a condition that is not the last one", func() {
		_, node := managedNode(badNode(), corev1.NodeCondition{Type: "Other", Status: corev1.ConditionTrue})

		Expect(unhealthyNames()).To(ConsistOf(node.Name))
	})
	It("should resolve the toleration against the clock without a Node update", func() {
		_, node := managedNode(badNode())

		env.Clock.Step(31 * time.Minute)
		result := stateNode(node).GetRepairResult(env.Clock.Now())
		Expect(result.Action).To(Equal(cloudprovider.ReplaceNode))
		Expect(result.Condition).To(Equal(corev1.NodeConditionType("BadNode")))
	})
	It("should re-match the policies when a condition's reason changes", func() {
		_, node := managedNode(badNode())
		env.Clock.Step(2 * time.Minute)
		Expect(stateNode(node).GetRepairResult(env.Clock.Now()).Action).To(BeEmpty())

		updateNode(node, func(n *corev1.Node) {
			n.Status.Conditions = []corev1.NodeCondition{badNode()}
			n.Status.Conditions[0].Reason = "Kernel"
			n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(env.Clock.Now().Add(-2 * time.Minute))
		})
		Expect(stateNode(node).GetRepairResult(env.Clock.Now()).ReasonRegex).To(Equal("^Kernel$"))
	})
	It("should mark a node healthy when its condition recovers", func() {
		_, node := managedNode(badNode())

		updateNode(node, func(n *corev1.Node) { n.Status.Conditions = nil })
		Expect(unhealthyNames()).To(BeEmpty())
	})
	It("should keep a node unhealthy across heartbeats", func() {
		_, node := managedNode(badNode())

		heartbeat(node)
		Expect(unhealthyNames()).To(ConsistOf(node.Name))
	})
	It("should keep a node unhealthy across a NodeClaim update followed by a heartbeat", func() {
		nodeClaim, node := managedNode(badNode())
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		nodeClaim.Labels["updated"] = "true"
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		heartbeat(node)
		Expect(unhealthyNames()).To(ConsistOf(node.Name))
	})
	It("should not report a node whose Node was deleted while its NodeClaim remains", func() {
		_, node := managedNode(badNode())
		ExpectDeleted(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectStateNodeCount("==", 1)

		Expect(unhealthyNames()).To(BeEmpty())
		for n := range cluster.Nodes() {
			Expect(n.GetRepairResult(env.Clock.Now().Add(time.Hour)).Action).To(BeEmpty())
		}
	})
	It("should match nodes in a newly constructed cluster", func() {
		_, node := managedNode(badNode())
		fresh := state.NewCluster(env.Clock, env.Client, cloudProvider, state.WithRepairPolicyMatcher(matcher))

		Expect(fresh.UpdateNode(ctx, ExpectExists(ctx, env.Client, node))).To(Succeed())
		Expect(lo.SomeBy(fresh.DeepCopyNodes(), func(n *state.StateNode) bool {
			return n.GetRepairResult(env.Clock.Now().Add(24*time.Hour)).Action != ""
		})).To(BeTrue())
	})
	It("should not match nodes without a matcher, as when node repair is disabled", func() {
		useMatcher(nil)
		managedNode(badNode())

		Expect(unhealthyNames()).To(BeEmpty())
	})
})
