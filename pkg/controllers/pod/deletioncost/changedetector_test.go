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

package deletioncost_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/pod/deletioncost"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("ChangeDetector", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
	})

	It("should detect change on first call (empty state)", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		for i := range nodeClaims {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}
		ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[0].Name}))
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		var stateNodes []*state.StateNode
		for n := range cluster.Nodes() {
			stateNodes = append(stateNodes, n)
		}

		cd := deletioncost.NewChangeDetector()
		changed, err := cd.HasChanged(ctx, env.Client, stateNodes)
		Expect(err).ToNot(HaveOccurred())
		Expect(changed).To(BeTrue(), "first call should always detect change")
	})

	It("should skip when same state is checked twice", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		for i := range nodeClaims {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}
		ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[0].Name}))
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		var stateNodes []*state.StateNode
		for n := range cluster.Nodes() {
			stateNodes = append(stateNodes, n)
		}

		cd := deletioncost.NewChangeDetector()
		// First call
		changed, err := cd.HasChanged(ctx, env.Client, stateNodes)
		Expect(err).ToNot(HaveOccurred())
		Expect(changed).To(BeTrue())

		// Second call with same state
		changed, err = cd.HasChanged(ctx, env.Client, stateNodes)
		Expect(err).ToNot(HaveOccurred())
		Expect(changed).To(BeFalse(), "same state should not be detected as changed")
	})

	It("should detect change when a node is added", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		for i := range nodeClaims {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}
		ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: nodes[0].Name}))
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		var stateNodes []*state.StateNode
		for n := range cluster.Nodes() {
			stateNodes = append(stateNodes, n)
		}

		cd := deletioncost.NewChangeDetector()
		// Baseline
		_, err := cd.HasChanged(ctx, env.Client, stateNodes)
		Expect(err).ToNot(HaveOccurred())

		// Add a second node
		newNodeClaims, newNodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		for i := range newNodeClaims {
			ExpectApplied(ctx, env.Client, newNodeClaims[i], newNodes[i])
		}
		ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{NodeName: newNodes[0].Name}))
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, newNodes, newNodeClaims)

		var updatedStateNodes []*state.StateNode
		for n := range cluster.Nodes() {
			updatedStateNodes = append(updatedStateNodes, n)
		}

		changed, err := cd.HasChanged(ctx, env.Client, updatedStateNodes)
		Expect(err).ToNot(HaveOccurred())
		Expect(changed).To(BeTrue(), "adding a node should be detected as a change")
	})

	It("should detect change when pod count changes", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		for i := range nodeClaims {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}
		pod := test.Pod(test.PodOptions{NodeName: nodes[0].Name})
		ExpectApplied(ctx, env.Client, pod)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		var stateNodes []*state.StateNode
		for n := range cluster.Nodes() {
			stateNodes = append(stateNodes, n)
		}

		cd := deletioncost.NewChangeDetector()
		// Baseline
		_, err := cd.HasChanged(ctx, env.Client, stateNodes)
		Expect(err).ToNot(HaveOccurred())

		// Add another pod to the same node
		newPod := test.Pod(test.PodOptions{NodeName: nodes[0].Name})
		ExpectApplied(ctx, env.Client, newPod)

		// Re-read state nodes (pods are fetched live from API)
		var updatedStateNodes []*state.StateNode
		for n := range cluster.Nodes() {
			updatedStateNodes = append(updatedStateNodes, n)
		}

		changed, err := cd.HasChanged(ctx, env.Client, updatedStateNodes)
		Expect(err).ToNot(HaveOccurred())
		Expect(changed).To(BeTrue(), "adding a pod should be detected as a change")
	})

	It("should handle empty node list", func() {
		cd := deletioncost.NewChangeDetector()
		changed, err := cd.HasChanged(ctx, env.Client, nil)
		Expect(err).ToNot(HaveOccurred())
		// Empty to empty is no change
		Expect(changed).To(BeFalse())
	})
})
