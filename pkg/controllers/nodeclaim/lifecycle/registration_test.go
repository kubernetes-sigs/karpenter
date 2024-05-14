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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Registration", func() {
	var nodePool *v1beta1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool()
	})
	It("should match the nodeClaim to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(nodeClaim.Status.NodeName).To(Equal(node.Name))
	})
	It("should add the owner reference to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))

		node = ExpectExists(ctx, env.Client, node)
		ExpectOwnerReferenceExists(node, nodeClaim)
	})
	It("should sync the karpenter.sh/registered label to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		node = ExpectExists(ctx, env.Client, node)
		Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodeRegisteredLabelKey, "true"))
	})
	It("should sync the labels to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
					"custom-label":           "custom-value",
					"other-custom-label":     "other-custom-value",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Labels).To(HaveKeyWithValue("custom-label", "custom-value"))
		Expect(nodeClaim.Labels).To(HaveKeyWithValue("other-custom-label", "other-custom-value"))

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		node = ExpectExists(ctx, env.Client, node)

		// Expect Node to have all the labels that the nodeClaim has
		for k, v := range nodeClaim.Labels {
			Expect(node.Labels).To(HaveKeyWithValue(k, v))
		}
	})
	It("should sync the annotations to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
				Annotations: map[string]string{
					v1beta1.DoNotDisruptAnnotationKey: "true",
					"my-custom-annotation":            "my-custom-value",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1beta1.DoNotDisruptAnnotationKey, "true"))
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue("my-custom-annotation", "my-custom-value"))

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		node = ExpectExists(ctx, env.Client, node)

		// Expect Node to have all the annotations that the nodeClaim has
		for k, v := range nodeClaim.Annotations {
			Expect(node.Annotations).To(HaveKeyWithValue(k, v))
		}
	})
	It("should sync the taints to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Taints: []v1.Taint{
					{
						Key:    "custom-taint",
						Effect: v1.TaintEffectNoSchedule,
						Value:  "custom-value",
					},
					{
						Key:    "other-custom-taint",
						Effect: v1.TaintEffectNoExecute,
						Value:  "other-custom-value",
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Spec.Taints).To(ContainElements(
			v1.Taint{
				Key:    "custom-taint",
				Effect: v1.TaintEffectNoSchedule,
				Value:  "custom-value",
			},
			v1.Taint{
				Key:    "other-custom-taint",
				Effect: v1.TaintEffectNoExecute,
				Value:  "other-custom-value",
			},
		))

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		node = ExpectExists(ctx, env.Client, node)

		Expect(node.Spec.Taints).To(ContainElements(
			v1.Taint{
				Key:    "custom-taint",
				Effect: v1.TaintEffectNoSchedule,
				Value:  "custom-value",
			},
			v1.Taint{
				Key:    "other-custom-taint",
				Effect: v1.TaintEffectNoExecute,
				Value:  "other-custom-value",
			},
		))
	})
	It("should sync the startupTaints to the Node when the Node comes online", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Taints: []v1.Taint{
					{
						Key:    "custom-taint",
						Effect: v1.TaintEffectNoSchedule,
						Value:  "custom-value",
					},
					{
						Key:    "other-custom-taint",
						Effect: v1.TaintEffectNoExecute,
						Value:  "other-custom-value",
					},
				},
				StartupTaints: []v1.Taint{
					{
						Key:    "custom-startup-taint",
						Effect: v1.TaintEffectNoSchedule,
						Value:  "custom-startup-value",
					},
					{
						Key:    "other-custom-startup-taint",
						Effect: v1.TaintEffectNoExecute,
						Value:  "other-custom-startup-value",
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Spec.StartupTaints).To(ContainElements(
			v1.Taint{
				Key:    "custom-startup-taint",
				Effect: v1.TaintEffectNoSchedule,
				Value:  "custom-startup-value",
			},
			v1.Taint{
				Key:    "other-custom-startup-taint",
				Effect: v1.TaintEffectNoExecute,
				Value:  "other-custom-startup-value",
			},
		))

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		node = ExpectExists(ctx, env.Client, node)

		Expect(node.Spec.Taints).To(ContainElements(
			v1.Taint{
				Key:    "custom-taint",
				Effect: v1.TaintEffectNoSchedule,
				Value:  "custom-value",
			},
			v1.Taint{
				Key:    "other-custom-taint",
				Effect: v1.TaintEffectNoExecute,
				Value:  "other-custom-value",
			},
			v1.Taint{
				Key:    "custom-startup-taint",
				Effect: v1.TaintEffectNoSchedule,
				Value:  "custom-startup-value",
			},
			v1.Taint{
				Key:    "other-custom-startup-taint",
				Effect: v1.TaintEffectNoExecute,
				Value:  "other-custom-startup-value",
			},
		))
	})
	It("should not re-sync the startupTaints to the Node when the startupTaints are removed", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				StartupTaints: []v1.Taint{
					{
						Key:    "custom-startup-taint",
						Effect: v1.TaintEffectNoSchedule,
						Value:  "custom-startup-value",
					},
					{
						Key:    "other-custom-startup-taint",
						Effect: v1.TaintEffectNoExecute,
						Value:  "other-custom-startup-value",
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		node = ExpectExists(ctx, env.Client, node)

		Expect(node.Spec.Taints).To(ContainElements(
			v1.Taint{
				Key:    "custom-startup-taint",
				Effect: v1.TaintEffectNoSchedule,
				Value:  "custom-startup-value",
			},
			v1.Taint{
				Key:    "other-custom-startup-taint",
				Effect: v1.TaintEffectNoExecute,
				Value:  "other-custom-startup-value",
			},
		))
		node.Spec.Taints = []v1.Taint{}
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimController), client.ObjectKeyFromObject(nodeClaim))
		node = ExpectExists(ctx, env.Client, node)
		Expect(node.Spec.Taints).To(HaveLen(0))
	})
})
