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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

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
var target prediction.WorkloadReference
var targetRef autoscalingv1.CrossVersionObjectReference
var dep *appsv1.Deployment

const vpaCRDName = "verticalpodautoscalers.autoscaling.k8s.io"

var vpaGVK = vpav1.SchemeGroupVersion.WithKind("VerticalPodAutoscaler")

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
	store = prediction.NewStore(env.Client)
	cluster = state.NewCluster(env.Clock, env.Client, fake.NewCloudProvider())
	recorder = test.NewEventRecorder()
	controller = informer.NewVPAController(env.Client, env.Client, &informertest.FakeInformers{}, store, cluster, recorder)

	dep = test.Deployment(test.DeploymentOptions{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
	})
	ExpectApplied(ctx, env.Client, dep)
	target = prediction.WorkloadReference{
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
						Factor: new(int32(3)),
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
						Quantity: new(resource.MustParse("1")),
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
						Factor: new(int32(2)),
					},
				},
				ResourcePolicy: &vpav1.PodResourcePolicy{
					ContainerPolicies: []vpav1.ContainerResourcePolicy{{
						ContainerName: "main",
						StartupBoost: &vpav1.StartupBoost{
							CPU: &vpav1.GenericStartupBoost{
								Type:   vpav1.FactorStartupBoostType,
								Factor: new(int32(5)),
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

	It("should publish a PredictionStored event when a prediction is stored or changes", func() {
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m")},
		})
		ExpectSingletonReconciled(ctx, controller)
		Expect(recorder.Calls(events.PredictionStored)).To(Equal(1))

		ExpectSingletonReconciled(ctx, controller)
		Expect(recorder.Calls(events.PredictionStored)).To(Equal(1))

		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("800m")},
		})
		ExpectSingletonReconciled(ctx, controller)
		Expect(recorder.Calls(events.PredictionStored)).To(Equal(2))
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
	Context("VPA CRD", func() {
		var reader *crdReader
		var kubeClient *vpaListClient
		var informers *informertest.FakeInformers

		BeforeEach(func() {
			reader = &crdReader{Reader: env.Client}
			kubeClient = &vpaListClient{Client: env.Client}
			informers = &informertest.FakeInformers{}
			controller = informer.NewVPAController(kubeClient, reader, informers, store, cluster, recorder)
		})

		It("should mark the store hydrated without listing VPAs when the CRD is absent", func() {
			_, err := informers.GetInformer(ctx, &vpav1.VerticalPodAutoscaler{})
			Expect(err).ToNot(HaveOccurred())
			reader.lookups = []crdLookup{crdNotFound}

			result := ExpectSingletonReconciled(ctx, controller)

			Expect(result.RequeueAfter).To(Equal(time.Minute))
			Expect(kubeClient.lists).To(Equal(0))
			Expect(store.Hydrated(ctx)).To(BeTrue())
			Expect(informers.InformersByGVK).ToNot(HaveKey(vpaGVK))
		})

		It("should list VPAs while the CRD is terminating", func() {
			vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
			ExpectApplied(ctx, env.Client, vpa)
			test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("500m")},
			})
			_, err := informers.GetInformer(ctx, &vpav1.VerticalPodAutoscaler{})
			Expect(err).ToNot(HaveOccurred())
			reader.lookups = []crdLookup{crdTerminating}

			result := ExpectSingletonReconciled(ctx, controller)

			Expect(reader.lookups).To(BeEmpty())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(kubeClient.lists).To(Equal(1))
			_, ok := store.Get(target)
			Expect(ok).To(BeTrue())
			Expect(store.Hydrated(ctx)).To(BeTrue())
			Expect(informers.InformersByGVK).To(HaveKey(vpaGVK))
		})

		It("should mark the store hydrated in the same reconcile when the CRD is terminating and the List times out", func() {
			_, err := informers.GetInformer(ctx, &vpav1.VerticalPodAutoscaler{})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(func(timeout time.Duration) { informer.VPAListTimeout = timeout }, informer.VPAListTimeout)
			informer.VPAListTimeout = time.Second
			kubeClient.list = blockUntilDone
			reader.lookups = []crdLookup{crdTerminating, crdTerminating}

			start := time.Now()
			result := ExpectSingletonReconciled(ctx, controller)

			Expect(time.Since(start)).To(BeNumerically("<", 2*informer.VPAListTimeout))
			Expect(reader.lookups).To(BeEmpty())
			Expect(result.RequeueAfter).To(Equal(time.Minute))
			Expect(store.Hydrated(ctx)).To(BeTrue())
			Expect(informers.InformersByGVK).ToNot(HaveKey(vpaGVK))
		})

		It("should not look up the CRD before listing once a List has returned VPAs", func() {
			ExpectApplied(ctx, env.Client, test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef}))

			ExpectSingletonReconciled(ctx, controller)
			ExpectSingletonReconciled(ctx, controller)

			Expect(kubeClient.lists).To(Equal(2))
			Expect(reader.gets).To(Equal(1))
		})

		It("should look up the CRD before listing while a List returns no VPAs, so a CRD deleted under a synced informer is still found", func() {
			vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
			ExpectApplied(ctx, env.Client, vpa)
			test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("500m")},
			})
			_, err := informers.GetInformer(ctx, &vpav1.VerticalPodAutoscaler{})
			Expect(err).ToNot(HaveOccurred())
			ExpectSingletonReconciled(ctx, controller)
			Expect(store.Len()).To(Equal(1))
			Expect(reader.gets).To(Equal(1))

			// Deleting the CRD deletes its VPAs, and the synced informer keeps answering the List from its cache, so the
			// empty List is all this reconcile sees
			ExpectDeleted(ctx, env.Client, vpa)
			ExpectSingletonReconciled(ctx, controller)
			Expect(store.Len()).To(Equal(0))
			Expect(reader.gets).To(Equal(1))

			lists := kubeClient.lists
			reader.lookups = []crdLookup{crdNotFound}
			result := ExpectSingletonReconciled(ctx, controller)

			Expect(reader.lookups).To(BeEmpty())
			Expect(kubeClient.lists).To(Equal(lists))
			Expect(result.RequeueAfter).To(Equal(time.Minute))
			Expect(store.Hydrated(ctx)).To(BeTrue())
			Expect(informers.InformersByGVK).ToNot(HaveKey(vpaGVK))
		})

		It("should look up the CRD before listing again once predictions are dropped, and restore them once it's back", func() {
			vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
			ExpectApplied(ctx, env.Client, vpa)
			test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("500m")},
			})
			ExpectSingletonReconciled(ctx, controller)
			_, ok := store.Get(target)
			Expect(ok).To(BeTrue())

			kubeClient.list = func(context.Context) error {
				return &meta.NoKindMatchError{GroupKind: vpaGVK.GroupKind(), SearchedVersions: []string{vpaGVK.Version}}
			}
			ExpectSingletonReconciled(ctx, controller)
			Expect(store.Len()).To(Equal(0))

			// A List now would start an informer that can't sync, so the CRD lookup has to come first
			lists := kubeClient.lists
			kubeClient.list = nil
			reader.lookups = []crdLookup{crdNotFound}
			result := ExpectSingletonReconciled(ctx, controller)
			Expect(reader.lookups).To(BeEmpty())
			Expect(kubeClient.lists).To(Equal(lists))
			Expect(result.RequeueAfter).To(Equal(time.Minute))
			Expect(store.Len()).To(Equal(0))

			ExpectSingletonReconciled(ctx, controller)
			_, ok = store.Get(target)
			Expect(ok).To(BeTrue())
		})

		It("should mark the store hydrated in the same reconcile when the List times out because the CRD was deleted", func() {
			vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
			ExpectApplied(ctx, env.Client, vpa)
			test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("500m")},
			})
			ExpectSingletonReconciled(ctx, controller)
			Expect(store.Len()).To(Equal(1))
			_, err := informers.GetInformer(ctx, &vpav1.VerticalPodAutoscaler{})
			Expect(err).ToNot(HaveOccurred())
			consolidationState := cluster.ConsolidationState()
			env.Clock.Step(time.Second)

			DeferCleanup(func(timeout time.Duration) { informer.VPAListTimeout = timeout }, informer.VPAListTimeout)
			informer.VPAListTimeout = time.Second
			kubeClient.list = blockUntilDone
			reader.lookups = []crdLookup{crdNotFound}

			start := time.Now()
			result := ExpectSingletonReconciled(ctx, controller)

			Expect(time.Since(start)).To(BeNumerically("<", 2*informer.VPAListTimeout))
			Expect(reader.lookups).To(BeEmpty())
			Expect(result.RequeueAfter).To(Equal(time.Minute))
			Expect(store.Len()).To(Equal(0))
			Expect(store.Hydrated(ctx)).To(BeTrue())
			Expect(cluster.ConsolidationState()).ToNot(Equal(consolidationState))
			Expect(informers.InformersByGVK).ToNot(HaveKey(vpaGVK))
		})

		It("should bound the VPA List with the list timeout", func() {
			var deadline time.Time
			kubeClient.list = func(ctx context.Context) error {
				var ok bool
				deadline, ok = ctx.Deadline()
				Expect(ok).To(BeTrue())
				return nil
			}
			start := time.Now()
			ExpectSingletonReconciled(ctx, controller)
			Expect(deadline).To(BeTemporally("~", start.Add(informer.VPAListTimeout), time.Second))
		})

		It("should keep the list timeout within half the hydration timeout", func() {
			Expect(informer.VPAListTimeout).To(BeNumerically("<=", prediction.HydrationTimeout/2))
		})

		It("should not mark the store hydrated when the CRD lookup is forbidden and the List times out", func() {
			kubeClient.list = func(context.Context) error {
				return apierrors.NewTimeoutError("failed waiting for *v1.VerticalPodAutoscaler Informer to sync", 0)
			}
			reader.lookups = []crdLookup{crdForbidden, crdForbidden}

			Expect(ExpectSingletonReconcileFailed(ctx, controller)).To(Satisfy(apierrors.IsTimeout))

			Expect(reader.lookups).To(BeEmpty())
			hydrationCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			Expect(store.Hydrated(hydrationCtx)).To(BeFalse())
		})

		It("should mark the store hydrated when the CRD lookup is forbidden and the List reports no match", func() {
			_, err := informers.GetInformer(ctx, &vpav1.VerticalPodAutoscaler{})
			Expect(err).ToNot(HaveOccurred())
			kubeClient.list = func(context.Context) error {
				return &meta.NoKindMatchError{GroupKind: vpaGVK.GroupKind(), SearchedVersions: []string{vpaGVK.Version}}
			}
			reader.lookups = []crdLookup{crdForbidden}

			result := ExpectSingletonReconciled(ctx, controller)

			Expect(reader.lookups).To(BeEmpty())
			Expect(result.RequeueAfter).To(Equal(time.Minute))
			Expect(store.Hydrated(ctx)).To(BeTrue())
			Expect(informers.InformersByGVK).ToNot(HaveKey(vpaGVK))
		})

		It("should fall back to the List when the CRD lookup fails", func() {
			vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
			ExpectApplied(ctx, env.Client, vpa)
			test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
				"main": {corev1.ResourceCPU: resource.MustParse("500m")},
			})
			reader.lookups = []crdLookup{crdUnavailable}

			ExpectSingletonReconciled(ctx, controller)

			_, ok := store.Get(target)
			Expect(ok).To(BeTrue())
			Expect(store.Hydrated(ctx)).To(BeTrue())
		})
	})
})

