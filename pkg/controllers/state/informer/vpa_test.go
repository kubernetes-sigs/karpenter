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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/state/prediction"
	"sigs.k8s.io/karpenter/pkg/test"
	testcrds "sigs.k8s.io/karpenter/pkg/test/crds"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var env *test.Environment
var store *prediction.Store
var cluster *state.Cluster
var recorder *test.EventRecorder
var controller *informer.VPAController
var target prediction.TargetKey
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
	cluster = state.NewCluster(env.Clock, env.Client, fake.NewCloudProvider())
	recorder = test.NewEventRecorder()
	controller = informer.NewVPAController(env.Client, store, cluster, recorder)

	dep = test.Deployment(test.DeploymentOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
	})
	ExpectApplied(ctx, env.Client, dep)
	target = prediction.TargetKey{
		GroupKind:      schema.GroupKind{Group: "apps", Kind: "Deployment"},
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"},
	}
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

var _ = Describe("VPA Controller", func() {
	DescribeTable("should compute predictions correctly",
		func(opts test.VerticalPodAutoscalerOptions, recommendation map[string]corev1.ResourceList, expectFound bool, expected map[string]corev1.ResourceList) {
			opts.TargetRef = targetRef
			vpa := test.VerticalPodAutoscaler(opts)
			ExpectApplied(ctx, env.Client, vpa)
			test.UpdateVPARecommendation(ctx, env.Client, vpa, recommendation)
			ExpectSingletonReconciled(ctx, controller)

			pred, ok := store.Get(target)
			Expect(ok).To(Equal(expectFound))
			if expectFound {
				for container, resources := range expected {
					for res, qty := range resources {
						actual := pred.Containers[container][res]
						Expect(actual.Cmp(qty)).To(Equal(0), "container=%s resource=%s expected=%s actual=%s", container, res, qty.String(), actual.String())
					}
				}
			}
		},
		Entry("basic recommendation",
			test.VerticalPodAutoscalerOptions{},
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
			true,
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
		),
		Entry("skip VPAs with updateMode Off",
			test.VerticalPodAutoscalerOptions{
				UpdatePolicy: &vpav1.PodUpdatePolicy{UpdateMode: lo.ToPtr(vpav1.UpdateModeOff)},
			},
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("100m")},
			},
			false,
			nil,
		),
		Entry("clamp recommendations to min/max bounds",
			test.VerticalPodAutoscalerOptions{
				ResourcePolicy: &vpav1.PodResourcePolicy{
					ContainerPolicies: []vpav1.ContainerResourcePolicy{{
						ContainerName: "main",
						MinAllowed:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
						MaxAllowed:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
					}},
				},
			},
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
			true,
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
		),
		Entry("prefer specific container policy over wildcard",
			test.VerticalPodAutoscalerOptions{
				ResourcePolicy: &vpav1.PodResourcePolicy{
					ContainerPolicies: []vpav1.ContainerResourcePolicy{
						{ContainerName: "*", MinAllowed: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
						{ContainerName: "main", MinAllowed: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}},
					},
				},
			},
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("50m")},
			},
			true,
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("500m")},
			},
		),
		Entry("apply startup boost factor to CPU prediction",
			test.VerticalPodAutoscalerOptions{
				StartupBoost: &vpav1.StartupBoost{
					CPU: &vpav1.GenericStartupBoost{
						Type:   vpav1.FactorStartupBoostType,
						Factor: lo.ToPtr(int32(3)),
					},
				},
			},
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
			true,
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("1500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
		),
		Entry("apply startup boost quantity to CPU prediction",
			test.VerticalPodAutoscalerOptions{
				StartupBoost: &vpav1.StartupBoost{
					CPU: &vpav1.GenericStartupBoost{
						Type:     vpav1.QuantityStartupBoostType,
						Quantity: lo.ToPtr(resource.MustParse("1")),
					},
				},
			},
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("500m")},
			},
			true,
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("1500m")},
			},
		),
		Entry("prefer per-container startup boost over VPA-level",
			test.VerticalPodAutoscalerOptions{
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
			},
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("200m")},
			},
			true,
			map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("1000m")},
			},
		),
	)

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

		pred, ok := store.Get(target)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers).NotTo(HaveKey("main"))
		Expect(pred.Containers).To(HaveKey("sidecar"))
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
			"main": {corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		})
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(target)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"]).To(HaveKey(corev1.ResourceMemory))
		Expect(pred.Containers["main"]).NotTo(HaveKey(corev1.ResourceCPU))
	})

	It("should populate the store only after recommendation is set", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		_, ok := store.Get(target)
		Expect(ok).To(BeFalse())

		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m")},
		})
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(target)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"][corev1.ResourceCPU]).To(Equal(resource.MustParse("500m")))
		Expect(store.Hydrated(ctx)).To(BeTrue())
	})

	It("should remove predictions when VPA is deleted", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("1")},
		})
		ExpectSingletonReconciled(ctx, controller)

		_, ok := store.Get(target)
		Expect(ok).To(BeTrue())

		ExpectDeleted(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)

		_, ok = store.Get(target)
		Expect(ok).To(BeFalse())
	})

	It("should mark the cluster unconsolidated when a prediction is added, changed, or removed", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)

		for _, cpu := range []string{"500m", "1"} {
			consolidationState := cluster.ConsolidationState()
			env.Clock.Step(time.Second)
			test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse(cpu)},
			})
			ExpectSingletonReconciled(ctx, controller)
			Expect(cluster.ConsolidationState()).ToNot(Equal(consolidationState))
		}

		consolidationState := cluster.ConsolidationState()
		env.Clock.Step(time.Second)
		ExpectDeleted(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)
		Expect(cluster.ConsolidationState()).ToNot(Equal(consolidationState))
	})

	It("should not mark the cluster unconsolidated when a VPA update leaves its prediction unchanged", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m")},
		})
		ExpectSingletonReconciled(ctx, controller)

		consolidationState := cluster.ConsolidationState()
		env.Clock.Step(time.Second)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(vpa), vpa)).To(Succeed())
		vpa.Labels = map[string]string{"updated": "true"}
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)
		Expect(cluster.ConsolidationState()).To(Equal(consolidationState))
	})

	It("should keep a prediction for a target that's deleted and recreated with the same name", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m")},
		})
		ExpectSingletonReconciled(ctx, controller)

		ExpectDeleted(ctx, env.Client, dep)
		ExpectApplied(ctx, env.Client, test.Deployment(test.DeploymentOptions{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		}))
		ExpectSingletonReconciled(ctx, controller)

		pred, ok := store.Get(target)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"][corev1.ResourceCPU]).To(Equal(resource.MustParse("500m")))
	})

	It("should use the oldest VPA's prediction and promote runner-up on deletion", func() {
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

		pred, ok := store.Get(target)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"][corev1.ResourceCPU]).To(Equal(resource.MustParse("200m")))

		ExpectDeleted(ctx, env.Client, olderVPA)
		ExpectSingletonReconciled(ctx, controller)

		pred, ok = store.Get(target)
		Expect(ok).To(BeTrue())
		Expect(pred.Containers["main"][corev1.ResourceCPU]).To(Equal(resource.MustParse("800m")))
	})

	It("should not store a prediction for an unsupported target kind and publish an event", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{
			TargetRef: autoscalingv1.CrossVersionObjectReference{APIVersion: "argoproj.io/v1alpha1", Kind: "Rollout", Name: "app"},
		})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m")},
		})
		ExpectSingletonReconciled(ctx, controller)

		Expect(store.Len()).To(Equal(0))
		Expect(recorder.Calls(events.PredictionTargetUnsupported)).To(Equal(1))
		Expect(store.Hydrated(ctx)).To(BeTrue())
	})

	It("should remove a VPA's prediction when it's retargeted to an unsupported kind", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m")},
		})
		ExpectSingletonReconciled(ctx, controller)
		_, ok := store.Get(target)
		Expect(ok).To(BeTrue())

		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(vpa), vpa)).To(Succeed())
		vpa.Spec.TargetRef = &autoscalingv1.CrossVersionObjectReference{APIVersion: "argoproj.io/v1alpha1", Kind: "Rollout", Name: "app"}
		ExpectApplied(ctx, env.Client, vpa)
		ExpectSingletonReconciled(ctx, controller)
		Expect(store.Len()).To(Equal(0))
	})
})
