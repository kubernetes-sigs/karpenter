/*
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

package nodeclaim_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/test"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodeClaimUtils")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("NodeClaimUtils", func() {
	var node *v1.Node
	BeforeEach(func() {
		node = test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.LabelTopologyZone:             "test-zone-1",
					v1.LabelTopologyRegion:           "test-region",
					"test-label-key":                 "test-label-value",
					"test-label-key2":                "test-label-value2",
					v1alpha5.LabelNodeRegistered:     "true",
					v1alpha5.LabelNodeInitialized:    "true",
					v1alpha5.ProvisionerNameLabelKey: "default",
					v1alpha5.LabelCapacityType:       v1alpha5.CapacityTypeOnDemand,
					v1.LabelOSStable:                 "linux",
					v1.LabelInstanceTypeStable:       "test-instance-type",
				},
				Annotations: map[string]string{
					"test-annotation-key":        "test-annotation-value",
					"test-annotation-key2":       "test-annotation-value2",
					"node-custom-annotation-key": "node-custom-annotation-value",
				},
			},
			ReadyStatus: v1.ConditionTrue,
			Taints: []v1.Taint{
				{
					Key:    "test-taint-key",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "test-taint-value",
				},
				{
					Key:    "test-taint-key2",
					Effect: v1.TaintEffectNoExecute,
					Value:  "test-taint-value2",
				},
			},
			ProviderID: test.RandomProviderID(),
			Capacity: v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("10"),
				v1.ResourceMemory:           resource.MustParse("10Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("8"),
				v1.ResourceMemory:           resource.MustParse("8Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("95Gi"),
			},
		})
	})
	It("should convert a Node to a NodeClaim", func() {
		nodeClaim := nodeclaimutil.NewFromNode(node)
		for k, v := range node.Annotations {
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(k, v))
		}
		for k, v := range node.Labels {
			Expect(nodeClaim.Labels).To(HaveKeyWithValue(k, v))
		}
		Expect(lo.Contains(nodeClaim.Finalizers, v1beta1.TerminationFinalizer)).To(BeTrue())
		Expect(nodeClaim.Spec.Taints).To(Equal(node.Spec.Taints))
		Expect(nodeClaim.Spec.Requirements).To(ContainElements(scheduling.NewLabelRequirements(node.Labels).NodeSelectorRequirements()))
		Expect(nodeClaim.Spec.Resources.Requests).To(Equal(node.Status.Allocatable))
		Expect(nodeClaim.Status.NodeName).To(Equal(node.Name))
		Expect(nodeClaim.Status.ProviderID).To(Equal(node.Spec.ProviderID))
		Expect(nodeClaim.Status.Capacity).To(Equal(node.Status.Capacity))
		Expect(nodeClaim.Status.Allocatable).To(Equal(node.Status.Allocatable))
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Launched).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Registered).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Initialized).IsTrue()).To(BeTrue())
	})
	It("should retrieve a NodeClaim with a get call", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClassRef: &v1beta1.NodeClassReference{
					Kind:       "NodeClassRef",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		retrieved, err := nodeclaimutil.Get(ctx, env.Client, nodeClaim.Name)
		Expect(err).ToNot(HaveOccurred())
		Expect(retrieved.Name).To(Equal(nodeClaim.Name))
	})
	It("should update the status on a NodeClaim", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClassRef: &v1beta1.NodeClassReference{
					Kind:       "NodeClassRef",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		providerID := test.RandomProviderID()
		nodeClaim.Status.ProviderID = providerID
		Expect(nodeclaimutil.UpdateStatus(ctx, env.Client, nodeClaim)).To(Succeed())

		retrieved := &v1beta1.NodeClaim{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodeClaim), retrieved)).To(Succeed())
		Expect(retrieved.Status.ProviderID).To(Equal(providerID))
	})
	It("should patch a NodeClaim", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClassRef: &v1beta1.NodeClassReference{
					Kind:       "NodeClassRef",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		stored := nodeClaim.DeepCopy()
		nodeClaim.Labels = lo.Assign(nodeClaim.Labels, map[string]string{
			"custom-key": "custom-value",
		})
		Expect(nodeclaimutil.Patch(ctx, env.Client, stored, nodeClaim)).To(Succeed())

		retrieved := &v1beta1.NodeClaim{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodeClaim), retrieved)).To(Succeed())
		Expect(retrieved.Labels).To(HaveKeyWithValue("custom-key", "custom-value"))
	})
	It("should patch the status on a NodeClaim", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClassRef: &v1beta1.NodeClassReference{
					Kind:       "NodeClassRef",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		stored := nodeClaim.DeepCopy()
		providerID := test.RandomProviderID()
		nodeClaim.Status.ProviderID = providerID
		Expect(nodeclaimutil.PatchStatus(ctx, env.Client, stored, nodeClaim)).To(Succeed())

		retrieved := &v1beta1.NodeClaim{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodeClaim), retrieved)).To(Succeed())
		Expect(retrieved.Status.ProviderID).To(Equal(providerID))
	})
	It("should delete a NodeClaim with a delete call", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClassRef: &v1beta1.NodeClassReference{
					Kind:       "NodeClassRef",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		err := nodeclaimutil.Delete(ctx, env.Client, nodeClaim)
		Expect(err).ToNot(HaveOccurred())

		nodeClaimList := &v1beta1.NodeClaimList{}
		Expect(env.Client.List(ctx, nodeClaimList)).To(Succeed())
		Expect(nodeClaimList.Items).To(HaveLen(0))
		Expect(errors.IsNotFound(env.Client.Get(ctx, client.ObjectKeyFromObject(nodeClaim), &v1beta1.NodeClaim{}))).To(BeTrue())
	})
	It("should update the owner for a Node to a NodeClaim", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClassRef: &v1beta1.NodeClassReference{
					Kind:       "NodeClassRef",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		node = test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		node = nodeclaimutil.UpdateNodeOwnerReferences(nodeClaim, node)

		Expect(lo.Contains(node.OwnerReferences, metav1.OwnerReference{
			APIVersion:         lo.Must(apiutil.GVKForObject(nodeClaim, scheme.Scheme)).GroupVersion().String(),
			Kind:               lo.Must(apiutil.GVKForObject(nodeClaim, scheme.Scheme)).String(),
			Name:               nodeClaim.Name,
			UID:                nodeClaim.UID,
			BlockOwnerDeletion: lo.ToPtr(true),
		}))
	})
	It("should retrieve the owner for a NodeClaim", func() {
		nodePool := test.NodePool(v1beta1.NodePool{
			Spec: v1beta1.NodePoolSpec{
				Template: v1beta1.NodeClaimTemplate{
					Spec: v1beta1.NodeClaimSpec{
						NodeClassRef: &v1beta1.NodeClassReference{
							Kind:       "NodeClassRef",
							APIVersion: "test.cloudprovider/v1",
							Name:       "default",
						},
					},
				},
			},
		})
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				NodeClassRef: &v1beta1.NodeClassReference{
					Kind:       "NodeClassRef",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		ownerKey := nodeclaimutil.OwnerKey(nodeClaim)
		Expect(ownerKey.Name).To(Equal(nodePool.Name))
		Expect(ownerKey.IsProvisioner).To(BeFalse())

		owner, err := nodeclaimutil.Owner(ctx, env.Client, nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		Expect(owner.Name).To(Equal(nodePool.Name))
	})
})
