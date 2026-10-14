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

package scheduling_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/state/prediction"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("PredictedRequests", func() {
	var store *prediction.Store

	BeforeEach(func() {
		store = prediction.NewStore(env.Client)
		metaScheme := runtime.NewScheme()
		Expect(metav1.AddMetaToScheme(metaScheme)).To(Succeed())
		Expect(store.RegisterTargetKinds(ctx, &informertest.FakeInformers{Scheme: metaScheme}, prediction.SupportedTargets.UnsortedList()...)).To(Succeed())
	})

	It("should return currentRequests when pod has no owner", func() {
		store.Set(types.NamespacedName{Name: "dummy"}, prediction.WorkloadReference{NamespacedName: types.NamespacedName{Name: "unrelated"}}, &prediction.Prediction{
			Containers: map[string]corev1.ResourceList{"c": {corev1.ResourceCPU: resource.MustParse("1")}},
		}, env.Clock.Now())
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			},
		})
		result := store.PredictedRequests(ctx, nil, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("50m"))).To(Equal(0))
	})

	It("should return currentRequests when owner exists but no prediction is stored", func() {
		store.Set(types.NamespacedName{Name: "dummy"}, prediction.WorkloadReference{NamespacedName: types.NamespacedName{Name: "unrelated"}}, &prediction.Prediction{
			Containers: map[string]corev1.ResourceList{"c": {corev1.ResourceCPU: resource.MustParse("1")}},
		}, env.Clock.Now())
		dep := test.Deployment()
		ExpectApplied(ctx, env.Client, dep)

		rs := test.ReplicaSet()
		rs.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: lo.ToPtr(true),
		}}
		ExpectApplied(ctx, env.Client, rs)

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		})
		result := store.PredictedRequests(ctx, nil, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("100m"))).To(Equal(0))
	})

	It("should resolve pod through replicaset to deployment and replace requests with predicted values", func() {
		dep := test.Deployment()
		ExpectApplied(ctx, env.Client, dep)

		rs := test.ReplicaSet()
		rs.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: lo.ToPtr(true),
		}}
		ExpectApplied(ctx, env.Client, rs)

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			},
			Overhead: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-web"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: client.ObjectKeyFromObject(dep)},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			}},
			env.Clock.Now(),
		)
		result := store.PredictedRequests(ctx, nil, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("600m"))).To(Equal(0))
		Expect(result.Memory().Cmp(resource.MustParse("256Mi"))).To(Equal(0))
		Expect(result.Pods().Cmp(resource.MustParse("1"))).To(Equal(0))
	})

	It("should resolve pods sharing an owner from the cached owner resolution", func() {
		dep := test.Deployment()
		ExpectApplied(ctx, env.Client, dep)

		rs := test.ReplicaSet()
		rs.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: lo.ToPtr(true),
		}}
		ExpectApplied(ctx, env.Client, rs)

		pods := test.UnschedulablePods(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		}, 2)
		pods[1].Spec.Containers[0].Name = pods[0].Spec.Containers[0].Name
		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-web"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: client.ObjectKeyFromObject(dep)},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pods[0].Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("500m")},
			}},
			env.Clock.Now(),
		)
		cachedOwnerResolutions := prediction.OwnerResolutions{}
		result := store.PredictedRequests(ctx, cachedOwnerResolutions, pods[0])
		Expect(result.Cpu().Cmp(resource.MustParse("500m"))).To(Equal(0))

		ExpectDeleted(ctx, env.Client, rs)
		result = store.PredictedRequests(ctx, cachedOwnerResolutions, pods[1])
		Expect(result.Cpu().Cmp(resource.MustParse("500m"))).To(Equal(0))
	})

	It("should match a prediction by workload name regardless of the workload's UID", func() {
		ss := test.StatefulSet()
		ExpectApplied(ctx, env.Client, ss)

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "StatefulSet", Name: ss.Name, UID: "recreated-uid", Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		})
		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-db"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "apps", Kind: "StatefulSet"}, NamespacedName: client.ObjectKeyFromObject(ss)},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("750m")},
			}},
			env.Clock.Now(),
		)
		result := store.PredictedRequests(ctx, nil, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("750m"))).To(Equal(0))
	})

	It("should resolve pod to statefulset directly", func() {
		ss := test.StatefulSet()
		ExpectApplied(ctx, env.Client, ss)

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "StatefulSet", Name: ss.Name, UID: ss.UID, Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-db"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "apps", Kind: "StatefulSet"}, NamespacedName: client.ObjectKeyFromObject(ss)},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceMemory: resource.MustParse("1Gi")},
			}},
			env.Clock.Now(),
		)
		result := store.PredictedRequests(ctx, nil, pod)
		Expect(result.Memory().Cmp(resource.MustParse("1Gi"))).To(Equal(0))
	})

	It("should resolve pod to daemonset directly", func() {
		ds := test.DaemonSet()
		ExpectApplied(ctx, env.Client, ds)

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-fluentd"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "apps", Kind: "DaemonSet"}, NamespacedName: client.ObjectKeyFromObject(ds)},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("250m")},
			}},
			env.Clock.Now(),
		)
		result := store.PredictedRequests(ctx, nil, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("250m"))).To(Equal(0))
	})

	It("should resolve pod through job to cronjob", func() {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: "etl-28400000", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "batch/v1", Kind: "CronJob", Name: "etl", UID: "cronjob-uid", Controller: lo.ToPtr(true),
				}},
			},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers:    []corev1.Container{{Name: "worker", Image: "public.ecr.aws/eks-distro/kubernetes/pause:3.2"}},
				RestartPolicy: corev1.RestartPolicyNever,
			}}},
		}
		ExpectApplied(ctx, env.Client, job)

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-etl"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "batch", Kind: "CronJob"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "etl"}},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("2")},
			}},
			env.Clock.Now(),
		)
		result := store.PredictedRequests(ctx, nil, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("2"))).To(Equal(0))
	})

	It("should resolve pod to standalone job with no cronjob owner", func() {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "migration-v2", Namespace: "default"},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers:    []corev1.Container{{Name: "migrate", Image: "public.ecr.aws/eks-distro/kubernetes/pause:3.2"}},
				RestartPolicy: corev1.RestartPolicyNever,
			}}},
		}
		ExpectApplied(ctx, env.Client, job)

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-migration"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "batch", Kind: "Job"}, NamespacedName: client.ObjectKeyFromObject(job)},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceMemory: resource.MustParse("4Gi")},
			}},
			env.Clock.Now(),
		)
		result := store.PredictedRequests(ctx, nil, pod)
		Expect(result.Memory().Cmp(resource.MustParse("4Gi"))).To(Equal(0))
	})

	It("should resolve a job's pod to the job without looking it up when cronjobs aren't targeted", func() {
		jobStore := prediction.NewStore(env.Client)
		Expect(jobStore.RegisterTargetKinds(ctx, &informertest.FakeInformers{}, schema.GroupKind{Group: "batch", Kind: "Job"})).To(Succeed())

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: "not-in-the-api-server", UID: "job-uid", Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
		})
		jobStore.Set(
			types.NamespacedName{Namespace: pod.Namespace, Name: "vpa-job"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "batch", Kind: "Job"}, NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: "not-in-the-api-server"}},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceMemory: resource.MustParse("4Gi")},
			}},
			env.Clock.Now(),
		)
		result := jobStore.PredictedRequests(ctx, nil, pod)
		Expect(result.Memory().Cmp(resource.MustParse("4Gi"))).To(Equal(0))
	})

	It("should keep current requests for a pod whose workload kind isn't targeted", func() {
		deploymentStore := prediction.NewStore(env.Client)
		metaScheme := runtime.NewScheme()
		Expect(metav1.AddMetaToScheme(metaScheme)).To(Succeed())
		Expect(deploymentStore.RegisterTargetKinds(ctx, &informertest.FakeInformers{Scheme: metaScheme}, schema.GroupKind{Group: "apps", Kind: "Deployment"})).To(Succeed())

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: "migrate", UID: "job-uid", Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
		})
		deploymentStore.Set(
			types.NamespacedName{Namespace: pod.Namespace, Name: "vpa-job"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "batch", Kind: "Job"}, NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: "migrate"}},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceMemory: resource.MustParse("4Gi")},
			}},
			env.Clock.Now(),
		)
		result := deploymentStore.PredictedRequests(ctx, nil, pod)
		Expect(result.Memory().Cmp(resource.MustParse("1Gi"))).To(Equal(0))
	})

	It("should resolve a replicaset's pod to the replicaset without looking it up when deployments aren't targeted", func() {
		rsStore := prediction.NewStore(env.Client)
		Expect(rsStore.RegisterTargetKinds(ctx, &informertest.FakeInformers{}, schema.GroupKind{Group: "apps", Kind: "ReplicaSet"})).To(Succeed())

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "not-in-the-api-server", UID: "rs-uid", Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		})
		rsStore.Set(
			types.NamespacedName{Namespace: pod.Namespace, Name: "vpa-rs"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "apps", Kind: "ReplicaSet"}, NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: "not-in-the-api-server"}},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("500m")},
			}},
			env.Clock.Now(),
		)
		result := rsStore.PredictedRequests(ctx, nil, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("500m"))).To(Equal(0))
	})

	It("should resolve pod to standalone replicaset with no deployment owner", func() {
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-legacy"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "apps", Kind: "ReplicaSet"}, NamespacedName: client.ObjectKeyFromObject(rs)},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("300m")},
			}},
			env.Clock.Now(),
		)
		result := store.PredictedRequests(ctx, nil, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("300m"))).To(Equal(0))
	})

	It("should return currentRequests when ReplicaSet owner is not found and keep them for the run", func() {
		rs := test.ReplicaSet()
		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: "rs-uid", Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		})
		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-web"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "apps", Kind: "ReplicaSet"}, NamespacedName: client.ObjectKeyFromObject(rs)},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("500m")},
			}},
			env.Clock.Now(),
		)
		cachedOwnerResolutions := prediction.OwnerResolutions{}
		result := store.PredictedRequests(ctx, cachedOwnerResolutions, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("100m"))).To(Equal(0))

		ExpectApplied(ctx, env.Client, rs)
		result = store.PredictedRequests(ctx, cachedOwnerResolutions, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("100m"))).To(Equal(0))
	})

	It("should apply predictions per-container and sum them for multi-container pods", func() {
		dep := test.Deployment()
		ExpectApplied(ctx, env.Client, dep)

		rs := test.ReplicaSet()
		rs.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: lo.ToPtr(true),
		}}
		ExpectApplied(ctx, env.Client, rs)

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
			Overhead: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			},
			InitContainers: []corev1.Container{{
				Name:  "db-migrate",
				Image: "public.ecr.aws/eks-distro/kubernetes/pause:3.2",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				},
			}},
		})
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			Name:  "sidecar",
			Image: "public.ecr.aws/eks-distro/kubernetes/pause:3.2",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-multi"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: client.ObjectKeyFromObject(dep)},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				"sidecar": {corev1.ResourceCPU: resource.MustParse("200m")},
			}},
			env.Clock.Now(),
		)
		result := store.PredictedRequests(ctx, nil, pod)
		// containerSum: main keeps 500m + sidecar gets 200m = 700m
		// initMax: db-migrate = 2000m
		// max(700m, 2000m) + overhead(100m) = 2100m
		Expect(result.Cpu().Cmp(resource.MustParse("2100m"))).To(Equal(0))
		Expect(result.Pods().Cmp(resource.MustParse("1"))).To(Equal(0))
	})

	It("should include predicted resources not currently requested by the container", func() {
		dep := test.Deployment()
		ExpectApplied(ctx, env.Client, dep)

		rs := test.ReplicaSet()
		rs.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: lo.ToPtr(true),
		}}
		ExpectApplied(ctx, env.Client, rs)

		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-newres"},
			prediction.WorkloadReference{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: client.ObjectKeyFromObject(dep)},
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}},
			env.Clock.Now(),
		)
		result := store.PredictedRequests(ctx, nil, pod)
		Expect(result.Cpu().Cmp(resource.MustParse("200m"))).To(Equal(0))
		Expect(result.Memory().Cmp(resource.MustParse("128Mi"))).To(Equal(0))
	})

})
