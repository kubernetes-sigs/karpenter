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

package informer_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/state/prediction"
	"sigs.k8s.io/karpenter/pkg/test"
	testcrds "sigs.k8s.io/karpenter/pkg/test/crds"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var env *test.Environment
var store *prediction.Store
var controller *informer.VPAController
var targetUID types.UID
var targetRef autoscalingv1.CrossVersionObjectReference
var dep *appsv1.Deployment

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
	controller = informer.NewVPAController(env.Client, env.Client, store)

	dep = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "app"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx"}}},
			},
		},
	}
	ExpectApplied(ctx, env.Client, dep)
	fetched := &appsv1.Deployment{}
	Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(dep), fetched)).To(Succeed())
	targetUID = fetched.UID
	targetRef = autoscalingv1.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "app"}
})

var _ = AfterEach(func() {
	vpaList := &vpav1.VerticalPodAutoscalerList{}
	if err := env.Client.List(ctx, vpaList); err == nil {
		for i := range vpaList.Items {
			ExpectDeleted(ctx, env.Client, &vpaList.Items[i])
		}
	}
	ExpectCleanedUp(ctx, env.Client)
})

type transientErrorReader func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error

func (f transientErrorReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return f(ctx, key, obj, opts...)
}

func (f transientErrorReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return fmt.Errorf("not implemented")
}

