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
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// These tests exercise Terminate-First Disruption for repair (F2 / RFC #3203): a capacity-constrained NodePool has no
// room to pre-spin a replacement, so repair issues a delete-only command and lets reactive provisioning refill the
// freed slot, while a headroom pool still replaces-first.

var _ = Describe("Repair/TerminateFirst", func() {
	var repairController *disruption.Controller

	markUnhealthy := func(n *corev1.Node) {
		n = ExpectExists(ctx, env.Client, n)
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
			Type:               "BadNode",
			Status:             corev1.ConditionFalse,
			LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
		})
		ExpectApplied(ctx, env.Client, n)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
	}

	BeforeEach(func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true), TerminateFirstRepair: lo.ToPtr(true)}}))
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute},
		}
		repairController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
			disruption.WithMethods(disruption.NewRepair(disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue))))
	})

	// staticNodePoolAtLimit: limit == replicas, so the pool is at its limit and can't stage a replacement.
	staticNodePoolAtLimit := func(replicas int64) *v1.NodePool {
		return test.StaticNodePool(v1.NodePool{Spec: v1.NodePoolSpec{
			Replicas: lo.ToPtr(replicas),
			Limits:   v1.Limits{resources.Node: resource.MustParse(strconv.FormatInt(replicas, 10))},
		}})
	}

	// INV-F2-1: a static NodePool at its node limit can't pre-spin, so repair terminates first.
	It("should issue a delete-only command for a static NodePool at its node limit", func() {
		nodePool := staticNodePoolAtLimit(1)
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"},
		}})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		markUnhealthy(node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Decision()).To(Equal(disruption.TerminateFirstDecision))
		Expect(cmds[0].Replacements).To(HaveLen(0))
	})

	// Below the limit there's room to stage a replacement, so repair replaces-first even with the gate on.
	It("should replace-first for a static NodePool below its node limit", func() {
		nodePool := test.StaticNodePool(v1.NodePool{Spec: v1.NodePoolSpec{
			Replicas: lo.ToPtr(int64(1)),
			Limits:   v1.Limits{resources.Node: resource.MustParse("2")},
		}})
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"},
		}})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		markUnhealthy(node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Decision()).To(Equal(disruption.ReplaceDecision))
		Expect(cmds[0].Replacements).To(HaveLen(1))
	})

	// Headroom must be judged with the NodePool's atomic reservation accounting, not a naive node count: outstanding
	// reservations (an in-flight replacement) and deleting nodes count against limits.nodes. Here replicas=1, limit=2,
	// one active node, and one slot already reserved — the pool is effectively at its limit, so repair must terminate
	// first rather than stage a replacement that would push it past the limit.
	It("does not stage a replacement past limits.nodes when a slot is already reserved", func() {
		nodePool := test.StaticNodePool(v1.NodePool{Spec: v1.NodePoolSpec{
			Replicas: lo.ToPtr(int64(1)),
			Limits:   v1.Limits{resources.Node: resource.MustParse("2")},
		}})
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"},
		}})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		// Simulate an outstanding reservation (an in-flight replacement from another command) consuming the only spare
		// slot under the limit. A naive active-node count (1 < 2) would wrongly see headroom and replace-first.
		Expect(cluster.NodePoolState.ReserveNodeCount(nodePool.Name, 2, 1)).To(BeEquivalentTo(int64(1)))
		markUnhealthy(node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Decision()).To(Equal(disruption.TerminateFirstDecision))
		Expect(cmds[0].Replacements).To(HaveLen(0))
	})

	// Gate off: a static NodePool at its limit is NOT terminate-first'd. It also can't pre-spin (at the limit), so it is
	// Blocked (no command) rather than freed — the feature gate is honored for the static path.
	It("does not terminate-first a static NodePool at its limit when TerminateFirstRepair is disabled", func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true), TerminateFirstRepair: lo.ToPtr(false)}}))
		nodePool := staticNodePoolAtLimit(1)
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"},
		}})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		markUnhealthy(node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	// A static NodePool at its limit is only terminate-first'd if it's actually refillable: static provisioning refuses
	// NotReady (or deleting) NodePools, so terminating first there would strand the workload. Repair must Block instead.
	It("does not terminate-first a static NodePool at its limit when the NodePool is NotReady", func() {
		nodePool := staticNodePoolAtLimit(1)
		nodePool.StatusConditions().SetFalse(v1.ConditionTypeValidationSucceeded, "NotReady", "NotReady")
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"},
		}})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		markUnhealthy(node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	// Repair doesn't disrupt a static pool mid scale-down (mirrors StaticDrift): with more nodes running than the desired
	// replica count, a staged replacement would just be deleted by deprovisioning, so repair issues no command.
	It("does not disrupt a static NodePool that is scaling down", func() {
		nodePool := test.StaticNodePool(v1.NodePool{Spec: v1.NodePoolSpec{
			Replicas: lo.ToPtr(int64(1)),
			Limits:   v1.Limits{resources.Node: resource.MustParse("3")}, // below limit, but over replicas
		}})
		nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"},
		}})
		ExpectApplied(ctx, env.Client, nodePool)
		for i := range nodes {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
		markUnhealthy(nodes[0])
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	// Terminate-first is behind the TerminateFirstRepair gate: with it off, a reserved candidate whose reservation is
	// full is not terminate-first'd — repair falls back to its normal pre-spin, which can't stage a replacement here, so
	// the node is Blocked (no command) rather than freed.
	It("does not terminate-first when TerminateFirstRepair is disabled", func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true), TerminateFirstRepair: lo.ToPtr(false)}}))
		reservationID := "r-" + mostExpensiveInstance.Name
		mostExpensiveInstance.Requirements.Add(scheduling.NewRequirement(cloudprovider.ReservationIDLabel, corev1.NodeSelectorOpIn, reservationID))
		mostExpensiveInstance.Requirements.Get(v1.CapacityTypeLabelKey).Insert(v1.CapacityTypeReserved)
		mostExpensiveInstance.Offerings = append(mostExpensiveInstance.Offerings, &cloudprovider.Offering{
			Price: mostExpensiveOffering.Price / 1_000_000.0, Available: true, ReservationCapacity: 0,
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
				corev1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				cloudprovider.ReservationIDLabel: reservationID,
			}),
		})
		ExpectSingletonReconciled(ctx, pricingController)
		nodePool := test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
			Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{mostExpensiveInstance.Name}},
				{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeReserved}},
			},
		}}}})
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			v1.NodePoolLabelKey:              nodePool.Name,
			corev1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
			v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
			corev1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			cloudprovider.ReservationIDLabel: reservationID,
		}}, Status: v1.NodeClaimStatus{
			ProviderID:  test.RandomProviderID(),
			Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32"), corev1.ResourcePods: resource.MustParse("100")},
		}})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
		}}}})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		markUnhealthy(node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		Expect(queue.GetCommands()).To(HaveLen(0))
	})

	// INV-F2-2: a normal (dynamic, headroom) NodePool still replaces-first — a pre-spun replacement is created.
	It("should still replace-first for a headroom (dynamic) NodePool", func() {
		nodePool := test.NodePool()
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"},
		}})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		// Bind a reschedulable pod so the pre-spin simulation produces a replacement.
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
		}}}})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		markUnhealthy(node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Decision()).To(Equal(disruption.ReplaceDecision))
		Expect(cmds[0].Replacements).To(HaveLen(1))
	})

	// INV-F2-4: terminate-first is still paced — repair issues one command per pass. A static pool of 10 nodes with 2
	// unhealthy stays under the 20% correlated-failure breaker (ceil(20%*10)=2; tripped only when unhealthy > 2), so with
	// two eligible candidates repair still returns a single terminate-first command.
	It("should still pace terminate-first (one command per pass)", func() {
		const count = 10
		nodePool := staticNodePoolAtLimit(count) // at limit -> terminate-first
		nodeClaims, nodes := test.NodeClaimsAndNodes(count, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"},
		}})
		ExpectApplied(ctx, env.Client, nodePool)
		for i := range nodes {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
		markUnhealthy(nodes[0])
		markUnhealthy(nodes[1])
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Decision()).To(Equal(disruption.TerminateFirstDecision))
	})

	// INV-F2-3: a reserved NodePool whose reservation is full (Available=true, ReservationCapacity=0) with no fallback
	// can't pre-spin a replacement — the pod can only reschedule once the candidate frees its own reservation slot. This
	// is decided by SimulateSchedulingWithReservedFallback (identical to Drift), so repair terminates first (delete-only).
	It("should issue a delete-only command for a reserved NodePool whose reservation is full with no fallback", func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true), TerminateFirstRepair: lo.ToPtr(true)}}))
		reservationID := "r-" + mostExpensiveInstance.Name
		mostExpensiveInstance.Requirements.Add(scheduling.NewRequirement(cloudprovider.ReservationIDLabel, corev1.NodeSelectorOpIn, reservationID))
		mostExpensiveInstance.Requirements.Get(v1.CapacityTypeLabelKey).Insert(v1.CapacityTypeReserved)
		mostExpensiveInstance.Offerings = append(mostExpensiveInstance.Offerings, &cloudprovider.Offering{
			Price:               mostExpensiveOffering.Price / 1_000_000.0,
			Available:           true, // full but healthy
			ReservationCapacity: 0,
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
				corev1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				cloudprovider.ReservationIDLabel: reservationID,
			}),
		})
		ExpectSingletonReconciled(ctx, pricingController)

		nodePool := test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
			Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{mostExpensiveInstance.Name}},
				{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeReserved}},
			},
		}}}})
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			v1.NodePoolLabelKey:              nodePool.Name,
			corev1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
			v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
			corev1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			cloudprovider.ReservationIDLabel: reservationID,
		}}, Status: v1.NodeClaimStatus{
			ProviderID:  test.RandomProviderID(),
			Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32"), corev1.ResourcePods: resource.MustParse("100")},
		}})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		// A reschedulable pod so the pre-spin simulation has a workload it can only place by freeing the reservation.
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
		}}}})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		markUnhealthy(node)
		env.Clock.Step(31 * time.Minute)

		ExpectSingletonReconciled(ctx, repairController)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Decision()).To(Equal(disruption.TerminateFirstDecision))
		Expect(cmds[0].Replacements).To(HaveLen(0))
		// The delete-only command must carry the pass-2 (credit-back) Results — that's what nominates the existing
		// (freed) node for the reschedulable pod; dropping it or returning pass-1 Results would lose that nomination.
		Expect(cmds[0].Results.NewNodeClaims).To(HaveLen(1))
		Expect(cmds[0].Results.NewNodeClaims[0].Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeReserved)).To(BeTrue())
		Expect(cmds[0].Results.NewNodeClaims[0].Requirements.Get(cloudprovider.ReservationIDLabel).Has(reservationID)).To(BeTrue())
	})
})
