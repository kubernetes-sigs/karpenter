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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	karpopts "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Placement Strategy (LeastAllocated vs MostAllocated)", func() {
	var nodePool *v1.NodePool
	var nodeA, nodeB *corev1.Node

	BeforeEach(func() {
		nodePool = test.NodePool()
		ExpectApplied(ctx, env.Client, nodePool)

		// Create two nodes: Node A has higher allocation (more pods), Node B has lower allocation
		nodeA = test.Node(test.NodeOptions{
			Allocatable: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:    resource.MustParse("16"),
				corev1.ResourceMemory: resource.MustParse("32Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		})
		nodeB = test.Node(test.NodeOptions{
			Allocatable: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:    resource.MustParse("16"),
				corev1.ResourceMemory: resource.MustParse("32Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		})
		ExpectApplied(ctx, env.Client, nodeA, nodeB)
		ExpectMakeNodesInitialized(ctx, env.Client, env.Clock, nodeA, nodeB)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodeA))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodeB))

		// Add heavy workload pod to node A to make it MostAllocated
		podA := test.Pod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("12"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, podA)
		ExpectManualBinding(ctx, env.Client, podA, nodeA)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(podA))
	})

	It("should schedule pod to LeastAllocated node (Node B) when PlacementStrategyLeastAllocated is set", func() {
		newPod := test.Pod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("2"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, newPod)

		opts := *karpopts.FromContext(ctx)
		opts.PlacementStrategy = karpopts.PlacementStrategyLeastAllocated
		schedulingCtx := karpopts.ToContext(ctx, &opts)
		s, err := prov.NewScheduler(schedulingCtx, []*corev1.Pod{newPod}, cluster.DeepCopyNodes().Active(), nil)
		Expect(err).ToNot(HaveOccurred())

		results, err := s.Solve(schedulingCtx, []*corev1.Pod{newPod})
		Expect(err).ToNot(HaveOccurred())
		Expect(results.ExistingNodes).To(HaveLen(2))

		// Under LeastAllocated, the new pod should be assigned to nodeB (which has 16 CPU free vs nodeA's 4 CPU free)
		var targetNode *scheduling.ExistingNode
		for _, n := range results.ExistingNodes {
			if n.Name() == nodeB.Name {
				targetNode = n
				break
			}
		}
		Expect(targetNode).ToNot(BeNil())
		Expect(targetNode.Pods).To(ContainElement(HaveField("Name", newPod.Name)))
	})

	It("should schedule pod to MostAllocated node (Node A) when PlacementStrategyMostAllocated is set", func() {
		newPod := test.Pod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("2"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, newPod)

		opts := *karpopts.FromContext(ctx)
		opts.PlacementStrategy = karpopts.PlacementStrategyMostAllocated
		schedulingCtx := karpopts.ToContext(ctx, &opts)
		s, err := prov.NewScheduler(schedulingCtx, []*corev1.Pod{newPod}, cluster.DeepCopyNodes().Active(), nil)
		Expect(err).ToNot(HaveOccurred())

		results, err := s.Solve(schedulingCtx, []*corev1.Pod{newPod})
		Expect(err).ToNot(HaveOccurred())
		Expect(results.ExistingNodes).To(HaveLen(2))

		// Under MostAllocated, the new pod should be packed onto nodeA (the more heavily utilized node)
		var targetNode *scheduling.ExistingNode
		for _, n := range results.ExistingNodes {
			if n.Name() == nodeA.Name {
				targetNode = n
				break
			}
		}
		Expect(targetNode).ToNot(BeNil())
		Expect(targetNode.Pods).To(ContainElement(HaveField("Name", newPod.Name)))
	})
})
