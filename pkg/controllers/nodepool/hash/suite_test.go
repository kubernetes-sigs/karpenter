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

package hash_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/nodepool/hash"
	"sigs.k8s.io/karpenter/pkg/operator/controller"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
	"sigs.k8s.io/karpenter/pkg/test"
)

var nodePoolController controller.Controller
var ctx context.Context
var env *test.Environment
var cp *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Hash")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	cp = fake.NewCloudProvider()
	nodePoolController = hash.NewController(env.Client, cp)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Static Drift Hash", func() {
	var nodePool *v1beta1.NodePool
	BeforeEach(func() {
		cp.Reset()
		nodePool = test.NodePool(v1beta1.NodePool{
			Spec: v1beta1.NodePoolSpec{
				Template: v1beta1.NodeClaimTemplate{
					ObjectMeta: v1beta1.ObjectMeta{
						Annotations: map[string]string{
							"keyAnnotation":  "valueAnnotation",
							"keyAnnotation2": "valueAnnotation2",
						},
						Labels: map[string]string{
							"keyLabel": "valueLabel",
						},
					},
					Spec: v1beta1.NodeClaimSpec{
						Taints: []v1.Taint{
							{
								Key:    "key",
								Effect: v1.TaintEffectNoExecute,
							},
						},
						StartupTaints: []v1.Taint{
							{
								Key:    "key",
								Effect: v1.TaintEffectNoExecute,
							},
						},
						Kubelet: &v1beta1.KubeletConfiguration{
							MaxPods: ptr.Int32(10),
						},
					},
				},
			},
		})
	})
	It("should update the drift hash when NodePool static field is updated", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		expectedHash := nodePool.Hash()
		Expect(nodePool.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashAnnotationKey, expectedHash))

		nodePool.Spec.Template.Labels = map[string]string{"keyLabeltest": "valueLabeltest"}
		nodePool.Spec.Template.Annotations = map[string]string{"keyAnnotation2": "valueAnnotation2", "keyAnnotation": "valueAnnotation"}
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		expectedHashTwo := nodePool.Hash()
		Expect(nodePool.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashAnnotationKey, expectedHashTwo))
	})
	It("should not update the drift hash when NodePool behavior field is updated", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		expectedHash := nodePool.Hash()
		Expect(nodePool.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashAnnotationKey, expectedHash))

		nodePool.Spec.Limits = v1beta1.Limits(v1.ResourceList{"cpu": resource.MustParse("16")})
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenEmpty
		nodePool.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{Duration: lo.ToPtr(30 * time.Second)}
		nodePool.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(30 * time.Second)
		nodePool.Spec.Template.Spec.Requirements = []v1beta1.NodeSelectorRequirementWithMinValues{
			{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
			{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1"}}},
			{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}}},
		}
		nodePool.Spec.Weight = lo.ToPtr(int32(80))
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		Expect(nodePool.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashAnnotationKey, expectedHash))
	})
	It("should update nodepool hash version when the nodepool hash version is out of sync with the controller hash version", func() {
		nodePool.Annotations = map[string]string{
			v1beta1.NodePoolHashAnnotationKey:        "abceduefed",
			v1beta1.NodePoolHashVersionAnnotationKey: "test",
		}
		ExpectApplied(ctx, env.Client, nodePool)

		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		expectedHash := nodePool.Hash()
		Expect(nodePool.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashAnnotationKey, expectedHash))
		Expect(nodePool.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashVersionAnnotationKey, v1beta1.NodePoolHashVersion))
	})
	It("should update nodepool hash versions on all nodeclaims when the hash versions don't match the controller hash version", func() {
		nodePool.Annotations = map[string]string{
			v1beta1.NodePoolHashAnnotationKey:        "abceduefed",
			v1beta1.NodePoolHashVersionAnnotationKey: "test",
		}
		nodeClaimOne := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1beta1.NodePoolLabelKey: nodePool.Name},
				Annotations: map[string]string{
					v1beta1.NodePoolHashAnnotationKey:        "123456",
					v1beta1.NodePoolHashVersionAnnotationKey: "test",
				},
			},
		})
		nodeClaimTwo := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1beta1.NodePoolLabelKey: nodePool.Name},
				Annotations: map[string]string{
					v1beta1.NodePoolHashAnnotationKey:        "123456",
					v1beta1.NodePoolHashVersionAnnotationKey: "test",
				},
			},
		})

		ExpectApplied(ctx, env.Client, nodePool, nodeClaimOne, nodeClaimTwo)

		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)
		nodeClaimOne = ExpectExists(ctx, env.Client, nodeClaimOne)
		nodeClaimTwo = ExpectExists(ctx, env.Client, nodeClaimTwo)

		expectedHash := nodePool.Hash()
		Expect(nodeClaimOne.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashAnnotationKey, expectedHash))
		Expect(nodeClaimOne.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashVersionAnnotationKey, v1beta1.NodePoolHashVersion))
		Expect(nodeClaimTwo.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashAnnotationKey, expectedHash))
		Expect(nodeClaimTwo.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashVersionAnnotationKey, v1beta1.NodePoolHashVersion))
	})
	It("should not update nodepool hash on all nodeclaims when the hash versions match the controller hash version", func() {
		nodePool.Annotations = map[string]string{
			v1beta1.NodePoolHashAnnotationKey:        "abceduefed",
			v1beta1.NodePoolHashVersionAnnotationKey: "test-version",
		}
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1beta1.NodePoolLabelKey: nodePool.Name},
				Annotations: map[string]string{
					v1beta1.NodePoolHashAnnotationKey:        "1234564654",
					v1beta1.NodePoolHashVersionAnnotationKey: v1beta1.NodePoolHashVersion,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodePool = ExpectExists(ctx, env.Client, nodePool)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		expectedHash := nodePool.Hash()

		// Expect NodeClaims to have been updated to the original hash
		Expect(nodePool.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashAnnotationKey, expectedHash))
		Expect(nodePool.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashVersionAnnotationKey, v1beta1.NodePoolHashVersion))
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashAnnotationKey, "1234564654"))
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashVersionAnnotationKey, v1beta1.NodePoolHashVersion))
	})
	It("should not update nodepool hash on the nodeclaim if it's drifted", func() {
		nodePool.Annotations = map[string]string{
			v1beta1.NodePoolHashAnnotationKey:        "abceduefed",
			v1beta1.NodePoolHashVersionAnnotationKey: "test",
		}
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1beta1.NodePoolLabelKey: nodePool.Name},
				Annotations: map[string]string{
					v1beta1.NodePoolHashAnnotationKey:        "123456",
					v1beta1.NodePoolHashVersionAnnotationKey: "test",
				},
			},
		})
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Drifted)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		// Expect NodeClaims hash to not have been updated
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashAnnotationKey, "123456"))
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1beta1.NodePoolHashVersionAnnotationKey, v1beta1.NodePoolHashVersion))
	})
	Context("NodeClassRef Defaulting", func() {
		BeforeEach(func() {
			cp.NodeClassGroupVersionKind = schema.GroupVersionKind{
				Group:   "testgroup.sh",
				Version: "v1test1",
				Kind:    "TestNodeClass",
			}
		})
		It("should set a cloudprovider default apiversion on a nodeclassref when apiversion is not set", func() {
			nodePool.Spec.Template.Spec.NodeClassRef.APIVersion = ""
			ExpectApplied(ctx, env.Client, nodePool)
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
			nodePool = ExpectExists(ctx, env.Client, nodePool)
			Expect(nodePool.Spec.Template.Spec.NodeClassRef.APIVersion).To(Equal(cp.NodeClassGroupVersionKind.GroupVersion().String()))
		})
		It("should not set a cloudprovider default apiversion on a nodeclassref when apiversion is set", func() {
			nodePool.Spec.Template.Spec.NodeClassRef.APIVersion = "ExistingAPIVersion"
			ExpectApplied(ctx, env.Client, nodePool)
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
			nodePool = ExpectExists(ctx, env.Client, nodePool)
			Expect(nodePool.Spec.Template.Spec.NodeClassRef.APIVersion).To(Equal("ExistingAPIVersion"))
		})
		It("should set a cloudprovider default kind on a nodeclassref when kind is not set", func() {
			nodePool.Spec.Template.Spec.NodeClassRef.Kind = ""
			ExpectApplied(ctx, env.Client, nodePool)
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
			nodePool = ExpectExists(ctx, env.Client, nodePool)
			Expect(nodePool.Spec.Template.Spec.NodeClassRef.Kind).To(Equal(cp.NodeClassGroupVersionKind.Kind))
		})
		It("should not set a cloudprovider default kind on a nodeclassref when kind is set", func() {
			nodePool.Spec.Template.Spec.NodeClassRef.Kind = "ExistingKind"
			ExpectApplied(ctx, env.Client, nodePool)
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
			nodePool = ExpectExists(ctx, env.Client, nodePool)
			Expect(nodePool.Spec.Template.Spec.NodeClassRef.Kind).To(Equal("ExistingKind"))
		})
	})
})