// These specs delete the VPA CRD from the suite's API server, so they run alone and put it back afterwards.
var _ = Describe("VPA Controller with a real cache when the CRD is deleted", Serial, func() {
	BeforeEach(func() {
		DeferCleanup(ensureVPACRD)
	})

	It("should remove the informer stuck on the deleted CRD, and list VPAs again once it's back", func() {
		// A Karpenter process whose RESTMapper resolved VPAs while the CRD was installed, built the way ctrl.NewManager
		// builds it
		httpClient := lo.Must(rest.HTTPClientFor(env.Config))
		mapper := lo.Must(apiutil.NewDynamicRESTMapper(env.Config, httpClient))
		informers := lo.Must(cache.New(env.Config, cache.Options{Scheme: env.Client.Scheme(), Mapper: mapper, HTTPClient: httpClient}))
		cachedClient := lo.Must(client.New(env.Config, client.Options{Scheme: env.Client.Scheme(), Mapper: mapper, HTTPClient: httpClient, Cache: &client.CacheOptions{Reader: informers}}))
		cacheCtx, cancel := context.WithCancel(ctx)
		DeferCleanup(cancel)
		go func() {
			defer GinkgoRecover()
			_ = informers.Start(cacheCtx)
		}()
		Expect(informers.WaitForCacheSync(cacheCtx)).To(BeTrue())
		_, err := mapper.RESTMapping(vpaGVK.GroupKind(), vpaGVK.Version)
		Expect(err).ToNot(HaveOccurred())

		Expect(env.Client.Delete(ctx, &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: vpaCRDName}})).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(env.Client.Get(ctx, client.ObjectKey{Name: vpaCRDName}, &apiextensionsv1.CustomResourceDefinition{}))
		}).WithTimeout(time.Minute).Should(BeTrue())

		DeferCleanup(func(timeout time.Duration) { informer.VPAListTimeout = timeout }, informer.VPAListTimeout)
		informer.VPAListTimeout = 2 * time.Second
		// The CRD was deleted just after the first lookup, so the List starts an informer that can't sync
		c := informer.NewVPAController(cachedClient, &crdReader{Reader: env.Client, lookups: []crdLookup{crdStillPresent}}, informers, store, cluster, recorder)
		result := ExpectSingletonReconciled(ctx, c)
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		Expect(store.Hydrated(ctx)).To(BeTrue())
		// readyz waits for every informer in the cache to sync
		syncCtx, syncCancel := context.WithTimeout(ctx, 2*time.Second)
		defer syncCancel()
		Expect(informers.WaitForCacheSync(syncCtx)).To(BeTrue())
		start := time.Now()
		ExpectSingletonReconciled(ctx, c)
		Expect(time.Since(start)).To(BeNumerically("<", informer.VPAListTimeout))

		ensureVPACRD()
		vpa := test.VerticalPodAutoscaler(test.VerticalPodAutoscalerOptions{TargetRef: targetRef})
		ExpectApplied(ctx, env.Client, vpa)
		test.UpdateVPARecommendation(ctx, env.Client, vpa, map[string]corev1.ResourceList{
			"main": {corev1.ResourceCPU: resource.MustParse("500m")},
		})
		Eventually(func(g Gomega) {
			_, err := c.Reconcile(ctx)
			g.Expect(err).ToNot(HaveOccurred())
			_, ok := store.Get(target)
			g.Expect(ok).To(BeTrue())
		}).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
	})
})

