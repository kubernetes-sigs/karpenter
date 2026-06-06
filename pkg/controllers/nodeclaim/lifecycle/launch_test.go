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

package lifecycle_test

import (
	"fmt"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	nodeclaimlifecycle "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	testv1alpha1 "sigs.k8s.io/karpenter/pkg/test/v1alpha1"
)

// fakeOverlayStore is a test double for overlayStore, used in overlay annotation tests
// to avoid exporting test-only helpers from the production store package.
type fakeOverlayStore struct {
	// key: "nodePool/instanceType/zone/capacityType/reservationID"
	priceEntries    map[string]fakePriceEntry
	capacityEntries map[string]string // key: "nodePool/instanceType" -> overlayName
}

type fakePriceEntry struct {
	overlayName   string
	adjustedPrice float64
}

func newFakeOverlayStore() *fakeOverlayStore {
	return &fakeOverlayStore{
		priceEntries:    map[string]fakePriceEntry{},
		capacityEntries: map[string]string{},
	}
}

func (f *fakeOverlayStore) withPrice(nodePool, instanceType, zone, capacityType, reservationID, overlayName string, adjustedPrice float64) *fakeOverlayStore { //nolint:unparam
	key := nodePool + "/" + instanceType + "/" + zone + "/" + capacityType + "/" + reservationID
	f.priceEntries[key] = fakePriceEntry{overlayName: overlayName, adjustedPrice: adjustedPrice}
	return f
}

func (f *fakeOverlayStore) withCapacity(nodePool, instanceType, overlayName string) *fakeOverlayStore { //nolint:unparam
	f.capacityEntries[nodePool+"/"+instanceType] = overlayName
	return f
}

func (f *fakeOverlayStore) PriceOverlayForOffering(nodePool, instanceType, zone, capacityType, reservationID string) (string, float64, bool) {
	key := nodePool + "/" + instanceType + "/" + zone + "/" + capacityType + "/" + reservationID
	e, ok := f.priceEntries[key]
	return e.overlayName, e.adjustedPrice, ok
}

func (f *fakeOverlayStore) CapacityOverlayName(nodePool, instanceType string) (string, bool) {
	name, ok := f.capacityEntries[nodePool+"/"+instanceType]
	return name, ok
}

var _ = Describe("Launch", func() {
	var nodePool *v1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool()
	})
	DescribeTable(
		"Launch",
		func(isNodeClaimManaged bool) {
			nodeClaimOpts := []v1.NodeClaim{{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey: nodePool.Name,
					},
				},
			}}
			if !isNodeClaimManaged {
				nodeClaimOpts = append(nodeClaimOpts, v1.NodeClaim{
					Spec: v1.NodeClaimSpec{
						NodeClassRef: &v1.NodeClassReference{
							Group: "karpenter.test.sh",
							Kind:  "UnmanagedNodeClass",
							Name:  "default",
						},
					},
				})
			}
			nodeClaim := test.NodeClaim(nodeClaimOpts...)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

			Expect(cloudProvider.CreateCalls).To(HaveLen(lo.Ternary(isNodeClaimManaged, 1, 0)))
			Expect(cloudProvider.CreatedNodeClaims).To(HaveLen(lo.Ternary(isNodeClaimManaged, 1, 0)))
			if isNodeClaimManaged {
				_, err := cloudProvider.Get(ctx, nodeClaim.Status.ProviderID)
				Expect(err).ToNot(HaveOccurred())
			}
		},
		Entry("should launch an instance when a new NodeClaim is created", true),
		Entry("should ignore NodeClaims which aren't managed by this Karpenter instance", false),
	)
	It("should add the Launched status condition after creating the NodeClaim", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1.ConditionTypeLaunched).Status).To(Equal(metav1.ConditionTrue))
	})
	It("should delete the nodeclaim if InsufficientCapacity is returned from the cloudprovider", func() {
		cloudProvider.NextCreateErr = cloudprovider.NewInsufficientCapacityError(fmt.Errorf("all instance types were unavailable"))
		nodeClaim := test.NodeClaim()
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
		ExpectNotFound(ctx, env.Client, nodeClaim)
	})
	It("should delete the nodeclaim if NodeClassNotReady is returned from the cloudprovider", func() {
		cloudProvider.NextCreateErr = cloudprovider.NewNodeClassNotReadyError(fmt.Errorf("nodeClass isn't ready"))
		nodeClaim := test.NodeClaim()
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
		ExpectNotFound(ctx, env.Client, nodeClaim)
	})
	It("should set nodeClaim status condition from the condition message received if error returned is CreateError", func() {
		conditionReason := "CustomReason"
		conditionMessage := "instance creation failed"
		cloudProvider.NextCreateErr = cloudprovider.NewCreateError(fmt.Errorf("error launching instance"), conditionReason, conditionMessage)
		nodeClaim := test.NodeClaim()
		ExpectApplied(ctx, env.Client, nodeClaim)
		_ = ExpectObjectReconcileFailed(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		condition := ExpectStatusConditionExists(nodeClaim, v1.ConditionTypeLaunched)
		Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
		Expect(condition.Reason).To(Equal(conditionReason))
		Expect(condition.Message).To(Equal(conditionMessage))
	})
})

