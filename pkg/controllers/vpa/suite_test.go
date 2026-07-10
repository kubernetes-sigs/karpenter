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

package vpa

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/state/prediction"
	"sigs.k8s.io/karpenter/pkg/test"
	testcrds "sigs.k8s.io/karpenter/pkg/test/crds"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var env *test.Environment
var store *prediction.Store
var controller *Controller

func TestVPA(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "VPA Controller")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(testcrds.CRDs...))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed())
})

var _ = BeforeEach(func() {
	store = prediction.NewStore()
	controller = NewController(env.Client, store)
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("VPA Controller", func() {
	It("should populate the store from VPA recommendations", func() {
		vpa := &vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "test-vpa", Namespace: "default"},
			Spec: vpav1.VerticalPodAutoscalerSpec{
				TargetRef: &autoscalingv1.CrossVersionObjectReference{Kind: "Deployment", Name: "web-app"},
			},
		}
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		_, ok := store.Get("default", "Deployment", "web-app")
		Expect(ok).To(BeFalse())

		vpa.Status.Recommendation = &vpav1.RecommendedPodResources{
			ContainerRecommendations: []vpav1.RecommendedContainerResources{
				{
					ContainerName: "app",
					Target: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get("default", "Deployment", "web-app")
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["app"][corev1.ResourceCPU]).To(Equal(resource.MustParse("500m")))
		Expect(pred.Containers["app"][corev1.ResourceMemory]).To(Equal(resource.MustParse("256Mi")))
	})

	It("should remove predictions when VPA is deleted", func() {
		vpa := &vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "test-vpa", Namespace: "default"},
			Spec: vpav1.VerticalPodAutoscalerSpec{
				TargetRef: &autoscalingv1.CrossVersionObjectReference{Kind: "StatefulSet", Name: "db"},
			},
			Status: vpav1.VerticalPodAutoscalerStatus{
				Recommendation: &vpav1.RecommendedPodResources{
					ContainerRecommendations: []vpav1.RecommendedContainerResources{
						{ContainerName: "postgres", Target: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		_, ok := store.Get("default", "StatefulSet", "db")
		Expect(ok).To(BeTrue())

		ExpectDeleted(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		_, ok = store.Get("default", "StatefulSet", "db")
		Expect(ok).To(BeFalse())
	})

	It("should skip VPAs with updateMode Off", func() {
		mode := vpav1.UpdateModeOff
		vpa := &vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "test-vpa", Namespace: "default"},
			Spec: vpav1.VerticalPodAutoscalerSpec{
				TargetRef:    &autoscalingv1.CrossVersionObjectReference{Kind: "Deployment", Name: "off-app"},
				UpdatePolicy: &vpav1.PodUpdatePolicy{UpdateMode: &mode},
			},
			Status: vpav1.VerticalPodAutoscalerStatus{
				Recommendation: &vpav1.RecommendedPodResources{
					ContainerRecommendations: []vpav1.RecommendedContainerResources{
						{ContainerName: "app", Target: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		_, ok := store.Get("default", "Deployment", "off-app")
		Expect(ok).To(BeFalse())
	})

	It("should skip containers with mode Off", func() {
		modeOff := vpav1.ContainerScalingModeOff
		vpa := &vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "test-vpa", Namespace: "default"},
			Spec: vpav1.VerticalPodAutoscalerSpec{
				TargetRef: &autoscalingv1.CrossVersionObjectReference{Kind: "Deployment", Name: "mixed-app"},
				ResourcePolicy: &vpav1.PodResourcePolicy{
					ContainerPolicies: []vpav1.ContainerResourcePolicy{
						{ContainerName: "app", Mode: &modeOff},
					},
				},
			},
			Status: vpav1.VerticalPodAutoscalerStatus{
				Recommendation: &vpav1.RecommendedPodResources{
					ContainerRecommendations: []vpav1.RecommendedContainerResources{
						{ContainerName: "app", Target: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
						{ContainerName: "sidecar", Target: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get("default", "Deployment", "mixed-app")
		Expect(ok).To(BeTrue())
		Expect(pred.Containers).NotTo(HaveKey("app"))
		Expect(pred.Containers).To(HaveKey("sidecar"))
	})

	It("should clamp recommendations to min/max bounds", func() {
		vpa := &vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "test-vpa", Namespace: "default"},
			Spec: vpav1.VerticalPodAutoscalerSpec{
				TargetRef: &autoscalingv1.CrossVersionObjectReference{Kind: "Deployment", Name: "clamp-app"},
				ResourcePolicy: &vpav1.PodResourcePolicy{
					ContainerPolicies: []vpav1.ContainerResourcePolicy{
						{
							ContainerName: "app",
							MinAllowed:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
							MaxAllowed:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
						},
					},
				},
			},
			Status: vpav1.VerticalPodAutoscalerStatus{
				Recommendation: &vpav1.RecommendedPodResources{
					ContainerRecommendations: []vpav1.RecommendedContainerResources{
						{
							ContainerName: "app",
							Target: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get("default", "Deployment", "clamp-app")
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["app"][corev1.ResourceCPU]).To(Equal(resource.MustParse("200m")))
		Expect(pred.Containers["app"][corev1.ResourceMemory]).To(Equal(resource.MustParse("512Mi")))
	})

	It("should only include controlled resources", func() {
		controlled := []corev1.ResourceName{corev1.ResourceMemory}
		vpa := &vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "test-vpa", Namespace: "default"},
			Spec: vpav1.VerticalPodAutoscalerSpec{
				TargetRef: &autoscalingv1.CrossVersionObjectReference{Kind: "Deployment", Name: "controlled-app"},
				ResourcePolicy: &vpav1.PodResourcePolicy{
					ContainerPolicies: []vpav1.ContainerResourcePolicy{
						{ContainerName: "app", ControlledResources: &controlled},
					},
				},
			},
			Status: vpav1.VerticalPodAutoscalerStatus{
				Recommendation: &vpav1.RecommendedPodResources{
					ContainerRecommendations: []vpav1.RecommendedContainerResources{
						{
							ContainerName: "app",
							Target: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get("default", "Deployment", "controlled-app")
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["app"]).To(HaveKey(corev1.ResourceMemory))
		Expect(pred.Containers["app"]).NotTo(HaveKey(corev1.ResourceCPU))
	})

	It("should prefer specific container policy over wildcard", func() {
		vpa := &vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "test-vpa", Namespace: "default"},
			Spec: vpav1.VerticalPodAutoscalerSpec{
				TargetRef: &autoscalingv1.CrossVersionObjectReference{Kind: "Deployment", Name: "web-app"},
				ResourcePolicy: &vpav1.PodResourcePolicy{
					ContainerPolicies: []vpav1.ContainerResourcePolicy{
						{
							ContainerName: "*",
							MinAllowed:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
						},
						{
							ContainerName: "app",
							MinAllowed:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
						},
					},
				},
			},
			Status: vpav1.VerticalPodAutoscalerStatus{
				Recommendation: &vpav1.RecommendedPodResources{
					ContainerRecommendations: []vpav1.RecommendedContainerResources{
						{
							ContainerName: "app",
							Target: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("50m"),
							},
						},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		// "app" should use its specific policy (min 500m), not the wildcard (min 100m)
		pred, ok := store.Get("default", "Deployment", "web-app")
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["app"][corev1.ResourceCPU]).To(Equal(resource.MustParse("500m")))
	})
})