// ensureVPACRD installs the VPA CRD if it's missing, or once it finishes deleting, and waits until VPAs are served.
func ensureVPACRD() {
	Eventually(func() bool {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := env.Client.Get(ctx, client.ObjectKey{Name: vpaCRDName}, crd)
		return apierrors.IsNotFound(err) || (err == nil && crd.DeletionTimestamp.IsZero())
	}).WithTimeout(time.Minute).Should(BeTrue())
	if err := env.Client.Get(ctx, client.ObjectKey{Name: vpaCRDName}, &apiextensionsv1.CustomResourceDefinition{}); apierrors.IsNotFound(err) {
		Expect(env.Client.Create(ctx, testcrds.CRDs[0].DeepCopy())).To(Succeed())
	}
	Eventually(func() error {
		return env.Client.List(ctx, &vpav1.VerticalPodAutoscalerList{})
	}).WithTimeout(time.Minute).Should(Succeed())
}

// crdLookup answers a lookup of the VPA CRD.
type crdLookup func(ctx context.Context, r client.Reader, obj client.Object) error

var (
	// crdStillPresent reports the CRD as present after it's been deleted, as a lookup that ran just before the deletion
	// would
	crdStillPresent crdLookup = func(context.Context, client.Reader, client.Object) error { return nil }
	crdNotFound     crdLookup = func(context.Context, client.Reader, client.Object) error {
		return apierrors.NewNotFound(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, vpaCRDName)
	}
	crdForbidden crdLookup = func(context.Context, client.Reader, client.Object) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, vpaCRDName, fmt.Errorf("rbac"))
	}
	crdUnavailable crdLookup = func(context.Context, client.Reader, client.Object) error {
		return apierrors.NewServiceUnavailable("unavailable")
	}
	crdTerminating crdLookup = func(ctx context.Context, r client.Reader, obj client.Object) error {
		if err := r.Get(ctx, client.ObjectKey{Name: vpaCRDName}, obj); err != nil {
			return err
		}
		obj.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
		return nil
	}
)

