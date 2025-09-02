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

package integration_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("StaticCapacity", func() {
	Context("Provisioning", func() {
		BeforeEach(func() {
			nodePool.Spec.Replicas = lo.ToPtr(int64(1))
			if env.IsDefaultNodeClassKWOK() {
				nodePool.Spec.Template.Spec.Requirements = append(nodePool.Spec.Template.Spec.Requirements, v1.NodeSelectorRequirementWithMinValues{
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values: []string{
							"c-16x-amd64-linux",
							"c-16x-arm64-linux",
						},
					},
				})
			}
		})

		It("should create static NodeClaims to meet desired replicas", func() {
			nodePool.Spec.Replicas = lo.ToPtr(int64(10))
			env.ExpectCreated(nodeClass, nodePool)
			env.EventuallyExpectInitializedNodeCount("==", 10)
			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 10)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)

			nodePool.Spec.Replicas = lo.ToPtr(int64(15))
			env.ExpectUpdated(nodePool)
			nodes := env.EventuallyExpectInitializedNodeCount("==", 15)
			nodeClaims = env.EventuallyExpectCreatedNodeClaimCount("==", 15)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)

			for _, node := range nodes {
				Expect(node.Labels).To(HaveKeyWithValue(v1.CapacityTypeLabelKey, v1.CapacityTypeOnDemand))
			}
		})

		It("should create static NodeClaim propagating all the NodePool spec details", func() {
			nodePool.Spec.Template.ObjectMeta = v1.ObjectMeta{
				Annotations: map[string]string{
					"custom-annotation": "custom-value",
				},
				Labels: map[string]string{
					"custom-label": "custom-value",
				},
			}
			nodePool.Spec.Template.Spec.Taints = []corev1.Taint{
				{
					Key:    "custom-taint",
					Effect: corev1.TaintEffectNoSchedule,
					Value:  "custom-value",
				},
				{
					Key:    "other-custom-taint",
					Effect: corev1.TaintEffectNoExecute,
					Value:  "other-custom-value",
				},
			}
			env.ExpectCreated(nodeClass, nodePool)
			node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
			Expect(node.Annotations).To(HaveKeyWithValue("custom-annotation", "custom-value"))
			Expect(node.Labels).To(HaveKeyWithValue("custom-label", "custom-value"))
			Expect(node.Spec.Taints).To(ContainElements(
				corev1.Taint{
					Key:    "custom-taint",
					Effect: corev1.TaintEffectNoSchedule,
					Value:  "custom-value",
				},
				corev1.Taint{
					Key:    "other-custom-taint",
					Effect: corev1.TaintEffectNoExecute,
					Value:  "other-custom-value",
				},
			))

			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 1)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)
		})

		It("should respect node limits when provisioning", func() {
			nodePool.Spec.Replicas = lo.ToPtr(int64(10))
			nodePool.Spec.Limits = v1.Limits{
				corev1.ResourceName("nodes"): resource.MustParse("5"),
			}
			env.ExpectCreated(nodeClass, nodePool)

			// Should only create 5 nodes due to limit
			env.EventuallyExpectInitializedNodeCount("==", 5)
			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 5)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)
		})
	})
	Context("Deprovisioning", func() {
		BeforeEach(func() {
			nodePool.Spec.Replicas = lo.ToPtr(int64(3))
			if env.IsDefaultNodeClassKWOK() {
				nodePool.Spec.Template.Spec.Requirements = append(nodePool.Spec.Template.Spec.Requirements, v1.NodeSelectorRequirementWithMinValues{
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values: []string{
							"c-16x-amd64-linux",
							"c-16x-arm64-linux",
						},
					},
				})
			}
			env.ExpectCreated(nodeClass, nodePool)
		})

		It("should scale down to zero", func() {
			// Initially should have 3 nodes
			env.EventuallyExpectInitializedNodeCount("==", 3)
			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 3)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)

			// Scale down to 0
			nodePool.Spec.Replicas = lo.ToPtr(int64(0))
			env.ExpectUpdated(nodePool)

			// Create no more
			env.EventuallyExpectInitializedNodeCount("==", 0)
			nodeClaims = env.EventuallyExpectCreatedNodeClaimCount("==", 0)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)
		})

		It("should prioritize empty nodes for termination", func() {
			// Initially should have 3 nodes
			nodes := env.EventuallyExpectInitializedNodeCount("==", 3)
			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 3)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)

			// Create a pod on one node to make it non-empty
			pods := test.Pods(2, test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("100Mi"),
					},
				},
			})
			var nodesWithPods []*corev1.Node
			for i, pod := range pods {
				pod.Spec.NodeName = nodes[i].Name
				env.ExpectCreated(pod)
				nodesWithPods = append(nodesWithPods, nodes[i])
			}

			// Scale down to 2
			nodePool.Spec.Replicas = lo.ToPtr(int64(2))
			env.ExpectUpdated(nodePool)
			remainingNodes := env.EventuallyExpectInitializedNodeCount("==", 2)
			nodeClaims = env.EventuallyExpectCreatedNodeClaimCount("==", 2)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)

			// The node with the pod should still exist
			Expect(remainingNodes).To(ContainElements(nodesWithPods))
		})

		It("should handle graceful pod eviction during scale down", func() {
			// Initially should have 3 nodes
			env.EventuallyExpectInitializedNodeCount("==", 3)
			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 3)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)

			// Create a deployment with pods on multiple nodes
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: lo.ToPtr(int32(2)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:  "test",
								Image: "nginx",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("100m"),
										corev1.ResourceMemory: resource.MustParse("100Mi"),
									},
								},
							}},
						},
					},
				},
			}
			env.ExpectCreated(deployment)
			selector := labels.SelectorFromSet(deployment.Spec.Selector.MatchLabels)

			env.EventuallyExpectHealthyPodCount(selector, 2)

			// Scale down to 1
			nodePool.Spec.Replicas = lo.ToPtr(int64(1))
			env.ExpectUpdated(nodePool)

			// Should eventually scale down to 1 node
			env.EventuallyExpectInitializedNodeCount("==", 1)
			env.EventuallyExpectCreatedNodeClaimCount("==", 1)

			// Pods should be rescheduled or remain running
			env.EventuallyExpectHealthyPodCount(selector, 2)
		})
	})

	Context("Drift", func() {
		BeforeEach(func() {
			nodePool.Spec.Replicas = lo.ToPtr(int64(5))
			if env.IsDefaultNodeClassKWOK() {
				nodePool.Spec.Template.Spec.Requirements = append(nodePool.Spec.Template.Spec.Requirements, v1.NodeSelectorRequirementWithMinValues{
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values: []string{
							"c-16x-amd64-linux",
							"c-16x-arm64-linux",
						},
					},
				})
			}
			env.ExpectCreated(nodeClass, nodePool)
		})

		It("should replace drifted nodes", func() {
			// Initially should have 2 nodes
			env.EventuallyExpectInitializedNodeCount("==", 5)
			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 5)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)

			// Drift the nodeclaims
			nodePool.Spec.Template.Annotations = map[string]string{"test": "annotation"}
			env.ExpectUpdated(nodePool)

			// Verify the drifted node was replaced
			env.EventuallyExpectDrifted(nodeClaims...)

			// Should create a replacement node and then remove the drifted one
			env.ConsistentlyExpectDisruptionsUntilNoneLeft(5, 5, 5*time.Minute)
		})

		It("should handle drift with node limits", func() {
			// Initially should have 2 nodes
			env.EventuallyExpectInitializedNodeCount("==", 5)
			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 5)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)

			nodePool.Spec.Limits = v1.Limits{
				corev1.ResourceName("nodes"): resource.MustParse("6"),
			}
			nodePool.Spec.Template.Annotations = map[string]string{"test": "annotation"}
			env.ExpectUpdated(nodePool)

			// Verify the drifted node was replaced
			env.EventuallyExpectDrifted(nodeClaims...)

			// Should create a replacement node and then remove the drifted one
			env.ConsistentlyExpectDisruptionsUntilNoneLeft(5, 1, 5*time.Minute)
		})
	})

	Context("Edge Cases", func() {
		BeforeEach(func() {
			if env.IsDefaultNodeClassKWOK() {
				nodePool.Spec.Template.Spec.Requirements = append(nodePool.Spec.Template.Spec.Requirements, v1.NodeSelectorRequirementWithMinValues{
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      corev1.LabelInstanceTypeStable,
						Operator: corev1.NodeSelectorOpIn,
						Values: []string{
							"c-16x-amd64-linux",
							"c-16x-arm64-linux",
						},
					},
				})
			}
		})

		It("should handle NodePool deletion gracefully", func() {
			nodePool.Spec.Replicas = lo.ToPtr(int64(3))
			env.ExpectCreated(nodeClass, nodePool)

			env.EventuallyExpectInitializedNodeCount("==", 3)
			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 3)
			env.EventuallyExpectNodeClaimsReady(nodeClaims...)

			// Delete the NodePool
			env.ExpectDeleted(nodePool)

			// All nodes should eventually be cleaned up
			env.EventuallyExpectInitializedNodeCount("==", 0)
			env.EventuallyExpectCreatedNodeClaimCount("==", 0)
		})
	})
})

// Add tests where
// Presence of static Nodepool should not affect dyanmic NodeClaim Creation
// Should add termination test and expiration test.
