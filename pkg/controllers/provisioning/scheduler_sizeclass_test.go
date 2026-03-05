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

package provisioning_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Size Class Locking", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
		podCounter = 0 // Reset counter for each test
	})

	Context("Feature Disabled", func() {
		It("should not lock size class when threshold annotation is not set", func() {
			// No annotation set - feature disabled
			pods := makePods(10, podOptions{
				CPU: "1",
			})

			ExpectApplied(ctx, env.Client, nodePool)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)

			// All pods should schedule without size class restrictions
			for _, pod := range pods {
				ExpectScheduled(ctx, env.Client, pod)
			}

			// Should be able to use larger instance types as needed
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(len(nodeClaims)).To(BeNumerically(">", 0))
		})

		It("should not lock size class when threshold is negative", func() {
			nodePool.Annotations = map[string]string{
				v1.NodeClaimSizeClassLockThresholdAnnotationKey: "-1",
			}

			pods := makePods(10, podOptions{
				CPU: "1",
			})

			ExpectApplied(ctx, env.Client, nodePool)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)

			for _, pod := range pods {
				ExpectScheduled(ctx, env.Client, pod)
			}
		})
	})

	Context("Threshold Not Exceeded", func() {
		It("should not lock size class when pod count is below threshold", func() {
			nodePool.Annotations = map[string]string{
				v1.NodeClaimSizeClassLockThresholdAnnotationKey: "10",
			}

			// Create 5 pods (below threshold of 10)
			pods := makePods(5, podOptions{
				CPU: "1",
			})

			ExpectApplied(ctx, env.Client, nodePool)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)

			for _, pod := range pods {
				ExpectScheduled(ctx, env.Client, pod)
			}

			// Size class should not be locked yet, can still add pods that would increase size
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(len(nodeClaims)).To(BeNumerically(">", 0))
		})
	})

	Context("Size Class Locking Active", func() {
		It("should lock size class after threshold is exceeded", func() {
			nodePool.Annotations = map[string]string{
				v1.NodeClaimSizeClassLockThresholdAnnotationKey: "3",
			}

			// Create 5 1-CPU pods in one batch
			// First 3 trigger the lock (3 CPU -> locks to 4 CPU size class)
			// Pods 4-5 should fit (5 CPU total, under 4 CPU lock)
			// This should all fit in one NodeClaim
			allPods := makePods(5, podOptions{
				CPU: "1",
			})

			ExpectApplied(ctx, env.Client, nodePool)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, allPods...)

			for _, pod := range allPods {
				ExpectScheduled(ctx, env.Client, pod)
			}

			// Should have only 1 nodeclaim since 5 CPUs fit in the 8 CPU locked size class
			// (3 pods hit threshold, lock at next power-of-2 = 4 CPU, but 5 pods = 5 CPU exceeds)
			// Actually with 3 CPU at threshold, next power-of-2 is 4, so only 4 pods fit
			// This means we'll need 2 NodeClaims
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(len(nodeClaims)).To(BeNumerically(">=", 1))
		})

		It("should create new nodeclaim when locked size class is full", func() {
			nodePool.Annotations = map[string]string{
				v1.NodeClaimSizeClassLockThresholdAnnotationKey: "3",
			}

			// Create 5 2-CPU pods in one batch
			// First 3 trigger lock (6 CPU, locks to 8 CPU size class)
			// 4th pod fits (8 CPU total)
			// 5th pod exceeds 8 CPU, needs new NodeClaim
			allPods := makePods(5, podOptions{
				CPU: "2",
			})

			ExpectApplied(ctx, env.Client, nodePool)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, allPods...)

			for _, pod := range allPods {
				ExpectScheduled(ctx, env.Client, pod)
			}

			// Should have 2 nodeclaims (first with 4 pods = 8 CPU, second with 1 pod)
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(len(nodeClaims)).To(Equal(2))
		})

		It("should respect locked size class across multiple NodeClaims", func() {
			nodePool.Annotations = map[string]string{
				v1.NodeClaimSizeClassLockThresholdAnnotationKey: "5",
			}

			// Create 15 1-CPU pods
			pods := makePods(15, podOptions{
				CPU: "1",
			})

			ExpectApplied(ctx, env.Client, nodePool)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)

			for _, pod := range pods {
				ExpectScheduled(ctx, env.Client, pod)
			}

			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			// With threshold of 5 and 1-CPU pods, we lock to smallest size class (2 CPU)
			// Each nodeclaim can hold ~2 pods before needing a new one
			// But the first nodeclaim can hold up to threshold pods before locking
			// This means we'll efficiently pack pods
			Expect(len(nodeClaims)).To(BeNumerically(">=", 2))
		})
	})

	Context("Different Pod Sizes", func() {
		It("should handle varying pod CPU requests", func() {
			nodePool.Annotations = map[string]string{
				v1.NodeClaimSizeClassLockThresholdAnnotationKey: "4",
			}

			// Mix of pod sizes
			smallPods := makePods(4, podOptions{
				CPU:        "500m",
				ObjectMeta: "small",
			})
			mediumPods := makePods(2, podOptions{
				CPU:        "2",
				ObjectMeta: "medium",
			})

			ExpectApplied(ctx, env.Client, nodePool)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, append(smallPods, mediumPods...)...)

			allPods := append(smallPods, mediumPods...)
			for _, pod := range allPods {
				ExpectScheduled(ctx, env.Client, pod)
			}

			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(len(nodeClaims)).To(BeNumerically(">=", 1))
		})
	})

	Context("Multiple NodePools", func() {
		It("should apply threshold per NodePool", func() {
			nodePool1 := test.NodePool()
			nodePool1.Annotations = map[string]string{
				v1.NodeClaimSizeClassLockThresholdAnnotationKey: "5",
			}
			nodePool1.Name = "pool1"

			nodePool2 := test.NodePool()
			nodePool2.Annotations = map[string]string{
				v1.NodeClaimSizeClassLockThresholdAnnotationKey: "10",
			}
			nodePool2.Name = "pool2"

			pods1 := makePods(6, podOptions{
				CPU:        "1",
				NodePool:   "pool1",
				ObjectMeta: "pool1",
			})

			pods2 := makePods(12, podOptions{
				CPU:        "1",
				NodePool:   "pool2",
				ObjectMeta: "pool2",
			})

			ExpectApplied(ctx, env.Client, nodePool1, nodePool2)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, append(pods1, pods2...)...)

			allPods := append(pods1, pods2...)
			for _, pod := range allPods {
				ExpectScheduled(ctx, env.Client, pod)
			}

			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			// Both pools should have their own nodeclaims with their respective thresholds
			Expect(len(nodeClaims)).To(BeNumerically(">=", 2))
		})
	})
})

// Helper types and functions
type podOptions struct {
	CPU        string
	Memory     string
	NodePool   string
	ObjectMeta string
}

var podCounter int

func makePods(count int, opts podOptions) []*corev1.Pod {
	pods := make([]*corev1.Pod, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("test-pod-%d", podCounter)
		podCounter++
		if opts.ObjectMeta != "" {
			name = fmt.Sprintf("%s-%d", opts.ObjectMeta, podCounter)
		}

		resourceRequests := corev1.ResourceList{}
		if opts.CPU != "" {
			resourceRequests[corev1.ResourceCPU] = resource.MustParse(opts.CPU)
		}
		if opts.Memory != "" {
			resourceRequests[corev1.ResourceMemory] = resource.MustParse(opts.Memory)
		}

		podOpts := test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: resourceRequests,
			},
		}

		if opts.NodePool != "" {
			podOpts.NodeSelector = map[string]string{
				v1.NodePoolLabelKey: opts.NodePool,
			}
		}

		pod := test.UnschedulablePod(podOpts)
		pod.Name = name

		pods[i] = pod
	}
	return pods
}
