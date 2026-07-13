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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("CapacityBuffer", func() {
	var bufferTemplate *corev1.PodTemplate

	BeforeEach(func() {
		nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")

		// Constrain to small instances (2 CPU) so buffer pods force multiple nodes.
		// With 1 CPU buffer pods on 2-CPU nodes, each node fits ~1 buffer pod after
		// daemonset/system overhead, making node count predictable.
		if env.IsDefaultNodeClassKWOK() {
			test.ReplaceRequirements(nodePool, v1.NodeSelectorRequirementWithMinValues{
				Key:      corev1.LabelInstanceTypeStable,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"c-2x-amd64-linux", "c-2x-arm64-linux"},
			})
		}

		bufferTemplate = &corev1.PodTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "buffer-template",
				Namespace: "default",
			},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "pause",
						Image: "registry.k8s.io/pause:3.10",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					}},
				},
			},
		}

		env.ExpectCreated(nodeClass, nodePool)

		// Verify clean starting state — no nodes provisioned before buffer is applied
		Consistently(func(g Gomega) {
			nodeClaims := &v1.NodeClaimList{}
			g.Expect(env.Client.List(env, nodeClaims)).To(Succeed())
			g.Expect(nodeClaims.Items).To(BeEmpty())
		}).WithTimeout(3 * time.Second).Should(Succeed())
	})

	Context("PodTemplateRef Provisioning", func() {
		It("should provision capacity when a buffer with podTemplateRef is applied", func() {
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ProvisioningStrategy: lo.ToPtr(autoscalingv1beta1.ActiveProvisioningStrategy),
					PodTemplateRef:       &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:             lo.ToPtr(int32(3)),
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			EventuallyExpectCapacityBufferReplicas(env, env.Client, buffer, 3)

			// With 2-CPU nodes and 1-CPU buffer pods, expect multiple nodes
			env.EventuallyExpectCreatedNodeClaimCount(">=", 2)
			env.EventuallyExpectInitializedNodeCount(">=", 2)

			EventuallyExpectCapacityBufferProvisionedWithReason(env, env.Client, buffer, "FitsExistingCapacity")
		})

		It("should update buffer status when PodTemplate is updated", func() {
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(2)),
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			// Wait for initial resolution
			EventuallyExpectCapacityBufferReady(env, env.Client, buffer)

			// Get current generation
			cb := &autoscalingv1beta1.CapacityBuffer{}
			Expect(env.Client.Get(env, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
			originalGen := *cb.Status.PodTemplateGeneration

			// Update the PodTemplate
			pt := &corev1.PodTemplate{}
			Expect(env.Client.Get(env, client.ObjectKeyFromObject(bufferTemplate), pt)).To(Succeed())
			pt.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("2")
			env.ExpectUpdated(pt)

			// Generation should update
			EventuallyExpectCapacityBufferGenerationUpdated(env, env.Client, buffer, originalGen)
		})
	})

	Context("ScalableRef Provisioning", func() {
		var scalableDeployment *appsv1.Deployment

		BeforeEach(func() {
			scalableDeployment = test.Deployment(test.DeploymentOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "scalable-app",
					Namespace: "default",
				},
				Replicas: 10,
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "scalable-app"}},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			})
		})

		It("should provision buffer capacity using scalableRef with percentage", func() {
			// Create the Deployment and wait for its pods to schedule
			env.ExpectCreated(scalableDeployment)
			selector := labels.SelectorFromSet(scalableDeployment.Spec.Selector.MatchLabels)
			env.EventuallyExpectHealthyPodCountWithTimeout(2*time.Minute, selector, 10)

			// Apply the buffer
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "apps",
						Kind:     "Deployment",
						Name:     "scalable-app",
					},
					Percentage: lo.ToPtr(int32(20)),
				},
			})
			env.ExpectCreated(buffer)

			// 20% of 10 replicas = 2 buffer chunks
			EventuallyExpectCapacityBufferReplicas(env, env.Client, buffer, 2)

			// Buffer virtual pods should be provisioned (may fit on existing nodes or new ones)
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)
		})

		It("should provision buffer capacity using scalableRef with fixed replicas", func() {
			// Create the Deployment and wait for pods to schedule
			env.ExpectCreated(scalableDeployment)
			selector := labels.SelectorFromSet(scalableDeployment.Spec.Selector.MatchLabels)
			env.EventuallyExpectHealthyPodCountWithTimeout(2*time.Minute, selector, 10)

			// Apply buffer with fixed replicas
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "apps",
						Kind:     "Deployment",
						Name:     "scalable-app",
					},
					Replicas: lo.ToPtr(int32(3)),
				},
			})
			env.ExpectCreated(buffer)

			// Buffer resolves with 3 replicas
			EventuallyExpectCapacityBufferReplicas(env, env.Client, buffer, 3)

			// Buffer virtual pods should be provisioned
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)
		})

		It("should recover when scalable ref is created after buffer", func() {
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "apps",
						Kind:     "Deployment",
						Name:     "scalable-app",
					},
					Replicas: lo.ToPtr(int32(2)),
				},
			})

			// Create buffer first, without the deployment
			env.ExpectCreated(buffer)

			// Buffer should initially be not ready (deployment doesn't exist yet)
			EventuallyExpectCapacityBufferNotReady(env, env.Client, buffer, "ScalableRefNotFound")

			// Now create the deployment
			env.ExpectCreated(scalableDeployment)

			// Buffer should recover and become ready
			EventuallyExpectCapacityBufferReplicas(env, env.Client, buffer, 2)
		})
	})

	Context("Consumer Interaction", func() {
		It("should allow consumer pods to use existing buffer capacity and then refill", func() {
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(2)),
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			// Wait for buffer capacity to be fully provisioned
			env.EventuallyExpectInitializedNodeCount(">=", 2)
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)

			// Record NodeClaim count before deploying consumers
			nodeClaimsBefore := env.EventuallyExpectCreatedNodeClaimCount(">=", 2)
			countBefore := len(nodeClaimsBefore)

			// Deploy consumer that fits within buffer shape — schedules on existing capacity
			dep := test.Deployment(test.DeploymentOptions{
				Replicas: 1,
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "consumer"}},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			})
			env.ExpectCreated(dep)

			// Consumer schedules immediately on existing buffer capacity
			selector := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
			env.EventuallyExpectHealthyPodCountWithTimeout(2*time.Minute, selector, 1)

			// Buffer refills — new node(s) created to restore buffer capacity
			env.EventuallyExpectCreatedNodeClaimCount(">=", countBefore+1)

			// Buffer should eventually be satisfied again
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)
		})

		It("should refill buffer capacity after consumption", func() {
			// Buffer uses the standard template (1 CPU). On 2-CPU nodes, each buffer
			// pod gets its own node. A consumer taking one node forces a refill.
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(2)),
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			env.EventuallyExpectInitializedNodeCount(">=", 2)
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)

			initialNodeClaims := env.EventuallyExpectCreatedNodeClaimCount(">=", 2)
			initialCount := len(initialNodeClaims)

			// Deploy a consumer that needs 1 CPU — takes one buffer node's capacity
			dep := test.Deployment(test.DeploymentOptions{
				Replicas: 1,
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "refill-consumer"}},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			})
			env.ExpectCreated(dep)
			selector := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
			env.EventuallyExpectHealthyPodCountWithTimeout(2*time.Minute, selector, 1)

			// Buffer needs refill — new NodeClaim(s) should be created
			env.EventuallyExpectCreatedNodeClaimCount(">=", initialCount+1)

			// Buffer should eventually be satisfied again
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)
		})
	})

	Context("Disruption", func() {
		It("should not empty-consolidate nodes hosting buffer pods", func() {
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(2)),
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			nodes := env.EventuallyExpectInitializedNodeCount(">=", 2)
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)

			// With ConsolidateAfter: 0s, disruption should trigger within seconds if
			// the node were truly empty. Wait 60s to be confident it's actually protected.
			env.ConsistentlyExpectNoDisruptions(len(nodes), 60*time.Second)
		})

		It("should consolidate buffer nodes after buffer is deleted", func() {
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(2)),
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount(">=", 2)
			env.EventuallyExpectInitializedNodeCount(">=", 2)
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)

			env.ExpectDeleted(buffer)

			env.EventuallyExpectNotFound(nodeClaims[0])
		})

		It("should allow drift to replace buffer nodes", func() {
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(2)),
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount(">=", 2)
			env.EventuallyExpectInitializedNodeCount(">=", 2)
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)

			originalNodeClaimNames := lo.Map(nodeClaims, func(nc *v1.NodeClaim, _ int) string { return nc.Name })

			nodePool.Spec.Template.Annotations = map[string]string{"drift-trigger": "true"}
			env.ExpectUpdated(nodePool)

			Eventually(func(g Gomega) {
				for _, name := range originalNodeClaimNames {
					nc := &v1.NodeClaim{}
					err := env.Client.Get(env, client.ObjectKey{Name: name}, nc)
					if err == nil {
						g.Expect(nc.DeletionTimestamp.IsZero()).To(BeFalse())
					}
				}
			}).WithTimeout(2 * time.Minute).Should(Succeed())

			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)
		})

	})

	Context("Lifecycle", func() {
		It("should scale buffer down when replicas are reduced", func() {
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(3)),
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			env.EventuallyExpectInitializedNodeCount(">=", 2)
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)

			// Scale buffer down to 1
			cb := &autoscalingv1beta1.CapacityBuffer{}
			Expect(env.Client.Get(env, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
			cb.Spec.Replicas = lo.ToPtr(int32(1))
			env.ExpectUpdated(cb)

			// Status should reflect new replica count
			EventuallyExpectCapacityBufferReplicas(env, env.Client, buffer, 1)
		})

		It("should handle scalableRef percentage update when deployment scales", func() {
			scalableDeployment := test.Deployment(test.DeploymentOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "growing-app",
					Namespace: "default",
				},
				Replicas: 5,
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "growing-app"}},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			})

			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "apps",
						Kind:     "Deployment",
						Name:     "growing-app",
					},
					Percentage: lo.ToPtr(int32(20)),
				},
			})

			env.ExpectCreated(scalableDeployment, buffer)

			// 20% of 5 = 1
			EventuallyExpectCapacityBufferReplicas(env, env.Client, buffer, 1)

			// Scale the deployment up to 20
			deploy := &appsv1.Deployment{}
			Expect(env.Client.Get(env, client.ObjectKeyFromObject(scalableDeployment), deploy)).To(Succeed())
			deploy.Spec.Replicas = lo.ToPtr(int32(20))
			env.ExpectUpdated(deploy)

			// Buffer should eventually recalculate: 20% of 20 = 4
			Eventually(func(g Gomega) {
				cb := &autoscalingv1beta1.CapacityBuffer{}
				g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
				g.Expect(cb.Status.Replicas).ToNot(BeNil())
				g.Expect(*cb.Status.Replicas).To(Equal(int32(4)))
			}).WithTimeout(60 * time.Second).Should(Succeed())
		})
	})

	Context("NodePool Limits", func() {
		It("should not provision buffer capacity beyond NodePool CPU limit", func() {
			// With 2-CPU nodes, a 4-CPU limit allows at most 2 nodes
			nodePool.Spec.Limits = v1.Limits{
				corev1.ResourceCPU: resource.MustParse("4"),
			}
			env.ExpectUpdated(nodePool)

			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(10)),
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			// Buffer requests 10 pods but NodePool limits to 4 CPU (2 nodes × 2 CPU).
			// Should provision exactly 2 nodes and stop.
			env.EventuallyExpectCreatedNodeClaimCount("==", 2)
			env.EventuallyExpectInitializedNodeCount("==", 2)

			// Verify node count stays at 2
			Consistently(func(g Gomega) {
				nodeClaims := &v1.NodeClaimList{}
				g.Expect(env.Client.List(env, nodeClaims)).To(Succeed())
				g.Expect(nodeClaims.Items).To(HaveLen(2))
			}).WithTimeout(30 * time.Second).Should(Succeed())
		})
	})

	Context("Multiple Buffers", func() {
		It("should provision capacity independently for multiple buffers", func() {
			bufferTemplateSmall := &corev1.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "small-buffer-template",
					Namespace: "default",
				},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "pause",
							Image: "registry.k8s.io/pause:3.10",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						}},
					},
				},
			}

			bufferA := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(2)),
				},
			})

			bufferB := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "small-buffer-template"},
					Replicas:       lo.ToPtr(int32(3)),
				},
			})

			env.ExpectCreated(bufferTemplate, bufferTemplateSmall, bufferA, bufferB)

			// Both buffers should become ready independently
			EventuallyExpectCapacityBufferReplicas(env, env.Client, bufferA, 2)
			EventuallyExpectCapacityBufferReplicas(env, env.Client, bufferB, 3)

			// Capacity should be provisioned for both
			env.EventuallyExpectCreatedNodeClaimCount(">=", 2)
			env.EventuallyExpectInitializedNodeCount(">=", 2)

			// Both should eventually report Provisioning=True
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, bufferA)
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, bufferB)
		})
	})

	Context("Edge Cases", func() {
		It("should not leak nodes on rapid buffer create and delete", func() {
			env.ExpectCreated(bufferTemplate)

			// Create a buffer and wait for it to actually provision a node,
			// proving the system is working before we test rapid create/delete.
			seedBuffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(1)),
				},
			})
			env.ExpectCreated(seedBuffer)
			env.EventuallyExpectCreatedNodeClaimCount(">=", 1)
			env.ExpectDeleted(seedBuffer)

			// Now do rapid create/delete cycles
			for i := 0; i < 5; i++ {
				buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("rapid-buffer-%d", i)},
					Spec: autoscalingv1beta1.CapacityBufferSpec{
						PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
						Replicas:       lo.ToPtr(int32(2)),
					},
				})
				env.ExpectCreated(buffer)
				time.Sleep(2 * time.Second)
				env.ExpectDeleted(buffer)
			}

			// After all rapid create/deletes, no buffers should remain
			Eventually(func(g Gomega) {
				buffers := &autoscalingv1beta1.CapacityBufferList{}
				g.Expect(env.Client.List(env, buffers, client.InNamespace("default"))).To(Succeed())
				g.Expect(buffers.Items).To(BeEmpty())
			}).WithTimeout(30 * time.Second).Should(Succeed())

			// All provisioned nodes should eventually be cleaned up (empty consolidation)
			Eventually(func(g Gomega) {
				nodeClaims := &v1.NodeClaimList{}
				g.Expect(env.Client.List(env, nodeClaims)).To(Succeed())
				g.Expect(nodeClaims.Items).To(BeEmpty())
			}).WithTimeout(2 * time.Minute).Should(Succeed())
		})

		It("should coexist with real pods on the same node", func() {
			// Deploy a small real pod first
			dep := test.Deployment(test.DeploymentOptions{
				Replicas: 1,
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "coexist"}},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			})
			env.ExpectCreated(dep)
			selector := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
			env.EventuallyExpectHealthyPodCountWithTimeout(2*time.Minute, selector, 1)

			// Before buffer: exactly 1 node for the real pod
			env.EventuallyExpectInitializedNodeCount("==", 1)

			// Now create a buffer — virtual pod should pack onto the same node
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(1)),
				},
			})
			env.ExpectCreated(bufferTemplate, buffer)

			// Buffer should become provisioned (virtual pod fits alongside real pod)
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)

			// After buffer: still exactly 1 node — buffer coexists on existing capacity
			Consistently(func(g Gomega) {
				nodeClaims := &v1.NodeClaimList{}
				g.Expect(env.Client.List(env, nodeClaims)).To(Succeed())
				g.Expect(nodeClaims.Items).To(HaveLen(1))
			}).WithTimeout(30 * time.Second).Should(Succeed())

			// Real pod should still be healthy
			env.EventuallyExpectHealthyPodCountWithTimeout(30*time.Second, selector, 1)
		})

		It("should respect nodeSelector from PodTemplate", func() {
			// Create a PodTemplate with a nodeSelector
			selectorTemplate := &corev1.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "selector-template",
					Namespace: "default",
				},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "pause",
							Image: "registry.k8s.io/pause:3.10",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						}},
						NodeSelector: map[string]string{
							"kubernetes.io/os": "linux",
						},
					},
				},
			}

			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "selector-template"},
					Replicas:       lo.ToPtr(int32(1)),
				},
			})

			env.ExpectCreated(selectorTemplate, buffer)

			// Buffer should provision — nodeSelector is preserved and matches NodePool
			env.EventuallyExpectCreatedNodeClaimCount(">=", 1)
			env.EventuallyExpectInitializedNodeCount(">=", 1)

			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)

			// Verify the node has the expected label
			nodes := env.EventuallyExpectInitializedNodeCount(">=", 1)
			Expect(nodes[0].Labels["kubernetes.io/os"]).To(Equal("linux"))
		})

		It("should refill buffer after node expiry", func() {
			// Use a short expiry
			nodePool.Spec.Template.Spec.ExpireAfter = v1.MustParseNillableDuration("1m")
			env.ExpectUpdated(nodePool)

			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Replicas:       lo.ToPtr(int32(1)),
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			// Wait for initial capacity
			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount(">=", 1)
			env.EventuallyExpectInitializedNodeCount(">=", 1)
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)

			originalName := nodeClaims[0].Name

			// Wait for expiry to replace the node (1m expiry + processing time)
			Eventually(func(g Gomega) {
				nc := &v1.NodeClaim{}
				err := env.Client.Get(env, client.ObjectKey{Name: originalName}, nc)
				if err == nil {
					g.Expect(nc.DeletionTimestamp.IsZero()).To(BeFalse())
				}
			}).WithTimeout(3 * time.Minute).Should(Succeed())

			// Buffer should refill on new capacity
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)
		})

		It("should grow buffer replicas when limits are increased", func() {
			buffer := test.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "buffer-template"},
					Limits: autoscalingv1beta1.Limits{
						corev1.ResourceCPU: resource.MustParse("2"),
					},
				},
			})

			env.ExpectCreated(bufferTemplate, buffer)

			// Initial: 2 CPU limit / 1 CPU per pod = 2 replicas
			EventuallyExpectCapacityBufferReplicas(env, env.Client, buffer, 2)

			// Wait for initial capacity
			env.EventuallyExpectCreatedNodeClaimCount(">=", 1)
			env.EventuallyExpectInitializedNodeCount(">=", 1)

			// Increase limits to 5 CPU → should grow to 5 replicas
			cb := &autoscalingv1beta1.CapacityBuffer{}
			Expect(env.Client.Get(env, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
			cb.Spec.Limits = autoscalingv1beta1.Limits{
				corev1.ResourceCPU: resource.MustParse("5"),
			}
			env.ExpectUpdated(cb)

			Eventually(func(g Gomega) {
				cb := &autoscalingv1beta1.CapacityBuffer{}
				g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
				g.Expect(cb.Status.Replicas).ToNot(BeNil())
				g.Expect(*cb.Status.Replicas).To(Equal(int32(5)))
			}).WithTimeout(60 * time.Second).Should(Succeed())

			// Eventually all 5 replicas should be provisioned
			EventuallyExpectCapacityBufferProvisioned(env, env.Client, buffer)
		})
	})
})
