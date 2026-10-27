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

package overlay_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const hugePages2Mi corev1.ResourceName = "hugepages-2Mi"

// rawStatusCloudProvider reports the capacity and allocatable of the instance type with NodeOverlays applied, as they
// are, in the status that Create returns. The AWS provider does the same.
type rawStatusCloudProvider struct {
	*fake.CloudProvider
}

func (r *rawStatusCloudProvider) Create(ctx context.Context, nodeClaim *v1.NodeClaim) (*v1.NodeClaim, error) {
	created, err := r.CloudProvider.Create(ctx, nodeClaim)
	if err != nil {
		return nil, err
	}
	it, err := store.Apply(nodeClaim.Labels[v1.NodePoolLabelKey], r.InstanceTypes[0])
	if err != nil {
		return nil, err
	}
	created.Status.Capacity = it.Capacity
	created.Status.Allocatable = it.Allocatable()
	return created, nil
}

func hugePagesOverlay(capacity, maxCapacity corev1.ResourceList) *v1alpha1.NodeOverlay {
	return test.NodeOverlay(v1alpha1.NodeOverlay{
		Spec: v1alpha1.NodeOverlaySpec{
			Requirements: []v1alpha1.NodeSelectorRequirement{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"default-instance-type"}},
			},
			Capacity:    capacity,
			MaxCapacity: maxCapacity,
		},
	})
}

var _ = Describe("Flexible Capacity", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var decorated cloudprovider.CloudProvider
	var gatedCtx context.Context
	BeforeEach(func() {
		fakeCloudProvider.Reset()
		store.Reset()
		fakeCloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("default-instance-type")}
		decorated = overlay.Decorate(&rawStatusCloudProvider{CloudProvider: fakeCloudProvider}, env.Client, store)
		gatedCtx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeOverlay: lo.ToPtr(true)}}))

		nodePool = test.NodePool()
		nodeClaim = test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      map[string]string{v1.NodePoolLabelKey: nodePool.Name},
				Annotations: map[string]string{v1.FlexibleCapacityAnnotationKey: `{"hugepages-2Mi":"1Gi"}`},
			},
			Spec: v1.NodeClaimSpec{
				Requirements: []v1.NodeSelectorRequirementWithMinValues{
					{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"default-instance-type"}},
				},
			},
		})
	})
	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
	})

	// overlaidMemory returns the allocatable memory of the instance type with NodeOverlays applied
	overlaidMemory := func() int64 {
		GinkgoHelper()
		it, err := store.Apply(nodePool.Name, fakeCloudProvider.InstanceTypes[0])
		Expect(err).ToNot(HaveOccurred())
		allocatable := it.Allocatable()
		return allocatable.Memory().Value()
	}

	It("should report the chosen amount instead of the ceiling and take it out of allocatable memory", func() {
		ExpectApplied(ctx, env.Client, nodePool, hugePagesOverlay(nil, corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")}))
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		created, err := decorated.Create(gatedCtx, nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		Expect(created.Status.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("1Gi")))
		Expect(created.Status.Allocatable).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("1Gi")))
		oneGi := resource.MustParse("1Gi")
		Expect(created.Status.Allocatable.Memory().Value()).To(Equal(overlaidMemory() - oneGi.Value()))
	})
	It("should remove the ceiling when no amount was chosen", func() {
		ExpectApplied(ctx, env.Client, nodePool, hugePagesOverlay(nil, corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")}))
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})
		delete(nodeClaim.Annotations, v1.FlexibleCapacityAnnotationKey)

		created, err := decorated.Create(gatedCtx, nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		Expect(created.Status.Capacity).ToNot(HaveKey(hugePages2Mi))
		Expect(created.Status.Allocatable).ToNot(HaveKey(hugePages2Mi))
		Expect(created.Status.Allocatable.Memory().Value()).To(Equal(overlaidMemory()))
	})
	It("should not change the status of a fixed capacity overlay", func() {
		ExpectApplied(ctx, env.Client, nodePool, hugePagesOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")}, nil))
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		created, err := decorated.Create(gatedCtx, nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		Expect(created.Status.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("8Gi")))
		Expect(created.Status.Allocatable).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("8Gi")))
		Expect(created.Status.Allocatable.Memory().Value()).To(Equal(overlaidMemory()))
	})
	It("should not change the status when the NodeOverlay feature gate is off", func() {
		ExpectApplied(ctx, env.Client, nodePool, hugePagesOverlay(nil, corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")}))
		ExpectReconciled(ctx, nodeOverlayController, reconcile.Request{})

		created, err := decorated.Create(options.ToContext(ctx, test.Options()), nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		Expect(created.Status.Capacity).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("8Gi")))
	})
})
