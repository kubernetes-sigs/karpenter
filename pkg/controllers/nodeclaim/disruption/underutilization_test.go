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
	"fmt"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Underutilization", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenUnderutilized
		nodePool.Spec.Disruption.ConsolidateAfter = &v1.NillableDuration{Duration: lo.ToPtr(1 * time.Minute)}
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: "default-instance-type", // need the instance type for the cluster state update
				},
			},
		})
		cluster.SetInitTime(fakeClock.Now().Add(-5 * time.Minute))
	})
	Context("Metrics", func() {
		It("should fire a karpenter_nodeclaims_disrupted metric when underutilized", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			fmt.Printf("this is the fake clock time: %s\n this is the apiserver time %s", fakeClock.Now(), nodeClaim.CreationTimestamp.Time)
			// set the fake clock to the same time as the creation timestamp of the nodeclaim.
			// this is necessary to avoid differences in the api server clock vs our injected clock
			fakeClock.SetTime(nodeClaim.CreationTimestamp.Time)

			fakeClock.Step(1 * time.Minute)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized).IsTrue()).To(BeTrue())

			metric, found := FindMetricWithLabelValues("karpenter_nodeclaims_disrupted", map[string]string{
				"type":     "underutilized",
				"nodepool": nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetCounter().GetValue()).To(BeNumerically("==", 1))
		})
	})
	It("should mark NodeClaims as underutilized based on node age", func() {
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		fmt.Printf("this is the fake clock time: %s\n this is the apiserver time %s", fakeClock.Now(), nodeClaim.CreationTimestamp.Time)
		// set the fake clock to the same time as the creation timestamp of the nodeclaim.
		// this is necessary to avoid differences in the api server clock vs our injected clock
		fakeClock.SetTime(nodeClaim.CreationTimestamp.Time)

		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		fakeClock.Step(30 * time.Second)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized).IsTrue()).To(BeFalse())
		fakeClock.Step(30 * time.Second)

		timeNow := fakeClock.Now()
		fmt.Printf("this is the time now %s\n", timeNow)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized).IsTrue()).To(BeTrue())
	})
	It("should mark NodeClaims as underutilized based on cluster state initialization", func() {
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		// set the fake clock to the same time as the creation timestamp of the nodeclaim.
		// this is necessary to avoid differences in the api server clock vs our injected clock
		fakeClock.SetTime(nodeClaim.CreationTimestamp.Time)

		fakeClock.Step(30 * time.Second)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		// reset the cluster state
		cluster.SetInitTime(fakeClock.Now())
		fakeClock.Step(30 * time.Second)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized).IsTrue()).To(BeFalse())
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		fakeClock.Step(30 * time.Second)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized).IsTrue()).To(BeTrue())
	})
	It("should clear the underutilized condition when a pod is added", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeUnderutilized)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		// set the fake clock to the same time as the creation timestamp of the nodeclaim.
		// this is necessary to avoid differences in the api server clock vs our injected clock
		fakeClock.SetTime(nodeClaim.CreationTimestamp.Time)
		// set the clock 1 minute in the future so it's underutilized after cluster state initialization
		fakeClock.Step(1 * time.Minute)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		// Pod owned by a Deployment
		pod := test.Pod(test.PodOptions{
			NodeName: node.Name,
		})
		ExpectApplied(ctx, env.Client, pod)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(pod))

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized).IsTrue()).To(BeFalse())
	})
	It("should clear the underutilized condition when a pod is removed", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeUnderutilized)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		// set the fake clock to the same time as the creation timestamp of the nodeclaim.
		// this is necessary to avoid differences in the api server clock vs our injected clock
		fakeClock.SetTime(nodeClaim.CreationTimestamp.Time)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		// Pod owned by a Deployment
		pod := test.Pod(test.PodOptions{
			NodeName: node.Name,
		})
		ExpectApplied(ctx, env.Client, pod)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(pod))

		// set the clock 1 minute in the future so it's underutilized after cluster state initialization
		fakeClock.Step(1 * time.Minute)

		ExpectDeleted(ctx, env.Client, pod)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(pod))

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized).IsTrue()).To(BeFalse())
	})
	It("should clear the underutilized condition when a pod succeeds", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeUnderutilized)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		// set the fake clock to the same time as the creation timestamp of the nodeclaim.
		// this is necessary to avoid differences in the api server clock vs our injected clock
		fakeClock.SetTime(nodeClaim.CreationTimestamp.Time)

		// Pod owned by a Deployment
		pod := test.Pod(test.PodOptions{
			NodeName: node.Name,
		})
		ExpectApplied(ctx, env.Client, pod)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(pod))

		// set the clock 1 minute in the future so it's underutilized after cluster state initialization
		fakeClock.Step(1 * time.Minute)

		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		pod.Status.Phase = corev1.PodSucceeded
		ExpectApplied(ctx, env.Client, pod)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(pod))

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized).IsTrue()).To(BeFalse())
	})
	It("should clear the underutilized condition when a pod fails", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeUnderutilized)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		// set the fake clock to the same time as the creation timestamp of the nodeclaim.
		// this is necessary to avoid differences in the api server clock vs our injected clock
		fakeClock.SetTime(nodeClaim.CreationTimestamp.Time)

		// Pod owned by a Deployment
		pod := test.Pod(test.PodOptions{
			NodeName: node.Name,
		})
		ExpectApplied(ctx, env.Client, pod)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(pod))

		// set the clock 1 minute in the future so it's underutilized after cluster state initialization
		fakeClock.Step(1 * time.Minute)

		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		pod.Status.Phase = corev1.PodFailed
		ExpectApplied(ctx, env.Client, pod)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(pod))

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized).IsTrue()).To(BeFalse())
	})
	It("should remove the status condition from the nodeClaim when underutilization is disabled", func() {
		nodePool.Spec.Disruption.ConsolidateAfter.Duration = nil
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeUnderutilized)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the nodeClaim initialization condition is unknown", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeUnderutilized)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		nodeClaim.StatusConditions().SetUnknown(v1.ConditionTypeInitialized)
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the nodeClaim initialization condition is false", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeUnderutilized)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		nodeClaim.StatusConditions().SetFalse(v1.ConditionTypeInitialized, "NotInitialized", "NotInitialized")
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the node doesn't exist", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeUnderutilized)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{}, []*v1.NodeClaim{nodeClaim})
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeUnderutilized)).To(BeNil())
	})
})
