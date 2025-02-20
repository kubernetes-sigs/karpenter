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

package lifecycle_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Registration", func() {
	var nodePool *v1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool()
	})
	DescribeTable(
		"Registration",
		func(isManagedNodeClaim bool) {
			nodeClaimOpts := []v1.NodeClaim{{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey: nodePool.Name,
					},
				},
			}}
			if !isManagedNodeClaim {
				nodeClaimOpts = append(nodeClaimOpts, v1.NodeClaim{
					Spec: v1.NodeClaimSpec{
						NodeClassRef: &v1.NodeClassReference{
							Group: "karpenter.test.sh",
							Kind:  "UnmanagedNodeClass",
							Name:  "default",
						},
					},
				})
			}
			nodeClaim := test.NodeClaim(nodeClaimOpts...)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

			node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID, Taints: []corev1.Taint{v1.UnregisteredNoExecuteTaint}})
			ExpectApplied(ctx, env.Client, node)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			if isManagedNodeClaim {
				Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).IsTrue()).To(BeTrue())
				Expect(nodeClaim.Status.NodeName).To(Equal(node.Name))
			} else {
				Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).IsUnknown()).To(BeTrue())
				Expect(nodeClaim.Status.NodeName).To(Equal(""))
			}
		},
		Entry("should match the nodeClaim to the Node when the Node comes online", true),
		Entry("should ignore NodeClaims not managed by this Karpenter instance", false),
	)
	It("should add the owner reference to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID, Taints: []corev1.Taint{v1.UnregisteredNoExecuteTaint}})
		ExpectApplied(ctx, env.Client, node)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)

		node = ExpectExists(ctx, env.Client, node)
		ExpectOwnerReferenceExists(node, nodeClaim)
	})
	It("should sync the karpenter.sh/registered label to the Node and remove the karpenter.sh/unregistered taint when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID, Taints: []corev1.Taint{v1.UnregisteredNoExecuteTaint}})
		ExpectApplied(ctx, env.Client, node)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		node = ExpectExists(ctx, env.Client, node)
		Expect(node.Labels).To(HaveKeyWithValue(v1.NodeRegisteredLabelKey, "true"))
		Expect(node.Spec.Taints).To(Not(ContainElement(v1.UnregisteredNoExecuteTaint)))
	})
	It("should succeed registration if the karpenter.sh/unregistered taint is not present and emit an event", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		// Create a node without the unregistered taint
		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		ExpectApplied(ctx, env.Client, node)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)

		// Verify the NodeClaim is registered
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).IsTrue()).To(BeTrue())
		Expect(nodeClaim.Status.NodeName).To(Equal(node.Name))

		// Verify the node is registered
		node = ExpectExists(ctx, env.Client, node)
		Expect(node.Labels).To(HaveKeyWithValue(v1.NodeRegisteredLabelKey, "true"))

		Expect(recorder.Calls(events.NodeClassUnregisteredTaintMissing)).To(Equal(1))
	})

	It("should sync the labels to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:  nodePool.Name,
					"custom-label":       "custom-value",
					"other-custom-label": "other-custom-value",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Labels).To(HaveKeyWithValue("custom-label", "custom-value"))
		Expect(nodeClaim.Labels).To(HaveKeyWithValue("other-custom-label", "other-custom-value"))

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID, Taints: []corev1.Taint{v1.UnregisteredNoExecuteTaint}})
		ExpectApplied(ctx, env.Client, node)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		node = ExpectExists(ctx, env.Client, node)

		// Expect Node to have all the labels that the nodeClaim has
		for k, v := range nodeClaim.Labels {
			Expect(node.Labels).To(HaveKeyWithValue(k, v))
		}
	})
	It("should sync the annotations to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
				Annotations: map[string]string{
					v1.DoNotDisruptAnnotationKey: "true",
					"my-custom-annotation":       "my-custom-value",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.DoNotDisruptAnnotationKey, "true"))
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue("my-custom-annotation", "my-custom-value"))

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID, Taints: []corev1.Taint{v1.UnregisteredNoExecuteTaint}})
		ExpectApplied(ctx, env.Client, node)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		node = ExpectExists(ctx, env.Client, node)

		// Expect Node to have all the annotations that the nodeClaim has
		for k, v := range nodeClaim.Annotations {
			Expect(node.Annotations).To(HaveKeyWithValue(k, v))
		}
	})
	It("should sync the taints to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1.NodeClaimSpec{
				Taints: []corev1.Taint{
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
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Spec.Taints).To(ContainElements(
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

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID, Taints: []corev1.Taint{v1.UnregisteredNoExecuteTaint}})
		ExpectApplied(ctx, env.Client, node)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		node = ExpectExists(ctx, env.Client, node)

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
	})
	It("should sync the startupTaints to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1.NodeClaimSpec{
				Taints: []corev1.Taint{
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
				},
				StartupTaints: []corev1.Taint{
					{
						Key:    "custom-startup-taint",
						Effect: corev1.TaintEffectNoSchedule,
						Value:  "custom-startup-value",
					},
					{
						Key:    "other-custom-startup-taint",
						Effect: corev1.TaintEffectNoExecute,
						Value:  "other-custom-startup-value",
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Spec.StartupTaints).To(ContainElements(
			corev1.Taint{
				Key:    "custom-startup-taint",
				Effect: corev1.TaintEffectNoSchedule,
				Value:  "custom-startup-value",
			},
			corev1.Taint{
				Key:    "other-custom-startup-taint",
				Effect: corev1.TaintEffectNoExecute,
				Value:  "other-custom-startup-value",
			},
		))

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID, Taints: []corev1.Taint{v1.UnregisteredNoExecuteTaint}})
		ExpectApplied(ctx, env.Client, node)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		node = ExpectExists(ctx, env.Client, node)

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
			corev1.Taint{
				Key:    "custom-startup-taint",
				Effect: corev1.TaintEffectNoSchedule,
				Value:  "custom-startup-value",
			},
			corev1.Taint{
				Key:    "other-custom-startup-taint",
				Effect: corev1.TaintEffectNoExecute,
				Value:  "other-custom-startup-value",
			},
		))
	})
	It("should not re-sync the startupTaints to the Node when the startupTaints are removed", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1.NodeClaimSpec{
				StartupTaints: []corev1.Taint{
					{
						Key:    "custom-startup-taint",
						Effect: corev1.TaintEffectNoSchedule,
						Value:  "custom-startup-value",
					},
					{
						Key:    "other-custom-startup-taint",
						Effect: corev1.TaintEffectNoExecute,
						Value:  "other-custom-startup-value",
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID, Taints: []corev1.Taint{v1.UnregisteredNoExecuteTaint}})
		ExpectApplied(ctx, env.Client, node)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		node = ExpectExists(ctx, env.Client, node)

		Expect(node.Spec.Taints).To(ContainElements(
			corev1.Taint{
				Key:    "custom-startup-taint",
				Effect: corev1.TaintEffectNoSchedule,
				Value:  "custom-startup-value",
			},
			corev1.Taint{
				Key:    "other-custom-startup-taint",
				Effect: corev1.TaintEffectNoExecute,
				Value:  "other-custom-startup-value",
			},
		))
		node.Spec.Taints = []corev1.Taint{}
		ExpectApplied(ctx, env.Client, node)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		node = ExpectExists(ctx, env.Client, node)
		Expect(node.Spec.Taints).To(HaveLen(0))
	})
})