var _ = Describe("VPA Controller", func() {
	It("should populate the store from VPA recommendations", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		_, ok := store.Get(targetUID)
		Expect(ok).To(BeFalse())

		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		})
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(targetUID)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"][corev1.ResourceCPU]).To(Equal(resource.MustParse("500m")))
		Expect(pred.Containers["main"][corev1.ResourceMemory]).To(Equal(resource.MustParse("256Mi")))
		Expect(store.Hydrated(ctx)).To(BeTrue())
	})

	It("should remove predictions when VPA is deleted", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("1")},
		})
		ExpectSingletonReconciled(ctx, controller)

		_, ok := store.Get(targetUID)
		Expect(ok).To(BeTrue())

		ExpectDeleted(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		_, ok = store.Get(targetUID)
		Expect(ok).To(BeFalse())
	})

	It("should skip VPAs with updateMode Off", func() {
		mode := vpav1.UpdateModeOff
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			TargetRef:    targetRef,
			UpdatePolicy: &vpav1.PodUpdatePolicy{UpdateMode: &mode},
		})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("100m")},
		})
		ExpectSingletonReconciled(ctx, controller)

		_, ok := store.Get(targetUID)
		Expect(ok).To(BeFalse())
	})

	It("should skip containers with mode Off", func() {
		modeOff := vpav1.ContainerScalingModeOff
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			TargetRef: targetRef,
			ResourcePolicy: &vpav1.PodResourcePolicy{
				ContainerPolicies: []vpav1.ContainerResourcePolicy{
					{ContainerName: "main", Mode: &modeOff},
				},
			},
		})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main":    {corev1.ResourceCPU: resource.MustParse("100m")},
			"sidecar": {corev1.ResourceCPU: resource.MustParse("50m")},
		})
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(targetUID)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers).NotTo(HaveKey("main"))
		Expect(pred.Containers).To(HaveKey("sidecar"))
	})

	It("should clamp recommendations to min/max bounds", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			TargetRef: targetRef,
			ResourcePolicy: &vpav1.PodResourcePolicy{
				ContainerPolicies: []vpav1.ContainerResourcePolicy{
					{
						ContainerName: "main",
						MinAllowed:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
						MaxAllowed:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		})
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(targetUID)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"][corev1.ResourceCPU]).To(Equal(resource.MustParse("200m")))
		Expect(pred.Containers["main"][corev1.ResourceMemory]).To(Equal(resource.MustParse("512Mi")))
	})

	It("should only include controlled resources", func() {
		controlled := []corev1.ResourceName{corev1.ResourceMemory}
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			TargetRef: targetRef,
			ResourcePolicy: &vpav1.PodResourcePolicy{
				ContainerPolicies: []vpav1.ContainerResourcePolicy{
					{ContainerName: "main", ControlledResources: &controlled},
				},
			},
		})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		})
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(targetUID)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"]).To(HaveKey(corev1.ResourceMemory))
		Expect(pred.Containers["main"]).NotTo(HaveKey(corev1.ResourceCPU))
	})

	It("should prefer specific container policy over wildcard", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			TargetRef: targetRef,
			ResourcePolicy: &vpav1.PodResourcePolicy{
				ContainerPolicies: []vpav1.ContainerResourcePolicy{
					{ContainerName: "*", MinAllowed: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
					{ContainerName: "main", MinAllowed: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}},
				},
			},
		})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("50m")},
		})
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(targetUID)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"][corev1.ResourceCPU]).To(Equal(resource.MustParse("500m")))
	})
	It("should retry target resolution on transient failure", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m")},
		})

		// Delete the target deployment so resolution fails
		ExpectDeleted(ctx, env.Client, dep)
		ExpectSingletonReconciled(ctx, controller)
		_, ok := store.Get(targetUID)
		Expect(ok).To(BeFalse())

		newDep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "app"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx"}}},
				},
			},
		}
		ExpectApplied(ctx, env.Client, newDep)
		fetched := &appsv1.Deployment{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(dep), fetched)).To(Succeed())
		ExpectSingletonReconciled(ctx, controller)
		pred, ok := store.Get(fetched.UID)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"][corev1.ResourceCPU]).To(Equal(resource.MustParse("500m")))
	})
	It("should use the oldest VPA's prediction and promote runner-up on deletion", func() {
		// With same creation timestamp, lexicographically smaller name wins
		olderVPA := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			ObjectMeta: metav1.ObjectMeta{Name: "vpa-alpha", Namespace: "default"},
			TargetRef:  targetRef,
		})
		ExpectApplied(ctx, env.Client, olderVPA)
		test.UpdateVPARecommendation(ctx, env.Client, olderVPA, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("200m")},
		})

		newerVPA := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			ObjectMeta: metav1.ObjectMeta{Name: "vpa-beta", Namespace: "default"},
			TargetRef:  targetRef,
		})
		ExpectApplied(ctx, env.Client, newerVPA)
		test.UpdateVPARecommendation(ctx, env.Client, newerVPA, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("800m")},
		})

		ExpectSingletonReconciled(ctx, controller)

		// vpa-alpha wins (lexicographically smaller when timestamps are equal)
		pred, ok := store.Get(targetUID)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"][corev1.ResourceCPU]).To(Equal(resource.MustParse("200m")))

		// Delete the winner — runner-up should be promoted
		ExpectDeleted(ctx, env.Client, olderVPA)
		ExpectSingletonReconciled(ctx, controller)

		pred, ok = store.Get(targetUID)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"][corev1.ResourceCPU]).To(Equal(resource.MustParse("800m")))
	})

	It("should not hydrate on transient errors", func() {
		failingReader := transientErrorReader(func(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return fmt.Errorf("connection refused")
		})
		failingController := informer.NewVPAController(env.Client, failingReader, store)

		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m")},
		})

		ExpectSingletonReconciled(ctx, failingController)
		checkCtx, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		Expect(store.Hydrated(checkCtx)).To(BeFalse())
	})
	It("should apply startup boost factor to CPU prediction", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			TargetRef: targetRef,
			StartupBoost: &vpav1.StartupBoost{
				CPU: &vpav1.GenericStartupBoost{
					Type:   vpav1.FactorStartupBoostType,
					Factor: lo.ToPtr(int32(3)),
				},
			},
		})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		})

		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(targetUID)
		Expect(ok).To(BeTrue())
		cpu := pred.Containers["main"][corev1.ResourceCPU]
		Expect(cpu.Cmp(resource.MustParse("1500m"))).To(Equal(0))
		Expect(pred.Containers["main"][corev1.ResourceMemory]).To(Equal(resource.MustParse("256Mi")))
	})
	It("should apply startup boost quantity to CPU prediction", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			TargetRef: targetRef,
			StartupBoost: &vpav1.StartupBoost{
				CPU: &vpav1.GenericStartupBoost{
					Type:     vpav1.QuantityStartupBoostType,
					Quantity: lo.ToPtr(resource.MustParse("1")),
				},
			},
		})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m")},
		})

		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(targetUID)
		Expect(ok).To(BeTrue())
		cpu := pred.Containers["main"][corev1.ResourceCPU]
		Expect(cpu.Cmp(resource.MustParse("1500m"))).To(Equal(0))
	})
	It("should prefer per-container startup boost over VPA-level", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			TargetRef: targetRef,
			StartupBoost: &vpav1.StartupBoost{
				CPU: &vpav1.GenericStartupBoost{
					Type:   vpav1.FactorStartupBoostType,
					Factor: lo.ToPtr(int32(2)),
				},
			},
			ResourcePolicy: &vpav1.PodResourcePolicy{
				ContainerPolicies: []vpav1.ContainerResourcePolicy{{
					ContainerName: "main",
					StartupBoost: &vpav1.StartupBoost{
						CPU: &vpav1.GenericStartupBoost{
							Type:   vpav1.FactorStartupBoostType,
							Factor: lo.ToPtr(int32(5)),
						},
					},
				}},
			},
		})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("200m")},
		})

		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(targetUID)
		Expect(ok).To(BeTrue())
		cpu := pred.Containers["main"][corev1.ResourceCPU]
		Expect(cpu.Cmp(resource.MustParse("1000m"))).To(Equal(0))
	})

})
