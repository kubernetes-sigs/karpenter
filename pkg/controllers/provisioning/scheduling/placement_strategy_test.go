/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package scheduling_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
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
			},
		})
		nodeB = test.Node(test.NodeOptions{
			Allocatable: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:    resource.MustParse("16"),
				corev1.ResourceMemory: resource.MustParse("32Gi"),
			},
		})
		ExpectApplied(ctx, env.Client, nodeA, nodeB)

		// Add heavy workload pod to node A to make it MostAllocated
		podA := test.Pod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("12"),
				},
			},
			NodeName: nodeA.Name,
		})
		ExpectApplied(ctx, env.Client, podA)
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

		s, err := prov.NewScheduler(ctx, []*corev1.Pod{newPod}, nil, nil, scheduling.WithPlacementStrategy(scheduling.PlacementStrategyLeastAllocated))
		Expect(err).ToNot(HaveOccurred())

		results, err := s.Solve(ctx, []*corev1.Pod{newPod})
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

		s, err := prov.NewScheduler(ctx, []*corev1.Pod{newPod}, nil, nil, scheduling.WithPlacementStrategy(scheduling.PlacementStrategyMostAllocated))
		Expect(err).ToNot(HaveOccurred())

		results, err := s.Solve(ctx, []*corev1.Pod{newPod})
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
