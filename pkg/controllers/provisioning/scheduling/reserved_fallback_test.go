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

package scheduling_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	pscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
)

// These specs pin down offeringsToReserve's handling of a reserved offering whose ReservationCapacity is 0 once
// reservation capacity and offering availability are decoupled at the provider (a full-but-healthy reservation is
// Available=true, ReservationCapacity=0). The invariant under test: a full reservation is treated as non-reservable
// (skipped), exactly as an unavailable offering would be — which (a) preserves strict/provisioning fall-back behavior,
// (b) keeps normal reserved scheduling working when capacity remains, (c) still respects Available=false regardless of
// capacity, and (d) in fallback mode leaves the reserved-only NodeClaim intact (which is what disruption terminate-first
// keys off of).
var _ = Describe("Reserved Instance Types/Full-but-available (capacity-availability decoupled)", func() {
	var nodePool *v1.NodePool
	// reservedOffering builds a reserved offering in test-zone-1 with the given availability and remaining capacity.
	reservedOffering := func(id string, available bool, capacity int) *cloudprovider.Offering {
		return &cloudprovider.Offering{
			Available:           available,
			ReservationCapacity: capacity,
			Price:               0.001,
			Requirements: pscheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:     v1.CapacityTypeReserved,
				corev1.LabelTopologyZone:    "test-zone-1",
				v1alpha1.LabelReservationID: id,
			}),
		}
	}
	// newReservedType builds a fake instance type (keeps its default on-demand/spot offerings) that also advertises the
	// given reserved offering.
	newReservedType := func(reserved *cloudprovider.Offering) *cloudprovider.InstanceType {
		it := fake.NewInstanceType("reserved-type", fake.WithResources(map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		}))
		it.Requirements.Get(v1.CapacityTypeLabelKey).Insert(v1.CapacityTypeReserved)
		it.Offerings = append(it.Offerings, reserved)
		return it
	}
	reservedOnlyNodePool := func(instanceTypeName string) *v1.NodePool {
		return test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
			Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{instanceTypeName}},
				{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeReserved}},
			},
		}}}})
	}
	smallPod := func() *corev1.Pod {
		return test.UnschedulablePod(test.PodOptions{ResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		}})
	}
	// solveFallback runs the scheduler in the default (fallback) ReservedOfferingMode — the mode the disruption
	// simulation uses — and returns the results.
	solveFallback := func(pods ...*corev1.Pod) scheduling.Results {
		GinkgoHelper()
		s, err := prov.NewScheduler(ctx, pods, nil, nil)
		Expect(err).ToNot(HaveOccurred())
		results, err := s.Solve(injection.WithControllerName(ctx, "provisioner"), pods)
		Expect(err).ToNot(HaveOccurred())
		return results
	}

	BeforeEach(func() {
		cloudProvider.Reset()
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{ReservedCapacity: lo.ToPtr(true)}}))
	})

	It("fallback: a full reservation with no fallback yields no NodeClaim (pod falls through / pends, not a phantom reserved NodeClaim)", func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{newReservedType(reservedOffering("r-full", true, 0))}
		nodePool = reservedOnlyNodePool("reserved-type")
		ExpectApplied(ctx, env.Client, nodePool)

		pod := smallPod()
		results := solveFallback(pod)

		// Fallback mode used to build a reserved-only NodeClaim holding no reservation (a phantom that would ICE at
		// launch). Now a full reservation with no fallback fails with a plain error, so the pod falls through — and with
		// no other NodePool it simply pends. This mirrors the pre-decouple behavior when a full reservation was
		// Available=false and was never offered here. It is NOT a ReservedOfferingError (that would poison fallthrough).
		Expect(results.NewNodeClaims).To(HaveLen(0))
		Expect(results.PodErrors).To(HaveKey(pod))
		Expect(results.ReservedOfferingErrors()).ToNot(HaveKey(pod))
	})

	It("fallback: a reservation that still has capacity is reserved normally (regression guard)", func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{newReservedType(reservedOffering("r-has-cap", true, 3))}
		nodePool = reservedOnlyNodePool("reserved-type")
		ExpectApplied(ctx, env.Client, nodePool)

		results := solveFallback(smallPod())

		Expect(results.NewNodeClaims).To(HaveLen(1))
		nc := results.NewNodeClaims[0]
		Expect(nc.Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeReserved)).To(BeTrue())
		// A reservable offering IS held, so the reservation id is pinned.
		Expect(nc.Requirements.Get(v1alpha1.LabelReservationID).Values()).To(ConsistOf("r-has-cap"))
	})

	It("a reservation marked unavailable is skipped even when it reports capacity (Available=false respected)", func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{newReservedType(reservedOffering("r-unavail", false, 5))}
		nodePool = reservedOnlyNodePool("reserved-type")
		ExpectApplied(ctx, env.Client, nodePool)

		pod := smallPod()
		results := solveFallback(pod)

		// The only offering for a reserved-only pool is unavailable, so the pod cannot be placed on a new NodeClaim.
		Expect(results.NewNodeClaims).To(HaveLen(0))
		Expect(results.PodErrors).To(HaveKey(pod))
	})

	It("strict (provisioning): a full-but-available reservation does not block scheduling and holds no reservation", func() {
		// A mixed NodePool (allows reserved + on-demand/spot). The instance type's reservation is full but Available.
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{newReservedType(reservedOffering("r-full", true, 0))}
		nodePool = test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
			Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"reserved-type"}},
			},
		}}}})
		ExpectApplied(ctx, env.Client, nodePool)

		// Strict mode (the provisioning path). A full reservation must NOT raise a blocking ReservedOfferingError:
		// the pod schedules, and the NodeClaim holds no reservation (it can launch on-demand/spot). We assert on the
		// scheduling decision rather than the launched node's capacity-type, because the fake cloudprovider's launch
		// still picks the cheapest Available offering (the cap=0 reserved one) — the real provider's launch filter
		// skips full reservations, but that is a provider-side concern, not the scheduler's.
		pod := smallPod()
		s, err := prov.NewScheduler(ctx, []*corev1.Pod{pod}, nil, nil, scheduling.DisableReservedCapacityFallback)
		Expect(err).ToNot(HaveOccurred())
		results, err := s.Solve(injection.WithControllerName(ctx, "provisioner"), []*corev1.Pod{pod})
		Expect(err).ToNot(HaveOccurred())
		Expect(results.PodErrors).ToNot(HaveKey(pod)) // not blocked by a ReservedOfferingError
		Expect(results.NewNodeClaims).To(HaveLen(1))
		// No reservation was held (it was full), so the NodeClaim wasn't pinned to reserved.
		Expect(results.NewNodeClaims[0].Requirements.Get(v1alpha1.LabelReservationID).Values()).To(BeEmpty())
		Expect(results.NewNodeClaims[0].Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeOnDemand)).To(BeTrue())
	})

	It("strict (provisioning): a reserved-only pool with a full reservation defers instead of launching on-demand", func() {
		// A reserved-ONLY NodePool (capacity-type In [reserved], no on-demand/spot fallback) whose reservation is full
		// but Available. There is no non-reserved fallback, so strict must defer (ReservedOfferingError) rather than
		// create a NodeClaim that would be launched on-demand — this is the reserved-only mislaunch guard.
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{newReservedType(reservedOffering("r-full", true, 0))}
		nodePool = test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
			Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"reserved-type"}},
				{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeReserved}},
			},
		}}}})
		ExpectApplied(ctx, env.Client, nodePool)

		pod := smallPod()
		s, err := prov.NewScheduler(ctx, []*corev1.Pod{pod}, nil, nil, scheduling.DisableReservedCapacityFallback)
		Expect(err).ToNot(HaveOccurred())
		results, err := s.Solve(injection.WithControllerName(ctx, "provisioner"), []*corev1.Pod{pod})
		Expect(err).ToNot(HaveOccurred())
		// No on-demand mislaunch: no NodeClaim is created and the pod defers via a (retryable) ReservedOfferingError.
		Expect(results.NewNodeClaims).To(HaveLen(0))
		Expect(results.ReservedOfferingErrors()).To(HaveKey(pod))
	})

	It("strict (provisioning): a reservation with capacity is preferred over on-demand (regression guard)", func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{newReservedType(reservedOffering("r-has-cap", true, 1))}
		nodePool = test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
			Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"reserved-type"}},
			},
		}}}})
		ExpectApplied(ctx, env.Client, nodePool)

		bindings := ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, smallPod())
		Expect(bindings.Bindings).To(HaveLen(1))
		node := lo.Values(bindings.Bindings)[0].Node
		Expect(node.Labels).To(HaveKeyWithValue(v1.CapacityTypeLabelKey, v1.CapacityTypeReserved))
		Expect(node.Labels).To(HaveKeyWithValue(cloudprovider.ReservationIDLabel, "r-has-cap"))
	})

	// The two knobs the terminate-first disruption passes use. onDemandNodePool builds a lower-weight on-demand pool on
	// the same instance type (which keeps its default on-demand offerings) so we can observe cross-NodePool fallthrough.
	onDemandNodePool := func(instanceTypeName string) *v1.NodePool {
		return test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{
			Weight: lo.ToPtr(int32(1)),
			Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{instanceTypeName}},
				{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}},
			}}},
		}})
	}
	solveWith := func(pods []*corev1.Pod, opts ...scheduling.Options) scheduling.Results {
		GinkgoHelper()
		s, err := prov.NewScheduler(ctx, pods, nil, nil, opts...)
		Expect(err).ToNot(HaveOccurred())
		results, err := s.Solve(injection.WithControllerName(ctx, "provisioner"), pods)
		Expect(err).ToNot(HaveOccurred())
		return results
	}

	Context("fallback: full reservation falls through across NodePools", func() {
		It("falls through to a lower-weight on-demand NodePool when the higher-weight reserved NodePool's reservation is full", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{newReservedType(reservedOffering("r-full", true, 0))}
			reserved := reservedOnlyNodePool("reserved-type")
			reserved.Spec.Weight = lo.ToPtr(int32(100))
			od := onDemandNodePool("reserved-type")
			ExpectApplied(ctx, env.Client, reserved, od)

			pod := smallPod()
			results := solveWith([]*corev1.Pod{pod}) // default fallback mode

			// In fallback mode the full reservation doesn't satisfy the pod (plain error), so it falls through to the
			// lower-weight on-demand NodePool — the same fallthrough that happened pre-decouple via Available=false.
			Expect(results.PodErrors).ToNot(HaveKey(pod))
			Expect(results.NewNodeClaims).To(HaveLen(1))
			Expect(results.NewNodeClaims[0].Requirements.Get(v1.CapacityTypeLabelKey).Has(v1.CapacityTypeOnDemand)).To(BeTrue())
		})
	})

	Context("CreditReservationCapacity (terminate-first feasibility pass)", func() {
		It("schedules onto a full reservation once a slot is credited back (strict)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{newReservedType(reservedOffering("r-full", true, 0))}
			ExpectApplied(ctx, env.Client, reservedOnlyNodePool("reserved-type"))

			pod := smallPod()
			results := solveWith([]*corev1.Pod{pod}, scheduling.DisableReservedCapacityFallback, scheduling.CreditReservationCapacity("r-full", 1))

			Expect(results.PodErrors).ToNot(HaveKey(pod))
			Expect(results.NewNodeClaims).To(HaveLen(1))
			Expect(results.NewNodeClaims[0].Requirements.Get(v1alpha1.LabelReservationID).Values()).To(ConsistOf("r-full"))
		})

		It("credits only the granted number of slots — a surplus pod still fails under strict (multi-pod/single-slot)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{newReservedType(reservedOffering("r-full", true, 0))}
			// Anti-affinity forces each pod onto its own node, so two pods need two reserved slots.
			antiAffinityPod := func() *corev1.Pod {
				return test.UnschedulablePod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "spread"}},
					PodAntiRequirements: []corev1.PodAffinityTerm{{
						TopologyKey:   corev1.LabelHostname,
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "spread"}},
					}},
				})
			}
			ExpectApplied(ctx, env.Client, reservedOnlyNodePool("reserved-type"))

			p1, p2 := antiAffinityPod(), antiAffinityPod()
			results := solveWith([]*corev1.Pod{p1, p2}, scheduling.DisableReservedCapacityFallback, scheduling.CreditReservationCapacity("r-full", 1))

			// Exactly one pod gets the single credited slot; the other has nowhere to go and fails.
			Expect(results.NewNodeClaims).To(HaveLen(1))
			scheduled := lo.Filter([]*corev1.Pod{p1, p2}, func(p *corev1.Pod, _ int) bool { _, pending := results.PodErrors[p]; return !pending })
			Expect(scheduled).To(HaveLen(1))
		})
	})
})
