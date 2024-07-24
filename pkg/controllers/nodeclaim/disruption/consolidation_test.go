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
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Underutilized", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenUnderutilized
		nodePool.Spec.Disruption.ConsolidateAfter = v1.NillableDuration{Duration: lo.ToPtr(1 * time.Minute)}
		nodeClaim, _ = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: "default-instance-type", // need the instance type for the cluster state update
				},
			},
		})
		// set the lastPodEvent to 5 minutes in the past
		nodeClaim.Status.LastPodEvent.Time = fakeClock.Now().Add(-5 * time.Minute)
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
		ExpectApplied(ctx, env.Client, nodeClaim, nodePool)
	})
	Context("Metrics", func() {
		It("should fire a karpenter_nodeclaims_disrupted metric when consolidatable", func() {
			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable).IsTrue()).To(BeTrue())
			metric, found := FindMetricWithLabelValues("karpenter_nodeclaims_disrupted", map[string]string{
				"type":     "consolidatable",
				"nodepool": nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetCounter().GetValue()).To(BeNumerically("==", 1))
		})
	})
	It("should mark NodeClaims as consolidatable", func() {
		// set the lastPodEvent as now, so it's first marked as not consolidatable
		nodeClaim.Status.LastPodEvent.Time = fakeClock.Now()
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable).IsTrue()).To(BeFalse())

		fakeClock.Step(1 * time.Minute)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable).IsTrue()).To(BeTrue())
	})
	It("should mark NodeClaims as consolidatable based on the nodeclaim initialized time", func() {
		// set the lastPodEvent as zero, so it's like no pods have scheduled
		nodeClaim.Status.LastPodEvent.Time = time.Time{}

		ExpectApplied(ctx, env.Client, nodeClaim)
		fakeClock.SetTime(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).LastTransitionTime.Time)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable).IsTrue()).To(BeFalse())

		fakeClock.Step(1 * time.Minute)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable).IsTrue()).To(BeTrue())
	})
	It("should remove the status condition from the nodeClaim when lastPodEvent is too recent", func() {
		nodeClaim.Status.LastPodEvent.Time = fakeClock.Now()
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when consolidateAfter is never", func() {
		nodePool.Spec.Disruption.ConsolidateAfter = v1.NillableDuration{}
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the nodeClaim initialization condition is unknown", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		nodeClaim.StatusConditions().SetUnknown(v1.ConditionTypeInitialized)
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the nodeClaim is not initialized", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		nodeClaim.StatusConditions().SetFalse(v1.ConditionTypeInitialized, "NotInitialized", "NotInitialized")
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
})
