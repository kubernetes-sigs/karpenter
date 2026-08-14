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
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/state/prediction"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("PredictedRequests", func() {
	var store *prediction.Store

	BeforeEach(func() {
		store = prediction.NewStore()
	})

	It("should return currentRequests unchanged when store is nil", func() {
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		})
		result := scheduling.PredictedRequests(ctx, env.Client, nil, pod, nil)
		Expect(result.Cpu().Cmp(resource.MustParse("100m"))).To(Equal(0))
		Expect(result.Pods().Cmp(resource.MustParse("1"))).To(Equal(0))
	})

	It("should return currentRequests when pod has no owner", func() {
		store.Set(types.NamespacedName{Name: "dummy"}, types.UID("unrelated"), &prediction.Prediction{
			Containers: map[string]corev1.ResourceList{"c": {corev1.ResourceCPU: resource.MustParse("1")}},
		}, env.Clock.Now())
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			},
		})
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
		Expect(result.Cpu().Cmp(resource.MustParse("50m"))).To(Equal(0))
	})

	It("should return currentRequests when owner exists but no prediction is stored", func() {
		store.Set(types.NamespacedName{Name: "dummy"}, types.UID("unrelated"), &prediction.Prediction{
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
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
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
			dep.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			}},
			env.Clock.Now(),
		)
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
		Expect(result.Cpu().Cmp(resource.MustParse("600m"))).To(Equal(0))
		Expect(result.Memory().Cmp(resource.MustParse("256Mi"))).To(Equal(0))
		Expect(result.Pods().Cmp(resource.MustParse("1"))).To(Equal(0))
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
			ss.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceMemory: resource.MustParse("1Gi")},
			}},
			env.Clock.Now(),
		)
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
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
			ds.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("250m")},
			}},
			env.Clock.Now(),
		)
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
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
			types.UID("cronjob-uid"),
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("2")},
			}},
			env.Clock.Now(),
		)
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
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
			job.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceMemory: resource.MustParse("4Gi")},
			}},
			env.Clock.Now(),
		)
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
		Expect(result.Memory().Cmp(resource.MustParse("4Gi"))).To(Equal(0))
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
			rs.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("300m")},
			}},
			env.Clock.Now(),
		)
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
		Expect(result.Cpu().Cmp(resource.MustParse("300m"))).To(Equal(0))
	})

	It("should return currentRequests when ReplicaSet owner is not found", func() {
		store.Set(types.NamespacedName{Name: "dummy"}, types.UID("unrelated"), &prediction.Prediction{
			Containers: map[string]corev1.ResourceList{"c": {corev1.ResourceCPU: resource.MustParse("1")}},
		}, env.Clock.Now())
		pod := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "ghost-rs-deleted", Controller: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		})
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
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

		// Only predict "sidecar", leave main unpredicted
		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-multi"},
			dep.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				"sidecar": {corev1.ResourceCPU: resource.MustParse("200m")},
			}},
			env.Clock.Now(),
		)
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
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

		// Prediction includes memory even though container doesn't request it
		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-newres"},
			dep.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}},
			env.Clock.Now(),
		)
		result := scheduling.PredictedRequests(ctx, env.Client, store, pod, nil)
		Expect(result.Cpu().Cmp(resource.MustParse("200m"))).To(Equal(0))
		Expect(result.Memory().Cmp(resource.MustParse("128Mi"))).To(Equal(0))
	})
})
