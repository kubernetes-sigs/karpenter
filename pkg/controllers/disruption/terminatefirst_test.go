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

// Terminate-First Disruption (RFC kubernetes-sigs/karpenter#3203) end-to-end. A voluntary disruption of a
// capacity-constrained pool (a static NodePool at its replica count, or a reserved candidate whose reservation is
// full) issues a delete-only command and lets reactive provisioning refill the freed slot, instead of the
// replace-first it cannot stage.

var _ = Describe("TerminateFirstDrift", func() {
	// bindReschedulablePod places a ReplicaSet-owned (reschedulable) pod on the node so the scheduling simulation has a
	// workload to protect — the replacement it produces is what terminate-first suppresses.
	bindReschedulablePod := func(node *corev1.Node) {
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
		}}}})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
	}

	// bindAntiAffinityPods binds `count` reschedulable, ReplicaSet-owned pods to the node that cannot co-locate (host
	// anti-affinity), so rescheduling them requires `count` separate nodes.
	bindAntiAffinityPods := func(node *corev1.Node, count int) {
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		for i := 0; i < count; i++ {
			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"tf-spread": "x"},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
					}},
				},
				PodAntiRequirements: []corev1.PodAffinityTerm{{
					TopologyKey:   corev1.LabelHostname,
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tf-spread": "x"}},
				}},
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)
		}
	}

	Context("StaticDrift", func() {
		var nodePool *v1.NodePool
		var nodeClaim *v1.NodeClaim
		var node *corev1.Node
		var staticDriftController *disruption.Controller

		BeforeEach(func() {
			nodePool = test.StaticNodePool(v1.NodePool{Spec: v1.NodePoolSpec{
				Replicas:   lo.ToPtr(int64(1)),
				Disruption: v1.Disruption{Budgets: []v1.Budget{{Nodes: "100%"}}},
			}})
			nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
					v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				}},
				Status: v1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32"), corev1.ResourcePods: resource.MustParse("100")},
				},
			})
			nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			staticDriftController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
				disruption.WithMethods(disruption.NewStaticDrift(cluster, prov, cloudProvider)))
		})

		It("issues a delete-only command for a static NodePool at its node limit when TerminateFirstDrift is enabled", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true)}}))
			// At the node limit (limit == replicas == 1): the pool can't stage a replacement without bursting over the
			// limit, so terminate-first applies.
			nodePool.Spec.Limits = v1.Limits{resources.Node: resource.MustParse("1")}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			ExpectSingletonReconciled(ctx, staticDriftController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.TerminateFirstDecision))
			Expect(cmds[0].Replacements).To(HaveLen(0))
		})

		It("replaces-first for a static NodePool below its node limit even when TerminateFirstDrift is enabled", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true)}}))
			// Below the node limit (limit 2 > replicas 1): the pool can stage a replacement without bursting over the
			// limit, so it replaces-first rather than terminating first even with the gate on.
			nodePool.Spec.Limits = v1.Limits{resources.Node: resource.MustParse("2")}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			ExpectSingletonReconciled(ctx, staticDriftController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.ReplaceDecision))
			Expect(cmds[0].Replacements).To(HaveLen(1))
		})

		It("replaces-first for a static NodePool when TerminateFirstDrift is disabled", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(false)}}))
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

			ExpectSingletonReconciled(ctx, staticDriftController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.ReplaceDecision))
			Expect(cmds[0].Replacements).To(HaveLen(1))
		})
	})

	Context("Drift/Reserved", func() {
		var nodePool *v1.NodePool
		var nodeClaim *v1.NodeClaim
		var node *corev1.Node
		var driftController *disruption.Controller
		var reservationID string

		// setupReservedOffering appends a reserved offering to mostExpensiveInstance and constrains the NodePool to that
		// instance type. reservationCapacity is the remaining free slots (0 = full) and available reflects whether the
		// reservation is usable for non-capacity reasons — matching the provider model where Available is decoupled from
		// ReservationCapacity (full-but-healthy = available:true, cap:0; something-else-wrong = available:false).
		// capacityTypes are the capacity types the NodePool permits.
		setupReservedOffering := func(reservationCapacity int, available bool, capacityTypes ...string) {
			reservationID = "r-" + mostExpensiveInstance.Name
			mostExpensiveInstance.Requirements.Add(scheduling.NewRequirement(cloudprovider.ReservationIDLabel, corev1.NodeSelectorOpIn, reservationID))
			mostExpensiveInstance.Requirements.Get(v1.CapacityTypeLabelKey).Insert(v1.CapacityTypeReserved)
			mostExpensiveInstance.Offerings = append(mostExpensiveInstance.Offerings, &cloudprovider.Offering{
				Price:               mostExpensiveOffering.Price / 1_000_000.0,
				Available:           available,
				ReservationCapacity: reservationCapacity,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
					corev1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					cloudprovider.ReservationIDLabel: reservationID,
				}),
			})
			ExpectSingletonReconciled(ctx, pricingController)

			nodePool = test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{ConsolidateAfter: v1.MustParseNillableDuration("1h"), Budgets: []v1.Budget{{Nodes: "100%"}}},
				Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{Requirements: []v1.NodeSelectorRequirementWithMinValues{
					{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{mostExpensiveInstance.Name}},
					{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: capacityTypes},
				}}},
			}})
			nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					v1.NodePoolLabelKey:              nodePool.Name,
					corev1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
					corev1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					cloudprovider.ReservationIDLabel: reservationID,
				}},
				Status: v1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32"), corev1.ResourcePods: resource.MustParse("100")},
				},
			})
			nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
			driftController = disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost,
				disruption.WithMethods(disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock)))
		}

		It("issues a delete-only command when the reservation is full and there is no fallback", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true)}}))
			setupReservedOffering(0, true, v1.CapacityTypeReserved) // full but healthy (row 1), reserved-only pool
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			bindReschedulablePod(node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			ExpectSingletonReconciled(ctx, driftController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.TerminateFirstDecision))
			Expect(cmds[0].Replacements).To(HaveLen(0))
			// The delete-only command must carry the pass-2 (credit-back) Results — that's what Record uses to nominate
			// existing nodes for the freed pods. Pin the payload (kills the "drop Results" / "return pass-1" mutations):
			// pass 2 places the pod on the credited reservation as a reserved NodeClaim; pass-1/empty Results would not.
			Expect(cmds[0].Results.NewNodeClaims).To(HaveLen(1))
			Expect(cmds[0].Results.NewNodeClaims[0].Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeReserved)).To(BeTrue())
			Expect(cmds[0].Results.NewNodeClaims[0].Requirements.Get(cloudprovider.ReservationIDLabel).Has(reservationID)).To(BeTrue())
		})

		It("does NOT terminate-first when more reschedulable pods need the reservation than the freed slot can hold", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true)}}))
			setupReservedOffering(0, true, v1.CapacityTypeReserved) // full but healthy, reserved-only pool
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			// Two reschedulable pods that can't co-locate need two nodes, but crediting the candidate's reservation frees
			// only one slot. Pass 2 runs strict, so the surplus pod fails and we must NOT terminate-first — a fallback
			// pass-2 would phantom-schedule both and wrongly terminate-first. Pins strict-vs-fallback in pass 2 (G1).
			bindAntiAffinityPods(node, 2)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			ExpectSingletonReconciled(ctx, driftController)
			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(recorder.Calls(events.DisruptionBlocked)).To(BeNumerically(">", 0)) // the candidate is reported Blocked, not silently skipped
		})

		It("does NOT terminate-first when the reservation is full AND otherwise unavailable (something else wrong)", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true)}}))
			setupReservedOffering(0, false, v1.CapacityTypeReserved) // full AND unavailable (row 2): expiring / ICE'd / incompatible
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			bindReschedulablePod(node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			ExpectSingletonReconciled(ctx, driftController)

			// The offering is unavailable, so the simulation can't stage a reserved replacement and drift is Blocked.
			// Freeing the candidate's slot wouldn't fix an expiring/ICE'd reservation, so we must NOT terminate-first.
			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(recorder.Calls(events.DisruptionBlocked)).To(BeNumerically(">", 0)) // the candidate is reported Blocked, not silently skipped
		})

		It("does NOT drift when TerminateFirstDrift is disabled and the reservation is full with no fallback (Blocked)", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(false), ReservedCapacity: lo.ToPtr(true)}}))
			setupReservedOffering(0, true, v1.CapacityTypeReserved) // full but healthy (row 1), reserved-only pool
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			bindReschedulablePod(node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			ExpectSingletonReconciled(ctx, driftController)

			// With the gate off, drift takes its normal replace-first path. In fallback mode a full reservation no longer
			// yields a phantom reserved-only replacement — it can't stage a replacement, so drift is Blocked and issues
			// no command (matching upstream behavior for a full reserved-only node). Terminate-first is exactly what
			// unblocks this case, and it's gated off here.
			Expect(queue.GetCommands()).To(HaveLen(0))
			Expect(recorder.Calls(events.DisruptionBlocked)).To(BeNumerically(">", 0)) // the candidate is reported Blocked, not silently skipped
		})

		It("replaces-first when the reservation is full but an on-demand fallback exists", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true)}}))
			setupReservedOffering(0, true, v1.CapacityTypeReserved, v1.CapacityTypeOnDemand) // full but healthy, on-demand fallback allowed
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			bindReschedulablePod(node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			ExpectSingletonReconciled(ctx, driftController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.ReplaceDecision))
			Expect(cmds[0].Replacements).To(HaveLen(1))
		})

		It("replaces-first when the reservation still has a spare slot", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true)}}))
			setupReservedOffering(5, true, v1.CapacityTypeReserved) // spare reservation capacity -> can grow in place
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			bindReschedulablePod(node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			ExpectSingletonReconciled(ctx, driftController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.ReplaceDecision))
		})

		// Two weighted NodePools — reserved-only NP1 (higher weight) + on-demand NP2 (lower weight). Drift of the NP1
		// node should fall through to NP2 (replace-first onto on-demand) rather than terminate-first, whenever the OD
		// NodePool can host the pods. This is the two-pass behavior: pass 1 (RequireReservedCapacity) makes NP1's full
		// reservation not satisfy the pod, so the scheduler falls through to NP2.
		setupODFallbackNodePool := func() *v1.NodePool {
			nodePool.Spec.Weight = lo.ToPtr(int32(100))
			od := test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{
				Weight:     lo.ToPtr(int32(1)),
				Disruption: v1.Disruption{ConsolidateAfter: v1.MustParseNillableDuration("1h"), Budgets: []v1.Budget{{Nodes: "100%"}}},
				Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{Requirements: []v1.NodeSelectorRequirementWithMinValues{
					{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}},
				}}},
			}})
			return od
		}

		It("weighted: a full-but-healthy reserved NodePool replaces-first onto a lower-weight on-demand NodePool (does NOT terminate-first)", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true)}}))
			setupReservedOffering(0, true, v1.CapacityTypeReserved) // full but healthy (Available=true, cap=0)
			od := setupODFallbackNodePool()
			ExpectApplied(ctx, env.Client, nodePool, od, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			bindReschedulablePod(node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			ExpectSingletonReconciled(ctx, driftController)
			cmds := queue.GetCommands()
			// Pass 1 (RequireReservedCapacity): NP1's full reservation doesn't satisfy the pod, so it falls through to
			// the lower-weight on-demand NP2 and we replace-first there — no terminate-first when the pool can grow
			// elsewhere.
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.ReplaceDecision))
			Expect(cmds[0].Replacements[0].Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeOnDemand)).To(BeTrue())
		})

		It("weighted: an unavailable reserved NodePool replaces-first onto a lower-weight on-demand NodePool", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true), ReservedCapacity: lo.ToPtr(true)}}))
			setupReservedOffering(0, false, v1.CapacityTypeReserved) // full AND unavailable (Available=false)
			od := setupODFallbackNodePool()
			ExpectApplied(ctx, env.Client, nodePool, od, nodeClaim, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
			bindReschedulablePod(node)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			ExpectSingletonReconciled(ctx, driftController)
			cmds := queue.GetCommands()
			// NP1's reserved offering is unavailable (skipped entirely), so NP1 can't host the pod and it falls through
			// to the lower-weight OD NP2 -> replace-first with an on-demand replacement.
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Decision()).To(Equal(disruption.ReplaceDecision))
			Expect(cmds[0].Replacements[0].Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeOnDemand)).To(BeTrue())
		})
	})
})
