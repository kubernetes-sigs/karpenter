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

package scheduling

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
)

var _ = Describe("VolumeTopology Internals", func() {
	var ctx context.Context
	var scheme *runtime.Scheme

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
	})

	Context("Virtual Pod PVC Handling", func() {
		It("should ignore missing PVC in GetRequirements for virtual pods", func() {
			virtualPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "capacity-buffer-web-1",
					Namespace: "default",
					Annotations: map[string]string{
						autoscalingv1beta1.FakePodAnnotationKey: autoscalingv1beta1.FakePodAnnotationValue,
					},
				},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: "missing-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "missing-data-pvc",
								},
							},
						},
					},
				},
			}

			kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			vt := NewVolumeTopology(kubeClient)

			reqs, err := vt.GetRequirements(ctx, virtualPod)
			Expect(err).ToNot(HaveOccurred())
			Expect(reqs).To(BeNil())
		})

		It("should fail GetRequirements for non-virtual pods when PVC is missing", func() {
			realPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "real-pod-1",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: "missing-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "missing-data-pvc",
								},
							},
						},
					},
				},
			}

			kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			vt := NewVolumeTopology(kubeClient)

			_, err := vt.GetRequirements(ctx, realPod)
			Expect(err).To(HaveOccurred())
		})

		It("should skip missing PVC in ValidatePersistentVolumeClaims for virtual pods", func() {
			virtualPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "capacity-buffer-web-1",
					Namespace: "default",
					Annotations: map[string]string{
						autoscalingv1beta1.FakePodAnnotationKey: autoscalingv1beta1.FakePodAnnotationValue,
					},
				},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: "missing-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "missing-data-pvc",
								},
							},
						},
					},
				},
			}

			kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			vt := NewVolumeTopology(kubeClient)

			err := vt.ValidatePersistentVolumeClaims(ctx, virtualPod)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should fail ValidatePersistentVolumeClaims for non-virtual pods when PVC is missing", func() {
			realPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "real-pod-1",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: "missing-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "missing-data-pvc",
								},
							},
						},
					},
				},
			}

			kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			vt := NewVolumeTopology(kubeClient)

			err := vt.ValidatePersistentVolumeClaims(ctx, realPod)
			Expect(err).To(HaveOccurred())
		})
	})
})
