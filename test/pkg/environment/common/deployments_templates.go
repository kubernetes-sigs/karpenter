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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

// DeploymentOptionModifier is a function that modifies DeploymentOptions
type DeploymentOptionModifier func(*test.DeploymentOptions)

// CreateDeploymentOptions creates a test.DeploymentOptions with the specified parameters
// and applies any provided modifiers. This provides a clean, extensible way to create
// deployment configurations for tests.
//
// Example usage:
//
//	// Simple deployment
//	opts := CreateDeploymentOptions("my-app", 10, "100m", "128Mi")
//
//	// With modifiers
//	opts := CreateDeploymentOptions("my-app", 10, "100m", "128Mi",
//	    WithHostnameSpread(),
//	    WithDoNotDisrupt(),
//	    WithLabels(map[string]string{"tier": "frontend"}))
//
//	// Use with test.Deployment
//	deployment := test.Deployment(opts)
func CreateDeploymentOptions(name string, replicas int32, cpuRequest, memoryRequest string, opts ...DeploymentOptionModifier) test.DeploymentOptions {
	// Create base deployment options
	deploymentOpts := test.DeploymentOptions{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Replicas: replicas,
		PodOptions: test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app":               name,
					test.DiscoveryLabel: "unspecified",
				},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpuRequest),
					corev1.ResourceMemory: resource.MustParse(memoryRequest),
				},
			},
		},
	}

	// Apply all modifiers
	for _, opt := range opts {
		opt(&deploymentOpts)
	}

	return deploymentOpts
}

// WithLabels adds or merges labels to the pod template
func WithLabels(labels map[string]string) DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		if opts.PodOptions.ObjectMeta.Labels == nil {
			opts.PodOptions.ObjectMeta.Labels = make(map[string]string)
		}
		for k, v := range labels {
			opts.PodOptions.ObjectMeta.Labels[k] = v
		}
	}
}

// WithAnnotations adds or merges annotations to the pod template
func WithAnnotations(annotations map[string]string) DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		if opts.PodOptions.ObjectMeta.Annotations == nil {
			opts.PodOptions.ObjectMeta.Annotations = make(map[string]string)
		}
		for k, v := range annotations {
			opts.PodOptions.ObjectMeta.Annotations[k] = v
		}
	}
}

// WithDoNotDisrupt adds the do-not-disrupt annotation to prevent Karpenter disruption
func WithDoNotDisrupt() DeploymentOptionModifier {
	return WithAnnotations(map[string]string{
		v1.DoNotDisruptAnnotationKey: "true",
	})
}

// WithHostnameSpread adds topology spread constraints to spread pods across hostnames
func WithHostnameSpread() DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		// Get the current labels to use in the label selector
		labels := opts.PodOptions.ObjectMeta.Labels
		if labels == nil {
			labels = map[string]string{"app": opts.ObjectMeta.Name}
		}

		constraint := corev1.TopologySpreadConstraint{
			MaxSkew:           1,
			TopologyKey:       corev1.LabelHostname,
			WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
		}

		opts.PodOptions.TopologySpreadConstraints = append(
			opts.PodOptions.TopologySpreadConstraints,
			constraint,
		)
	}
}

// WithZoneSpread adds topology spread constraints to spread pods across zones
func WithZoneSpread() DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		// Get the current labels to use in the label selector
		labels := opts.PodOptions.ObjectMeta.Labels
		if labels == nil {
			labels = map[string]string{"app": opts.ObjectMeta.Name}
		}

		constraint := corev1.TopologySpreadConstraint{
			MaxSkew:           1,
			TopologyKey:       corev1.LabelTopologyZone,
			WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
		}

		opts.PodOptions.TopologySpreadConstraints = append(
			opts.PodOptions.TopologySpreadConstraints,
			constraint,
		)
	}
}

// WithPodAntiAffinity adds pod anti-affinity to prevent pods from being scheduled on the same topology
func WithPodAntiAffinity(topologyKey string) DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		// Get the current labels to use in the label selector
		labels := opts.PodOptions.ObjectMeta.Labels
		if labels == nil {
			labels = map[string]string{"app": opts.ObjectMeta.Name}
		}

		antiAffinityTerm := corev1.PodAffinityTerm{
			TopologyKey: topologyKey,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
		}

		opts.PodOptions.PodAntiRequirements = append(
			opts.PodOptions.PodAntiRequirements,
			antiAffinityTerm,
		)
	}
}

