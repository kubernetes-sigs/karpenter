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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cloudproviderapi "k8s.io/cloud-provider/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Initialization", func() {
	var nodePool *v1beta1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
	})
	It("should consider the nodeClaim initialized when all initialization conditions are met", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Resources: v1beta1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("50Mi"),
						v1.ResourcePods:   resource.MustParse("5"),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectMakeNodesReady(ctx, env.Client, node) // Remove the not-ready taint

		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionFalse))

		node = ExpectExists(ctx, env.Client, node)
		node.Status.Capacity = v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("10"),
			v1.ResourceMemory: resource.MustParse("100Mi"),
			v1.ResourcePods:   resource.MustParse("110"),
		}
		node.Status.Allocatable = v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("8"),
			v1.ResourceMemory: resource.MustParse("80Mi"),
			v1.ResourcePods:   resource.MustParse("110"),
		}
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionTrue))
	})
	It("should add the initialization label to the node when the nodeClaim is initialized", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Resources: v1beta1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("50Mi"),
						v1.ResourcePods:   resource.MustParse("5"),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("100Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("80Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectMakeNodesReady(ctx, env.Client, node) // Remove the not-ready taint

		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		node = ExpectExists(ctx, env.Client, node)
		Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodeInitializedLabelKey, "true"))
	})
	It("should not consider the Node to be initialized when the status of the Node is NotReady", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Resources: v1beta1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("50Mi"),
						v1.ResourcePods:   resource.MustParse("5"),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("100Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("80Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			ReadyStatus: v1.ConditionFalse,
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionFalse))
	})
	It("should not consider the Node to be initialized when all requested resources aren't registered", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Resources: v1beta1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:          resource.MustParse("2"),
						v1.ResourceMemory:       resource.MustParse("50Mi"),
						v1.ResourcePods:         resource.MustParse("5"),
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		// Update the nodeClaim to add mock the instance type having an extended resource
		nodeClaim.Status.Capacity[fake.ResourceGPUVendorA] = resource.MustParse("2")
		nodeClaim.Status.Allocatable[fake.ResourceGPUVendorA] = resource.MustParse("2")
		ExpectApplied(ctx, env.Client, nodeClaim)

		// Extended resource hasn't registered yet by the daemonset
		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("100Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("80Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectMakeNodesReady(ctx, env.Client, node) // Remove the not-ready taint

		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionFalse))
	})
	It("should consider the node to be initialized once all the resources are registered", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Resources: v1beta1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:          resource.MustParse("2"),
						v1.ResourceMemory:       resource.MustParse("50Mi"),
						v1.ResourcePods:         resource.MustParse("5"),
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		// Update the nodeClaim to add mock the instance type having an extended resource
		nodeClaim.Status.Capacity[fake.ResourceGPUVendorA] = resource.MustParse("2")
		nodeClaim.Status.Allocatable[fake.ResourceGPUVendorA] = resource.MustParse("2")
		ExpectApplied(ctx, env.Client, nodeClaim)

		// Extended resource hasn't registered yet by the daemonset
		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("100Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("80Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectMakeNodesReady(ctx, env.Client, node) // Remove the not-ready taint

		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionFalse))

		// Node now registers the resource
		node = ExpectExists(ctx, env.Client, node)
		node.Status.Capacity[fake.ResourceGPUVendorA] = resource.MustParse("2")
		node.Status.Allocatable[fake.ResourceGPUVendorA] = resource.MustParse("2")
		ExpectApplied(ctx, env.Client, node)

		// Reconcile the nodeClaim and the nodeClaim/Node should now be initilized
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionTrue))
	})
	It("should not consider the Node to be initialized when all startupTaints aren't removed", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Resources: v1beta1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("50Mi"),
						v1.ResourcePods:   resource.MustParse("5"),
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
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("100Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("80Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectMakeNodesReady(ctx, env.Client, node) // Remove the not-ready taint

		// Should add the startup taints to the node
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
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

		// Shouldn't consider the node ready since the startup taints still exist
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionFalse))
	})
	It("should consider the Node to be initialized once the startupTaints are removed", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Resources: v1beta1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("50Mi"),
						v1.ResourcePods:   resource.MustParse("5"),
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
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("100Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("80Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectMakeNodesReady(ctx, env.Client, node) // Remove the not-ready taint

		// Shouldn't consider the node ready since the startup taints still exist
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionFalse))

		node = ExpectExists(ctx, env.Client, node)
		node.Spec.Taints = []v1.Taint{}
		ExpectApplied(ctx, env.Client, node)

		// nodeClaim should now be ready since all startup taints are removed
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionTrue))
	})
	It("should not consider the Node to be initialized when all ephemeralTaints aren't removed", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Resources: v1beta1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("50Mi"),
						v1.ResourcePods:   resource.MustParse("5"),
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
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("100Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("80Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			Taints: []v1.Taint{
				{
					Key:    v1.TaintNodeNotReady,
					Effect: v1.TaintEffectNoSchedule,
				},
				{
					Key:    v1.TaintNodeUnreachable,
					Effect: v1.TaintEffectNoSchedule,
				},
				{
					Key:    cloudproviderapi.TaintExternalCloudProvider,
					Effect: v1.TaintEffectNoSchedule,
					Value:  "true",
				},
			},
		})
		ExpectApplied(ctx, env.Client, node)

		// Shouldn't consider the node ready since the ephemeral taints still exist
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionFalse))
	})
	It("should consider the Node to be initialized once the ephemeralTaints are removed", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				Resources: v1beta1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("50Mi"),
						v1.ResourcePods:   resource.MustParse("5"),
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
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("100Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("8"),
				v1.ResourceMemory: resource.MustParse("80Mi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
			Taints: []v1.Taint{
				{
					Key:    v1.TaintNodeNotReady,
					Effect: v1.TaintEffectNoSchedule,
				},
				{
					Key:    v1.TaintNodeUnreachable,
					Effect: v1.TaintEffectNoSchedule,
				},
				{
					Key:    cloudproviderapi.TaintExternalCloudProvider,
					Effect: v1.TaintEffectNoSchedule,
					Value:  "true",
				},
			},
		})
		ExpectApplied(ctx, env.Client, node)

		// Shouldn't consider the node ready since the ephemeral taints still exist
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionFalse))

		node = ExpectExists(ctx, env.Client, node)
		node.Spec.Taints = []v1.Taint{}
		ExpectApplied(ctx, env.Client, node)

		// nodeClaim should now be ready since all startup taints are removed
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeRegistered).Status).To(Equal(metav1.ConditionTrue))
		Expect(ExpectStatusConditionExists(nodeClaim, v1beta1.ConditionTypeInitialized).Status).To(Equal(metav1.ConditionTrue))
	})
})
