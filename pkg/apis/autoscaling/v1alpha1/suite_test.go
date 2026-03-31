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

package v1alpha1_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	autoscalingv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "CapacityBuffer")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(apis.CapacityBufferCRDInstance()))
})

var _ = AfterEach(func() {
	cbList := &autoscalingv1alpha1.CapacityBufferList{}
	Expect(env.Client.List(ctx, cbList)).To(Succeed())
	for i := range cbList.Items {
		Expect(env.Client.Delete(ctx, &cbList.Items[i])).To(Succeed())
	}
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("CapacityBuffer CRD", func() {
	Context("Registration", func() {
		It("should register the CapacityBuffer CRD", func() {
			crd := apis.CapacityBufferCRDInstance()
			Expect(crd).ToNot(BeNil())
			Expect(crd.Name).To(Equal("capacitybuffers.autoscaling.x-k8s.io"))
			Expect(crd.Spec.Group).To(Equal("autoscaling.x-k8s.io"))
			Expect(crd.Spec.Names.Kind).To(Equal("CapacityBuffer"))
			Expect(crd.Spec.Names.Plural).To(Equal("capacitybuffers"))
			Expect(crd.Spec.Scope).To(Equal(apiextensionsv1.NamespaceScoped))
		})
		It("should create and retrieve a CapacityBuffer resource", func() {
			cb := &autoscalingv1alpha1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: "test-buffer", Namespace: "default"},
				Spec: autoscalingv1alpha1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1alpha1.LocalObjectRef{Name: "my-template"},
					Replicas:       lo.ToPtr[int32](5),
				},
			}
			Expect(env.Client.Create(ctx, cb)).To(Succeed())
			retrieved := &autoscalingv1alpha1.CapacityBuffer{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(cb), retrieved)).To(Succeed())
			Expect(retrieved.Spec.PodTemplateRef.Name).To(Equal("my-template"))
			Expect(lo.FromPtr(retrieved.Spec.Replicas)).To(Equal(int32(5)))
		})
	})
	Context("Schema Validation", func() {
		It("should accept a valid CapacityBuffer with podTemplateRef", func() {
			cb := &autoscalingv1alpha1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: test.RandomName(), Namespace: "default"},
				Spec: autoscalingv1alpha1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1alpha1.LocalObjectRef{Name: "my-template"},
					Replicas:       lo.ToPtr[int32](3),
				},
			}
			Expect(env.Client.Create(ctx, cb)).To(Succeed())
		})
		It("should accept a valid CapacityBuffer with scalableRef", func() {
			cb := &autoscalingv1alpha1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: test.RandomName(), Namespace: "default"},
				Spec: autoscalingv1alpha1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1alpha1.ScalableRef{APIGroup: "apps", Kind: "Deployment", Name: "my-deploy"},
					Replicas:    lo.ToPtr[int32](5),
				},
			}
			Expect(env.Client.Create(ctx, cb)).To(Succeed())
		})
		It("should reject negative replicas", func() {
			cb := &autoscalingv1alpha1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: test.RandomName(), Namespace: "default"},
				Spec: autoscalingv1alpha1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1alpha1.LocalObjectRef{Name: "my-template"},
					Replicas:       lo.ToPtr[int32](-1),
				},
			}
			Expect(env.Client.Create(ctx, cb)).ToNot(Succeed())
		})
		It("should reject negative percentage", func() {
			cb := &autoscalingv1alpha1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: test.RandomName(), Namespace: "default"},
				Spec: autoscalingv1alpha1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1alpha1.ScalableRef{APIGroup: "apps", Kind: "Deployment", Name: "my-deploy"},
					Percentage:  lo.ToPtr[int32](-1),
				},
			}
			Expect(env.Client.Create(ctx, cb)).ToNot(Succeed())
		})
		It("should accept zero replicas", func() {
			cb := &autoscalingv1alpha1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: test.RandomName(), Namespace: "default"},
				Spec: autoscalingv1alpha1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1alpha1.LocalObjectRef{Name: "my-template"},
					Replicas:       lo.ToPtr[int32](0),
				},
			}
			Expect(env.Client.Create(ctx, cb)).To(Succeed())
		})
		It("should accept zero percentage", func() {
			cb := &autoscalingv1alpha1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: test.RandomName(), Namespace: "default"},
				Spec: autoscalingv1alpha1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1alpha1.ScalableRef{APIGroup: "apps", Kind: "Deployment", Name: "my-deploy"},
					Percentage:  lo.ToPtr[int32](0),
				},
			}
			Expect(env.Client.Create(ctx, cb)).To(Succeed())
		})
		It("should set default provisioningStrategy", func() {
			cb := &autoscalingv1alpha1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: test.RandomName(), Namespace: "default"},
				Spec: autoscalingv1alpha1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1alpha1.LocalObjectRef{Name: "my-template"},
					Replicas:       lo.ToPtr[int32](1),
				},
			}
			Expect(env.Client.Create(ctx, cb)).To(Succeed())
			retrieved := &autoscalingv1alpha1.CapacityBuffer{}
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(cb), retrieved)).To(Succeed())
			Expect(lo.FromPtr(retrieved.Spec.ProvisioningStrategy)).To(Equal("buffer.x-k8s.io/active-capacity"))
		})
		It("should accept both replicas and percentage together", func() {
			cb := &autoscalingv1alpha1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: test.RandomName(), Namespace: "default"},
				Spec: autoscalingv1alpha1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1alpha1.ScalableRef{APIGroup: "apps", Kind: "Deployment", Name: "my-deploy"},
					Replicas:    lo.ToPtr[int32](10),
					Percentage:  lo.ToPtr[int32](20),
				},
			}
			Expect(env.Client.Create(ctx, cb)).To(Succeed())
		})
	})
})
