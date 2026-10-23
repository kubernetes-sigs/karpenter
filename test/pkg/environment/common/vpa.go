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

package common

import (
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	utilruntime.Must(vpav1.AddToScheme(scheme.Scheme))
}

func VPA(name, deploymentName, namespace string) *vpav1.VerticalPodAutoscaler {
	return &vpav1.VerticalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: vpav1.VerticalPodAutoscalerSpec{
			TargetRef: &autoscalingv1.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			UpdatePolicy: &vpav1.PodUpdatePolicy{
				UpdateMode: lo.ToPtr(vpav1.UpdateModeRecreate),
			},
		},
	}
}

func VPAForDaemonSet(name, daemonSetName, namespace string) *vpav1.VerticalPodAutoscaler {
	return &vpav1.VerticalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: vpav1.VerticalPodAutoscalerSpec{
			TargetRef: &autoscalingv1.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "DaemonSet",
				Name:       daemonSetName,
			},
			UpdatePolicy: &vpav1.PodUpdatePolicy{
				UpdateMode: lo.ToPtr(vpav1.UpdateModeRecreate),
			},
		},
	}
}

func VPAForJob(name, jobName, namespace string) *vpav1.VerticalPodAutoscaler {
	return &vpav1.VerticalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: vpav1.VerticalPodAutoscalerSpec{
			TargetRef: &autoscalingv1.CrossVersionObjectReference{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       jobName,
			},
			UpdatePolicy: &vpav1.PodUpdatePolicy{
				UpdateMode: lo.ToPtr(vpav1.UpdateModeRecreate),
			},
		},
	}
}

func VPAWithStartupBoost(name, deploymentName, namespace string, factor int32) *vpav1.VerticalPodAutoscaler {
	return &vpav1.VerticalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: vpav1.VerticalPodAutoscalerSpec{
			TargetRef: &autoscalingv1.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			UpdatePolicy: &vpav1.PodUpdatePolicy{
				UpdateMode: lo.ToPtr(vpav1.UpdateModeRecreate),
			},
			StartupBoost: &vpav1.StartupBoost{
				CPU: &vpav1.GenericStartupBoost{
					Type:   vpav1.FactorStartupBoostType,
					Factor: &factor,
				},
			},
		},
	}
}

func (env *Environment) UpdateVPAStatus(vpa *vpav1.VerticalPodAutoscaler, containerName string, recommendation corev1.ResourceList) {
	Eventually(func(g Gomega) {
		fetched := &vpav1.VerticalPodAutoscaler{}
		g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(vpa), fetched)).To(Succeed())
		fetched.Status.Recommendation = &vpav1.RecommendedPodResources{
			ContainerRecommendations: []vpav1.RecommendedContainerResources{
				{
					ContainerName: containerName,
					Target:        recommendation,
				},
			},
		}
		g.Expect(env.Client.Status().Update(env, fetched)).To(Succeed())
	}).Should(Succeed())
}

func (env *Environment) CleanupVPAs() {
	vpaList := &vpav1.VerticalPodAutoscalerList{}
	if err := env.Client.List(env, vpaList); err == nil {
		for i := range vpaList.Items {
			_ = env.Client.Delete(env, &vpaList.Items[i])
		}
	}
}

func (env *Environment) VPACRDExists() bool {
	vpaList := &vpav1.VerticalPodAutoscalerList{}
	return env.Client.List(env, vpaList, &client.ListOptions{Limit: 1}) == nil
}
