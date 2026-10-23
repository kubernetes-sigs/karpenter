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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// These specs check that, with a nodeResourcesFit scoring strategy configured, a pod is simulated onto the existing node
// kube-scheduler would rank highest. Each node's name order differs from both strategies' order, so the default name
// order can't make a spec pass by accident.
var _ = Describe("NodeResourcesFit", func() {
	BeforeEach(func() {
		ExpectApplied(ctx, env.Client, test.NodePool())
	})
	AfterEach(func() {
		ctx = options.ToContext(ctx, test.Options())
	})

	// withSchedulerConfig parses the config as the operator would, so kube-scheduler's defaults are applied.
	withSchedulerConfig := func(raw string) {
		GinkgoHelper()
		cfg, err := options.ParseSchedulerConfiguration(raw)
		Expect(err).ToNot(HaveOccurred())
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{SchedulerConfig: cfg}))
	}
	// existingNode creates an initialized 10 cpu / 10Gi node with a pod bound to it requesting the given resources.
	existingNode := func(name string, requests corev1.ResourceList) {
		GinkgoHelper()
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10"),
				corev1.ResourceMemory: resource.MustParse("10Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectMakeNodesInitialized(ctx, env.Client, env.Clock, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		pod := test.Pod(test.PodOptions{ResourceRequirements: corev1.ResourceRequirements{Requests: requests}})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(pod))
	}
	cpu := func(quantity string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(quantity)}
	}
	// scheduleOnto provisions pods requesting the given cpu and returns how many landed on each node.
	scheduleOnto := func(count int, quantity string) map[string]int {
		GinkgoHelper()
		pods := test.UnschedulablePods(test.PodOptions{ResourceRequirements: corev1.ResourceRequirements{Requests: cpu(quantity)}}, count)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		placements := map[string]int{}
		for _, pod := range pods {
			placements[ExpectScheduled(ctx, env.Client, pod).Name]++
		}
		return placements
	}

	Context("with cpu-loaded nodes in name order a=5, b=8, c=2 cpu requested", func() {
		BeforeEach(func() {
			existingNode("node-a", cpu("5"))
			existingNode("node-b", cpu("8"))
			existingNode("node-c", cpu("2"))
		})
		It("should keep name order when no scoring strategy is configured", func() {
			Expect(scheduleOnto(1, "1")).To(Equal(map[string]int{"node-a": 1}))
		})
		It("should pick the most allocated node for MostAllocated", func() {
			withSchedulerConfig(`{"nodeResourcesFit":{"scoringStrategy":{"type":"MostAllocated"}}}`)
			Expect(scheduleOnto(1, "1")).To(Equal(map[string]int{"node-b": 1}))
		})
		It("should pick the least allocated node for LeastAllocated", func() {
			withSchedulerConfig(`{"nodeResourcesFit":{"scoringStrategy":{"type":"LeastAllocated"}}}`)
			Expect(scheduleOnto(1, "1")).To(Equal(map[string]int{"node-c": 1}))
		})
		It("should re-rank a node after each pod is simulated onto it", func() {
			// node-c goes 2 -> 4 -> 6 cpu, at which point node-a (5 cpu) is less allocated and takes the third pod. A
			// stale ordering would have put all three on node-c.
			withSchedulerConfig(`{"nodeResourcesFit":{"scoringStrategy":{"type":"LeastAllocated"}}}`)
			Expect(scheduleOnto(3, "2")).To(Equal(map[string]int{"node-c": 2, "node-a": 1}))
		})
		It("should move on to the next ranked node once the most allocated node is full", func() {
			withSchedulerConfig(`{"nodeResourcesFit":{"scoringStrategy":{"type":"MostAllocated"}}}`)
			Expect(scheduleOnto(4, "1")).To(Equal(map[string]int{"node-b": 2, "node-a": 2}))
		})
	})
	It("should score only the configured resources with their weights", func() {
		// node-a scores (20+80)/2=50 and node-b (80+10)/2=45 with equal weights, but weighting cpu 5x makes node-b
		// (80*5+10)/6=68 outrank node-a (20*5+80)/6=30.
		existingNode("node-a", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi")})
		existingNode("node-b", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("1Gi")})
		withSchedulerConfig(`{"nodeResourcesFit":{"scoringStrategy":{"type":"MostAllocated","resources":[{"name":"cpu","weight":5},{"name":"memory","weight":1}]}}}`)
		Expect(scheduleOnto(1, "1")).To(Equal(map[string]int{"node-b": 1}))
	})
	It("should still try initialized nodes before uninitialized ones", func() {
		existingNode("node-a", cpu("2"))
		uninitialized := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10"),
				corev1.ResourceMemory: resource.MustParse("10Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		})
		ExpectApplied(ctx, env.Client, uninitialized)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(uninitialized))
		// The empty uninitialized node scores highest for LeastAllocated, but initialized nodes still come first.
		withSchedulerConfig(`{"nodeResourcesFit":{"scoringStrategy":{"type":"LeastAllocated"}}}`)
		Expect(scheduleOnto(1, "1")).To(Equal(map[string]int{"node-a": 1}))
	})
})
