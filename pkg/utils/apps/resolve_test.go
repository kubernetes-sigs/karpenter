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

package apps_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/utils/apps"
)

const namespace = "default"

func newClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
}

func podTemplate(image string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "buffer"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: image}}},
	}
}

var _ = Describe("ResolvePodTemplateRef", func() {
	It("should return the template, name and generation of the PodTemplate", func() {
		pt := &corev1.PodTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "pt", Namespace: namespace, Generation: 3},
			Template:   podTemplate("pause"),
		}
		result, err := apps.ResolvePodTemplateRef(context.Background(), newClient(pt), "pt", namespace)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.Name).To(Equal("pt"))
		Expect(result.Generation).To(Equal(int64(3)))
		Expect(result.PodTemplateSpec.Spec.Containers[0].Image).To(Equal("pause"))
	})
	It("should return an error when the PodTemplate does not exist", func() {
		_, err := apps.ResolvePodTemplateRef(context.Background(), newClient(), "missing", namespace)
		Expect(err).To(HaveOccurred())
	})
	It("should not find a PodTemplate in another namespace", func() {
		pt := &corev1.PodTemplate{ObjectMeta: metav1.ObjectMeta{Name: "pt", Namespace: "other"}, Template: podTemplate("pause")}
		_, err := apps.ResolvePodTemplateRef(context.Background(), newClient(pt), "pt", namespace)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("ResolveScalableRef", func() {
	DescribeTable("should resolve the pod template and replicas of a workload",
		func(kind string, obj client.Object) {
			ref := &autoscalingv1beta1.ScalableRef{APIGroup: "apps", Kind: kind, Name: "workload"}
			result, err := apps.ResolveScalableRef(context.Background(), newClient(obj), ref, namespace)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.ScalableReplicas).To(Equal(int32(4)))
			Expect(result.PodTemplateSpec.Spec.Containers[0].Image).To(Equal("pause"))
		},
		Entry("Deployment", autoscalingv1beta1.KindDeployment, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace},
			Spec:       appsv1.DeploymentSpec{Replicas: lo.ToPtr(int32(4)), Template: podTemplate("pause")},
		}),
		Entry("StatefulSet", autoscalingv1beta1.KindStatefulSet, &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace},
			Spec:       appsv1.StatefulSetSpec{Replicas: lo.ToPtr(int32(4)), Template: podTemplate("pause")},
		}),
		Entry("ReplicaSet", autoscalingv1beta1.KindReplicaSet, &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace},
			Spec:       appsv1.ReplicaSetSpec{Replicas: lo.ToPtr(int32(4)), Template: podTemplate("pause")},
		}),
	)
	It("should default the API group to apps", func() {
		d := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace},
			Spec:       appsv1.DeploymentSpec{Replicas: lo.ToPtr(int32(2)), Template: podTemplate("pause")},
		}
		ref := &autoscalingv1beta1.ScalableRef{Kind: autoscalingv1beta1.KindDeployment, Name: "workload"}
		result, err := apps.ResolveScalableRef(context.Background(), newClient(d), ref, namespace)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.ScalableReplicas).To(Equal(int32(2)))
	})
	It("should default unset replicas to 1 but keep an explicit 0", func() {
		unset := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "unset", Namespace: namespace},
			Spec:       appsv1.DeploymentSpec{Template: podTemplate("pause")},
		}
		zero := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "zero", Namespace: namespace},
			Spec:       appsv1.DeploymentSpec{Replicas: lo.ToPtr(int32(0)), Template: podTemplate("pause")},
		}
		c := newClient(unset, zero)
		result, err := apps.ResolveScalableRef(context.Background(), c, &autoscalingv1beta1.ScalableRef{Kind: autoscalingv1beta1.KindDeployment, Name: "unset"}, namespace)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.ScalableReplicas).To(Equal(int32(1)))
		result, err = apps.ResolveScalableRef(context.Background(), c, &autoscalingv1beta1.ScalableRef{Kind: autoscalingv1beta1.KindDeployment, Name: "zero"}, namespace)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.ScalableReplicas).To(Equal(int32(0)))
	})
	It("should reject an API group other than apps", func() {
		ref := &autoscalingv1beta1.ScalableRef{APIGroup: "example.com", Kind: autoscalingv1beta1.KindDeployment, Name: "workload"}
		_, err := apps.ResolveScalableRef(context.Background(), newClient(), ref, namespace)
		Expect(err).To(MatchError(ContainSubstring("unsupported scalableRef API group")))
	})
	It("should reject an unsupported kind", func() {
		ref := &autoscalingv1beta1.ScalableRef{APIGroup: "apps", Kind: "DaemonSet", Name: "workload"}
		_, err := apps.ResolveScalableRef(context.Background(), newClient(), ref, namespace)
		Expect(err).To(MatchError(ContainSubstring("unsupported kind")))
	})
	It("should return an error when the workload does not exist", func() {
		ref := &autoscalingv1beta1.ScalableRef{APIGroup: "apps", Kind: autoscalingv1beta1.KindDeployment, Name: "missing"}
		_, err := apps.ResolveScalableRef(context.Background(), newClient(), ref, namespace)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("ResolveCapacityBuffer", func() {
	It("should resolve a podTemplateRef and report the template name and generation", func() {
		pt := &corev1.PodTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "pt", Namespace: namespace, Generation: 5},
			Template:   podTemplate("from-template"),
		}
		cb := &autoscalingv1beta1.CapacityBuffer{
			ObjectMeta: metav1.ObjectMeta{Name: "cb", Namespace: namespace},
			Spec:       autoscalingv1beta1.CapacityBufferSpec{PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "pt"}},
		}
		result, err := apps.ResolveCapacityBuffer(context.Background(), newClient(pt), cb)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.UsesPodTemplate).To(BeTrue())
		Expect(result.PodTemplateName).To(Equal("pt"))
		Expect(result.PodTemplateGeneration).To(Equal(int64(5)))
		Expect(result.ScalableReplicas).To(BeZero())
		Expect(result.PodTemplateSpec.Spec.Containers[0].Image).To(Equal("from-template"))
	})
	It("should resolve a scalableRef and report the workload replicas", func() {
		d := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace},
			Spec:       appsv1.DeploymentSpec{Replicas: lo.ToPtr(int32(7)), Template: podTemplate("from-deployment")},
		}
		cb := &autoscalingv1beta1.CapacityBuffer{
			ObjectMeta: metav1.ObjectMeta{Name: "cb", Namespace: namespace},
			Spec: autoscalingv1beta1.CapacityBufferSpec{
				ScalableRef: &autoscalingv1beta1.ScalableRef{APIGroup: "apps", Kind: autoscalingv1beta1.KindDeployment, Name: "workload"},
			},
		}
		result, err := apps.ResolveCapacityBuffer(context.Background(), newClient(d), cb)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.UsesPodTemplate).To(BeFalse())
		Expect(result.PodTemplateName).To(BeEmpty())
		Expect(result.ScalableReplicas).To(Equal(int32(7)))
		Expect(result.PodTemplateSpec.Spec.Containers[0].Image).To(Equal("from-deployment"))
	})
	It("should return an error naming the buffer when it has no reference", func() {
		cb := &autoscalingv1beta1.CapacityBuffer{ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: namespace}}
		_, err := apps.ResolveCapacityBuffer(context.Background(), newClient(), cb)
		Expect(err).To(MatchError(ContainSubstring(`buffer "empty" has neither podTemplateRef nor scalableRef`)))
	})
	It("should return the lookup error when the referenced object is missing", func() {
		cb := &autoscalingv1beta1.CapacityBuffer{
			ObjectMeta: metav1.ObjectMeta{Name: "cb", Namespace: namespace},
			Spec:       autoscalingv1beta1.CapacityBufferSpec{PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "missing"}},
		}
		_, err := apps.ResolveCapacityBuffer(context.Background(), newClient(), cb)
		Expect(err).To(HaveOccurred())
	})
})