var _ = Describe("Launch overlay annotations", func() {
	var nodePool *v1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool()
	})

	It("should annotate price overlay name and adjusted price when a price overlay applies to the launched offering", func() {
		store := newFakeOverlayStore().withPrice(nodePool.Name, "default-instance-type", "test-zone-1", "spot", "", "my-price-overlay", 0.50)
		ctrl := nodeclaimlifecycle.NewController(env.Clock, env.Client, cloudProvider, recorder, npState, nil, store)

		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name},
			},
			Spec: v1.NodeClaimSpec{
				Requirements: []v1.NodeSelectorRequirementWithMinValues{
					{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"default-instance-type"}},
					{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"spot"}},
					{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-zone-1"}},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, ctrl, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Annotations[v1alpha1.PriceOverlayAppliedAnnotationKey]).To(Equal("my-price-overlay"))
		adjustedPrice, err := strconv.ParseFloat(nodeClaim.Annotations[v1alpha1.PriceOverlayAdjustedPriceAnnotationKey], 64)
		Expect(err).NotTo(HaveOccurred())
		Expect(adjustedPrice).To(BeNumerically("==", 0.50))
	})

	It("should annotate capacity overlay name when a capacity overlay applies", func() {
		store := newFakeOverlayStore().withCapacity(nodePool.Name, "default-instance-type", "my-capacity-overlay")
		ctrl := nodeclaimlifecycle.NewController(env.Clock, env.Client, cloudProvider, recorder, npState, nil, store)

		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name},
			},
			Spec: v1.NodeClaimSpec{
				Requirements: []v1.NodeSelectorRequirementWithMinValues{
					{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"default-instance-type"}},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, ctrl, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Annotations[v1alpha1.CapacityOverlayAppliedAnnotationKey]).To(Equal("my-capacity-overlay"))
	})

	It("should annotate the correct price overlay for a reserved offering distinguished by reservation ID", func() {
		// Two reserved offerings in the same zone/capacityType but different reservation IDs and different overlay prices.
		store := newFakeOverlayStore().
			withPrice(nodePool.Name, "default-instance-type", "test-zone-1", "reserved", "res-aaa", "overlay-aaa", 0.30).
			withPrice(nodePool.Name, "default-instance-type", "test-zone-1", "reserved", "res-bbb", "overlay-bbb", 0.60)
		ctrl := nodeclaimlifecycle.NewController(env.Clock, env.Client, cloudProvider, recorder, npState, nil, store)

		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:                     nodePool.Name,
					corev1.LabelInstanceTypeStable:          "default-instance-type",
					corev1.LabelTopologyZone:                "test-zone-1",
					v1.CapacityTypeLabelKey:                 "reserved",
					testv1alpha1.LabelReservationID:         "res-bbb",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, ctrl, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Annotations[v1alpha1.PriceOverlayAppliedAnnotationKey]).To(Equal("overlay-bbb"))
		adjustedPrice, err := strconv.ParseFloat(nodeClaim.Annotations[v1alpha1.PriceOverlayAdjustedPriceAnnotationKey], 64)
		Expect(err).NotTo(HaveOccurred())
		Expect(adjustedPrice).To(BeNumerically("==", 0.60))
	})

	It("should not set price overlay annotations when no price overlay applies", func() {
		ctrl := nodeclaimlifecycle.NewController(env.Clock, env.Client, cloudProvider, recorder, npState, nil, nil)

		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, ctrl, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Annotations).NotTo(HaveKey(v1alpha1.PriceOverlayAppliedAnnotationKey))
		Expect(nodeClaim.Annotations).NotTo(HaveKey(v1alpha1.PriceOverlayAdjustedPriceAnnotationKey))
	})

	It("should not re-annotate on second reconcile once launched", func() {
		store := newFakeOverlayStore().withPrice(nodePool.Name, "default-instance-type", "test-zone-1", "spot", "", "my-price-overlay", 0.50)
		ctrl := nodeclaimlifecycle.NewController(env.Clock, env.Client, cloudProvider, recorder, npState, nil, store)

		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name},
			},
			Spec: v1.NodeClaimSpec{
				Requirements: []v1.NodeSelectorRequirementWithMinValues{
					{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"default-instance-type"}},
					{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"spot"}},
					{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-zone-1"}},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, ctrl, nodeClaim)
		// Second reconcile — should return early since already launched; annotations already set
		ExpectObjectReconciled(ctx, env.Client, ctrl, nodeClaim)
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Annotations[v1alpha1.PriceOverlayAppliedAnnotationKey]).To(Equal("my-price-overlay"))
	})
})
