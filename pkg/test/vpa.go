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

package test

import (
	"context"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type VerticalPodAutoscalerOptions struct {
	metav1.ObjectMeta
	TargetRef      autoscalingv1.CrossVersionObjectReference
	UpdatePolicy   *vpav1.PodUpdatePolicy
	ResourcePolicy *vpav1.PodResourcePolicy
}

func VerticalPodAutoscaler(opts ...VerticalPodAutoscalerOptions) *vpav1.VerticalPodAutoscaler {
	var options VerticalPodAutoscalerOptions
	if len(opts) > 0 {
		options = opts[0]
	}
	objectMeta := NamespacedObjectMeta(options.ObjectMeta)

	return &vpav1.VerticalPodAutoscaler{
		ObjectMeta: objectMeta,
		Spec: vpav1.VerticalPodAutoscalerSpec{
			TargetRef: &autoscalingv1.CrossVersionObjectReference{
				APIVersion: options.TargetRef.APIVersion,
				Kind:       options.TargetRef.Kind,
				Name:       options.TargetRef.Name,
			},
			UpdatePolicy:   options.UpdatePolicy,
			ResourcePolicy: options.ResourcePolicy,
		},
	}
}

// UpdateVPARecommendation updates the status subresource of a VPA with the given recommendations.
func UpdateVPARecommendation(ctx context.Context, c client.Client, vpa *vpav1.VerticalPodAutoscaler, recommendations map[string]corev1.ResourceList) {
	fetched := &vpav1.VerticalPodAutoscaler{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(vpa), fetched); err != nil {
		panic(err)
	}
	var recs []vpav1.RecommendedContainerResources
	for name, resources := range recommendations {
		recs = append(recs, vpav1.RecommendedContainerResources{
			ContainerName: name,
			Target:        resources,
		})
	}
	fetched.Status.Recommendation = &vpav1.RecommendedPodResources{ContainerRecommendations: recs}
	if err := c.Status().Update(ctx, fetched); err != nil {
		panic(err)
	}
}
