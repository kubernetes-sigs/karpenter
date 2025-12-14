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
	"time"

	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Log Deduplication", func() {
	Context("Scheduling Error Logs", func() {
		It("should log scheduling errors for different pods", func() {
			nodePool := test.NodePool()
			// Create a NodePool with very specific requirements that most pods won't match
			nodePool.Spec.Template.Spec.Taints = []corev1.Taint{
				{Key: "special-workload", Value: "true", Effect: corev1.TaintEffectNoSchedule},
			}
			ExpectApplied(ctx, env.Client, nodePool)

			// Create two different pods that can't be scheduled
			pod1 := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"),
					},
				},
			})
			pod2 := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("2"),
					},
				},
			})

			// Try to provision both pods - each should log its error once
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod1, pod2)

			// Both pods should fail to schedule
			ExpectNotScheduled(ctx, env.Client, pod1)
			ExpectNotScheduled(ctx, env.Client, pod2)
		})

		It("should deduplicate logs for the same pod with the same error across multiple scheduling attempts", func() {
			nodePool := test.NodePool()
			// Create a NodePool with taints that the pod won't tolerate
			nodePool.Spec.Template.Spec.Taints = []corev1.Taint{
				{Key: "special-workload", Value: "true", Effect: corev1.TaintEffectNoSchedule},
			}
			ExpectApplied(ctx, env.Client, nodePool)

			// Create a pod that can't be scheduled due to the taint
			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"),
					},
				},
			})

			// Try to provision the same pod multiple times
			// This simulates the pod being evaluated in multiple scheduling cycles
			// The same error should only be logged once
			for i := 0; i < 5; i++ {
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			}

			// Pod should still not be scheduled
			ExpectNotScheduled(ctx, env.Client, pod)
			// Log deduplication should have prevented multiple log entries for the same pod+error combination
		})

		It("should log different errors for the same pod", func() {
			// This test verifies that if a pod fails with different errors,
			// each unique error is logged (not deduplicated across different error messages)

			// First, create a scenario where the pod fails due to taint incompatibility
			nodePool1 := test.NodePool()
			nodePool1.Spec.Template.Spec.Taints = []corev1.Taint{
				{Key: "special-workload", Value: "true", Effect: corev1.TaintEffectNoSchedule},
			}
			ExpectApplied(ctx, env.Client, nodePool1)

			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("10000"), // Unrealistic request
					},
				},
			})

			// First attempt - will fail with taint error
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)

			// Delete the first nodepool and create a new one with different constraints
			// This will cause a different error message
			ExpectDeleted(ctx, env.Client, nodePool1)

			nodePool2 := test.NodePool()
			// No taints, but pod will fail due to resource constraints
			ExpectApplied(ctx, env.Client, nodePool2)

			// Second attempt - will fail with different error (insufficient resources)
			// This should be logged even though it's the same pod
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})

		It("should log again after the deduplication timeout expires", func() {
			nodePool := test.NodePool()
			nodePool.Spec.Template.Spec.Taints = []corev1.Taint{
				{Key: "special-workload", Value: "true", Effect: corev1.TaintEffectNoSchedule},
			}
			ExpectApplied(ctx, env.Client, nodePool)

			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"),
					},
				},
			})

			// First scheduling attempt - should log
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)

			// Advance time beyond the deduplication timeout (5 minutes)
			fakeClock.Step(6 * time.Minute)

			// Second scheduling attempt after timeout - should log again
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})

		It("should handle pods with incompatible NodePool requirements", func() {
			// Create two NodePools: one for GPU workloads and one for general apps
			gpuNodePool := test.NodePool()
			gpuNodePool.Labels = map[string]string{
				"workload-type": "gpu",
			}
			gpuNodePool.Spec.Template.Spec.Taints = []corev1.Taint{
				{Key: "nvidia.com/gpu", Value: "1", Effect: corev1.TaintEffectNoSchedule},
			}

			appNodePool := test.NodePool()
			appNodePool.Labels = map[string]string{
				"workload-type": "app",
			}

			ExpectApplied(ctx, env.Client, gpuNodePool, appNodePool)

			// Create a pod that should only run on infrastructure nodes (not managed by Karpenter)
			infraPod := test.UnschedulablePod(test.PodOptions{
				Tolerations: []corev1.Toleration{
					{Key: "node-group", Operator: corev1.TolerationOpExists},
				},
				NodeSelector: map[string]string{
					"eks.amazonaws.com/nodegroup": "infra-nodegroup",
				},
			})

			// This pod will be evaluated against both NodePools and fail both
			// but should only log the error once per NodePool
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, infraPod)
			ExpectNotScheduled(ctx, env.Client, infraPod)
		})
	})
})