// WithTopologySpreadConstraints adds custom topology spread constraints
func WithTopologySpreadConstraints(constraints []corev1.TopologySpreadConstraint) DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		opts.PodOptions.TopologySpreadConstraints = append(
			opts.PodOptions.TopologySpreadConstraints,
			constraints...,
		)
	}
}

// WithResourceLimits adds resource limits to the pod containers
func WithResourceLimits(cpuLimit, memoryLimit string) DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		if opts.PodOptions.ResourceRequirements.Limits == nil {
			opts.PodOptions.ResourceRequirements.Limits = make(corev1.ResourceList)
		}
		opts.PodOptions.ResourceRequirements.Limits[corev1.ResourceCPU] = resource.MustParse(cpuLimit)
		opts.PodOptions.ResourceRequirements.Limits[corev1.ResourceMemory] = resource.MustParse(memoryLimit)
	}
}

// WithPriorityClass sets the priority class for the pods
func WithPriorityClass(priorityClassName string) DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		opts.PodOptions.PriorityClassName = priorityClassName
	}
}

// WithNodeSelector adds node selector requirements
func WithNodeSelector(nodeSelector map[string]string) DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		if opts.PodOptions.NodeSelector == nil {
			opts.PodOptions.NodeSelector = make(map[string]string)
		}
		for k, v := range nodeSelector {
			opts.PodOptions.NodeSelector[k] = v
		}
	}
}

// WithTolerations adds tolerations to the pod spec
func WithTolerations(tolerations []corev1.Toleration) DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		opts.PodOptions.Tolerations = append(opts.PodOptions.Tolerations, tolerations...)
	}
}

// WithTerminationGracePeriod sets the termination grace period for pods
func WithTerminationGracePeriod(seconds int64) DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		opts.PodOptions.TerminationGracePeriodSeconds = &seconds
	}
}

// Predefined resource configurations for common use cases

// WithSmallResources applies small resource profile (950m CPU, 3900Mi memory)
func WithSmallResources() DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		opts.PodOptions.ResourceRequirements.Requests[corev1.ResourceCPU] = resource.MustParse("950m")
		opts.PodOptions.ResourceRequirements.Requests[corev1.ResourceMemory] = resource.MustParse("3900Mi")
	}
}

// WithLargeResources applies large resource profile (3800m CPU, 31Gi memory)
func WithLargeResources() DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		opts.PodOptions.ResourceRequirements.Requests[corev1.ResourceCPU] = resource.MustParse("3800m")
		opts.PodOptions.ResourceRequirements.Requests[corev1.ResourceMemory] = resource.MustParse("31Gi")
	}
}

// WithMinimalResources applies minimal resource profile (100m CPU, 128Mi memory)
func WithMinimalResources() DeploymentOptionModifier {
	return func(opts *test.DeploymentOptions) {
		opts.PodOptions.ResourceRequirements.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
		opts.PodOptions.ResourceRequirements.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
	}
}

// Convenience functions for common deployment patterns

// CreateSmallDeployment creates a deployment with small resource profile
func CreateSmallDeployment(name string, replicas int32, opts ...DeploymentOptionModifier) test.DeploymentOptions {
	modifiers := append([]DeploymentOptionModifier{WithSmallResources()}, opts...)
	return CreateDeploymentOptions(name, replicas, "100m", "128Mi", modifiers...)
}

// CreateLargeDeployment creates a deployment with large resource profile
func CreateLargeDeployment(name string, replicas int32, opts ...DeploymentOptionModifier) test.DeploymentOptions {
	modifiers := append([]DeploymentOptionModifier{WithLargeResources()}, opts...)
	return CreateDeploymentOptions(name, replicas, "100m", "128Mi", modifiers...)
}

// CreateSpreadDeployment creates a deployment with hostname spreading
func CreateSpreadDeployment(name string, replicas int32, cpuRequest, memoryRequest string, opts ...DeploymentOptionModifier) test.DeploymentOptions {
	modifiers := append([]DeploymentOptionModifier{WithHostnameSpread()}, opts...)
	return CreateDeploymentOptions(name, replicas, cpuRequest, memoryRequest, modifiers...)
}

// CreateProtectedDeployment creates a deployment with do-not-disrupt annotation
func CreateProtectedDeployment(name string, replicas int32, cpuRequest, memoryRequest string, opts ...DeploymentOptionModifier) test.DeploymentOptions {
	modifiers := append([]DeploymentOptionModifier{WithDoNotDisrupt()}, opts...)
	return CreateDeploymentOptions(name, replicas, cpuRequest, memoryRequest, modifiers...)
}
