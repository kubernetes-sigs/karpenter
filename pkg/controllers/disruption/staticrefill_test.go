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
	"strings"
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
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// Static terminate-first must only terminate a node when the NodePool can refill the freed slot. The motivating case is
// a reserved-only static NodePool at limits.nodes whose capacity reservation is cancelled or expires: the provider
// demotes its NodeClaims to on-demand, requirements drift marks them Drifted, and terminating them would trade working
// nodes for slots that can never be refilled.
var _ = Describe("TerminateFirst/StaticRefill", func() {
	const noRefill = "no launchable capacity to refill this node"

	// blockedWith counts DisruptionBlocked events whose message contains substr.
	blockedWith := func(substr string) int {
		n := 0
		recorder.ForEachEvent(func(evt events.Event) {
			if evt.Reason == events.DisruptionBlocked && strings.Contains(evt.Message, substr) {
				n++
			}
		})
		return n
	}

	// addReservation appends a reserved offering for mostExpensiveInstance in the first offering's zone.
	addReservation := func(id string, capacity int, available bool) {
		mostExpensiveInstance.Requirements.Get(v1.CapacityTypeLabelKey).Insert(v1.CapacityTypeReserved)
		if mostExpensiveInstance.Requirements.Has(cloudprovider.ReservationIDLabel) {
			mostExpensiveInstance.Requirements.Get(cloudprovider.ReservationIDLabel).Insert(id)
		} else {
			mostExpensiveInstance.Requirements.Add(scheduling.NewRequirement(cloudprovider.ReservationIDLabel, corev1.NodeSelectorOpIn, id))
		}
		mostExpensiveInstance.Offerings = append(mostExpensiveInstance.Offerings, &cloudprovider.Offering{
			Price:               mostExpensiveOffering.Price / 1_000_000.0,
			Available:           available,
			ReservationCapacity: capacity,
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
				corev1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				cloudprovider.ReservationIDLabel: id,
			}),
		})
		ExpectSingletonReconciled(ctx, pricingController)
	}

	// staticPool returns a static NodePool at its node limit (replicas == limits.nodes == replicas) pinned to
	// mostExpensiveInstance with the given capacity types.
	staticPool := func(replicas int64, capacityTypes ...string) *v1.NodePool {
		return test.StaticNodePool(v1.NodePool{Spec: v1.NodePoolSpec{
			Replicas:   lo.ToPtr(replicas),
			Limits:     v1.Limits{resources.Node: resource.MustParse(fmt.Sprint(replicas))},
			Disruption: v1.Disruption{Budgets: []v1.Budget{{Nodes: "100%"}}},
			Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{mostExpensiveInstance.Name}},
				{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: capacityTypes},
			}}},
		}})
	}

	// nodeFor returns a NodeClaim/Node of the pool. reservationID == "" models a node that holds no reservation, e.g. one
	// the provider demoted from reserved to on-demand after its reservation ended.
	nodeFor := func(np *v1.NodePool, capacityType, reservationID string, drifted bool) (*v1.NodeClaim, *corev1.Node) {
		labels := map[string]string{
			v1.NodePoolLabelKey:            np.Name,
			corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
			v1.CapacityTypeLabelKey:        capacityType,
			corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
		}
		if reservationID != "" {
			labels[cloudprovider.ReservationIDLabel] = reservationID
		}
		nc, n := test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Status: v1.NodeClaimStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("32"), corev1.ResourcePods: resource.MustParse("100")},
			},
		})
		if drifted {
			nc.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
		}
		return nc, n
	}

	apply := func(np *v1.NodePool, ncs []*v1.NodeClaim, nodes []*corev1.Node) {
		ExpectApplied(ctx, env.Client, np)
		for i := range ncs {
			ExpectApplied(ctx, env.Client, ncs[i], nodes[i])
		}
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, ncs)
	}

	Context("StaticDrift", func() {
		var staticDrift *disruption.Controller

		BeforeEach(func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true)}}))
			staticDrift = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
				disruption.WithMethods(disruption.NewStaticDrift(cluster, prov, cloudProvider, recorder)))
		})

		// demoted builds a reserved-only pool of n nodes that were all demoted to on-demand (reservation gone).
		demoted := func(n int) (*v1.NodePool, []*v1.NodeClaim, []*corev1.Node) {
			np := staticPool(int64(n), v1.CapacityTypeReserved)
			var ncs []*v1.NodeClaim
			var nodes []*corev1.Node
			for range n {
				nc, node := nodeFor(np, v1.CapacityTypeOnDemand, "", true)
				ncs, nodes = append(ncs, nc), append(nodes, node)
			}
			return np, ncs, nodes
		}

		It("does not terminate-first demoted nodes of a reserved-only pool once the reservation is gone", func() {
			// The reservation no longer appears in the offerings, so the reserved-only template has nothing to launch into.
			np, ncs, nodes := demoted(3)
			apply(np, ncs, nodes)

			ExpectSingletonReconciled(ctx, staticDrift)

			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(blockedWith(noRefill)).To(BeNumerically(">", 0))
			for _, nc := range ncs {
				ExpectExists(ctx, env.Client, nc)
			}
		})

		It("does not terminate-first demoted nodes while the ended reservation is still listed as full", func() {
			// The provider can still list the reservation (healthy, 0 free) briefly after it ends. The demoted nodes hold no
			// slot in it, so terminating them frees nothing a refill could use.
			addReservation("r-gone", 0, true)
			np, ncs, nodes := demoted(2)
			apply(np, ncs, nodes)

			ExpectSingletonReconciled(ctx, staticDrift)

			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(blockedWith(noRefill)).To(BeNumerically(">", 0))
		})

		It("terminates first when each node frees a slot in its own full reservation", func() {
			addReservation("r-1", 0, true)
			np := staticPool(2, v1.CapacityTypeReserved)
			nc1, n1 := nodeFor(np, v1.CapacityTypeReserved, "r-1", true)
			nc2, n2 := nodeFor(np, v1.CapacityTypeReserved, "r-1", true)
			apply(np, []*v1.NodeClaim{nc1, nc2}, []*corev1.Node{n1, n2})

			ExpectSingletonReconciled(ctx, staticDrift)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(2))
			for _, cmd := range cmds {
				Expect(cmd.Decision()).To(Equal(disruption.TerminateFirstDecision))
			}
		})

		It("does not terminate first a reserved node whose reservation is no longer usable", func() {
			// Expiring / ICE'd reservations are reported unavailable; the freed slot couldn't be relaunched into.
			addReservation("r-1", 0, false)
			np := staticPool(1, v1.CapacityTypeReserved)
			nc, n := nodeFor(np, v1.CapacityTypeReserved, "r-1", true)
			apply(np, []*v1.NodeClaim{nc}, []*corev1.Node{n})

			ExpectSingletonReconciled(ctx, staticDrift)

			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(blockedWith(noRefill)).To(BeNumerically(">", 0))
		})

		It("terminates first an on-demand static pool at its node limit", func() {
			np := staticPool(3, v1.CapacityTypeOnDemand)
			var ncs []*v1.NodeClaim
			var nodes []*corev1.Node
			for range 3 {
				nc, node := nodeFor(np, v1.CapacityTypeOnDemand, "", true)
				ncs, nodes = append(ncs, nc), append(nodes, node)
			}
			apply(np, ncs, nodes)

			ExpectSingletonReconciled(ctx, staticDrift)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(3))
			for _, cmd := range cmds {
				Expect(cmd.Decision()).To(Equal(disruption.TerminateFirstDecision))
			}
		})

		It("does not terminate first an on-demand static pool whose offerings are all unavailable", func() {
			for _, o := range mostExpensiveInstance.Offerings {
				if o.CapacityType() == v1.CapacityTypeOnDemand {
					o.Available = false
				}
			}
			np := staticPool(1, v1.CapacityTypeOnDemand)
			nc, n := nodeFor(np, v1.CapacityTypeOnDemand, "", true)
			apply(np, []*v1.NodeClaim{nc}, []*corev1.Node{n})

			ExpectSingletonReconciled(ctx, staticDrift)

			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(blockedWith(noRefill)).To(BeNumerically(">", 0))
		})

		It("terminates first only as many demoted nodes as a new reservation has free slots", func() {
			// Reservation rotation: the old reservation ended (nodes demoted) and a new one with one free slot was added.
			addReservation("r-new", 1, true)
			np, ncs, nodes := demoted(2)
			apply(np, ncs, nodes)

			ExpectSingletonReconciled(ctx, staticDrift)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.TerminateFirstDecision))
			Expect(blockedWith(noRefill)).To(BeNumerically(">", 0))
		})

		It("counts nodes still owed to the pool against a new reservation's free slots", func() {
			// replicas 2 at the limit: one demoted node is already terminating (its refill is owed and will take the only
			// free slot), so the other demoted node must not be terminated first.
			addReservation("r-new", 1, true)
			np := staticPool(2, v1.CapacityTypeReserved)
			nc1, n1 := nodeFor(np, v1.CapacityTypeOnDemand, "", true)
			nc2, n2 := nodeFor(np, v1.CapacityTypeOnDemand, "", false)
			nc2.Finalizers = []string{v1.TerminationFinalizer}
			apply(np, []*v1.NodeClaim{nc1, nc2}, []*corev1.Node{n1, n2})
			Expect(env.Client.Delete(ctx, nc2)).To(Succeed())
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nc2))
			active, deleting, _ := cluster.NodePoolState.GetNodeCount(np.Name)
			Expect([]int{active, deleting}).To(Equal([]int{1, 1}))

			ExpectSingletonReconciled(ctx, staticDrift)

			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(blockedWith(noRefill)).To(BeNumerically(">", 0))
		})

		It("terminates first a node that frees its own slot even when other candidates can't be refilled", func() {
			// Budget 1: a demoted candidate must not starve one that holds a slot in the healthy reservation.
			addReservation("r-1", 0, true)
			np := staticPool(2, v1.CapacityTypeReserved)
			np.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "1"}}
			ncDemoted, nDemoted := nodeFor(np, v1.CapacityTypeOnDemand, "", true)
			ncReserved, nReserved := nodeFor(np, v1.CapacityTypeReserved, "r-1", true)
			apply(np, []*v1.NodeClaim{ncDemoted, ncReserved}, []*corev1.Node{nDemoted, nReserved})

			ExpectSingletonReconciled(ctx, staticDrift)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.TerminateFirstDecision))
			Expect(cmds[0].Candidates[0].NodeClaim.Name).To(Equal(ncReserved.Name))
		})

		It("does not terminate first when the NodePool is NotReady", func() {
			np := staticPool(1, v1.CapacityTypeOnDemand)
			np.StatusConditions().SetFalse(v1.ConditionTypeValidationSucceeded, "NotReady", "NotReady")
			nc, n := nodeFor(np, v1.CapacityTypeOnDemand, "", true)
			apply(np, []*v1.NodeClaim{nc}, []*corev1.Node{n})

			ExpectSingletonReconciled(ctx, staticDrift)

			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(blockedWith("not ready to provision a replacement")).To(BeNumerically(">", 0))
		})

		It("leaves the gates-off behavior unchanged (Blocked at the node limit)", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(false), ReservedCapacity: lo.ToPtr(true)}}))
			np, ncs, nodes := demoted(1)
			apply(np, ncs, nodes)

			ExpectSingletonReconciled(ctx, staticDrift)

			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(blockedWith("at its node limit and cannot stage a replacement")).To(BeNumerically(">", 0))
			Expect(blockedWith(noRefill)).To(BeZero())
		})
	})

	Context("StaticRepair", func() {
		var repair *disruption.Controller

		BeforeEach(func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true), TerminateFirstRepair: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true)}}))
			cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
				{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, TerminationGracePeriod: lo.ToPtr(10 * time.Minute), Action: cloudprovider.ReplaceNode},
			}
			repair = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
				disruption.WithMethods(disruption.NewRepair(disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue))))
		})

		markUnhealthy := func(n *corev1.Node) {
			n = ExpectExists(ctx, env.Client, n)
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionFalse, LastTransitionTime: metav1.Time{Time: env.Clock.Now()}})
			ExpectApplied(ctx, env.Client, n)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
		}

		It("does not terminate first a demoted node of a reserved-only pool whose reservation is gone", func() {
			np := staticPool(1, v1.CapacityTypeReserved)
			nc, n := nodeFor(np, v1.CapacityTypeOnDemand, "", false)
			apply(np, []*v1.NodeClaim{nc}, []*corev1.Node{n})
			markUnhealthy(n)
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repair)

			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(blockedWith(noRefill)).To(BeNumerically(">", 0))
		})

		It("terminates first a reserved node that frees a slot in its own full reservation", func() {
			addReservation("r-1", 0, true)
			np := staticPool(1, v1.CapacityTypeReserved)
			nc, n := nodeFor(np, v1.CapacityTypeReserved, "r-1", false)
			apply(np, []*v1.NodeClaim{nc}, []*corev1.Node{n})
			markUnhealthy(n)
			env.Clock.Step(31 * time.Minute)

			ExpectSingletonReconciled(ctx, repair)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.TerminateFirstDecision))
		})
	})
})