// blockUntilDone answers a List the way a cached List against a deleted CRD does: it blocks until its context is done,
// then reports the informer didn't sync.
func blockUntilDone(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return apierrors.NewTimeoutError("failed waiting for *v1.VerticalPodAutoscaler Informer to sync", 0)
	case <-time.After(10 * time.Second):
		return fmt.Errorf("list context wasn't bounded")
	}
}

// crdReader counts VPA CRD lookups and answers them from lookups in order, then from the API server once they run out.
// Like a real reader, it fails a lookup on a done context without consuming it.
type crdReader struct {
	client.Reader
	gets    int
	lookups []crdLookup
}

func (r *crdReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Name == vpaCRDName {
		r.gets++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if key.Name != vpaCRDName || len(r.lookups) == 0 {
		return r.Reader.Get(ctx, key, obj, opts...)
	}
	lookup := r.lookups[0]
	r.lookups = r.lookups[1:]
	return lookup(ctx, r.Reader, obj)
}

// vpaListClient counts VPA Lists and answers them with list when it's set.
type vpaListClient struct {
	client.Client
	lists int
	list  func(ctx context.Context) error
}

func (c *vpaListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*vpav1.VerticalPodAutoscalerList); ok {
		c.lists++
		if c.list != nil {
			return c.list(ctx)
		}
	}
	return c.Client.List(ctx, list, opts...)
}
