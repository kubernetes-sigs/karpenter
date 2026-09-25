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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/utils/apps"
)

func TestApps(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Apps Suite")
}

var _ = Describe("ResolveScalableRef", func() {
	var ctx context.Context
	var scheme *runtime.Scheme

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		_ = appsv1.AddToScheme(scheme)
		_ = corev1.AddToScheme(scheme)
		scheme.AddKnownTypes(schema.GroupVersion{Group: "autoscaling.x-k8s.io", Version: "v1beta1"}, &autoscalingv1beta1.CapacityBuffer{}, &autoscalingv1beta1.CapacityBufferList{})
	})

	It("should resolve StatefulSet and append VolumeClaimTemplates to PodSpec.Volumes", func() {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-sts",
				Namespace: "default",
			},
			Spec: appsv1.StatefulSetSpec{
				Replicas: lo.ToPtr(int32(3)),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "nginx", Image: "nginx"},
						},
						Volumes: []corev1.Volume{
							{Name: "config-vol", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}}}},
						},
					},
				},
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "data-vct"},
						Spec: corev1.PersistentVolumeClaimSpec{
							StorageClassName: lo.ToPtr("standard"),
						},
					},
				},
			},
		}

		kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
		ref := &autoscalingv1beta1.ScalableRef{
			Kind: autoscalingv1beta1.KindStatefulSet,
			Name: "web-sts",
		}

		res, err := apps.ResolveScalableRef(ctx, kubeClient, ref, "default")
		Expect(err).ToNot(HaveOccurred())
		Expect(res.ScalableReplicas).To(Equal(int32(3)))
		Expect(res.PodTemplateSpec.Spec.Volumes).To(HaveLen(2))
		Expect(res.PodTemplateSpec.Spec.Volumes[0].Name).To(Equal("config-vol"))
		Expect(res.PodTemplateSpec.Spec.Volumes[1].Name).To(Equal("data-vct"))
		Expect(res.PodTemplateSpec.Spec.Volumes[1].PersistentVolumeClaim).ToNot(BeNil())
		Expect(res.PodTemplateSpec.Spec.Volumes[1].PersistentVolumeClaim.ClaimName).To(Equal("data-vct"))
	})

	It("should not duplicate volume if StatefulSet podSpec already contains volume with same VCT name", func() {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-sts-existing",
				Namespace: "default",
			},
			Spec: appsv1.StatefulSetSpec{
				Replicas: lo.ToPtr(int32(2)),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "app", Image: "app"}},
						Volumes: []corev1.Volume{
							{Name: "data-vct", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "custom-claim"}}},
						},
					},
				},
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
					{ObjectMeta: metav1.ObjectMeta{Name: "data-vct"}},
				},
			},
		}

		kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
		ref := &autoscalingv1beta1.ScalableRef{
			Kind: autoscalingv1beta1.KindStatefulSet,
			Name: "web-sts-existing",
		}

		res, err := apps.ResolveScalableRef(ctx, kubeClient, ref, "default")
		Expect(err).ToNot(HaveOccurred())
		Expect(res.PodTemplateSpec.Spec.Volumes).To(HaveLen(1))
		Expect(res.PodTemplateSpec.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("custom-claim"))
	})

	It("should resolve Deployment", func() {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-dep",
				Namespace: "default",
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: lo.ToPtr(int32(5)),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "web", Image: "nginx"}},
					},
				},
			},
		}

		kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
		ref := &autoscalingv1beta1.ScalableRef{
			Kind: autoscalingv1beta1.KindDeployment,
			Name: "web-dep",
		}

		res, err := apps.ResolveScalableRef(ctx, kubeClient, ref, "default")
		Expect(err).ToNot(HaveOccurred())
		Expect(res.ScalableReplicas).To(Equal(int32(5)))
	})
})
