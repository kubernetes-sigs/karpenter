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
	"time"

	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Liveness", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
	})
	DescribeTable(
		"Liveness",
		func(isManagedNodeClaim bool) {
			nodeClaimOpts := []v1.NodeClaim{{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey: nodePool.Name,
					},
				},
				Spec: v1.NodeClaimSpec{
					Resources: v1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:      resource.MustParse("2"),
							corev1.ResourceMemory:   resource.MustParse("50Mi"),
							corev1.ResourcePods:     resource.MustParse("5"),
							fake.ResourceGPUVendorA: resource.MustParse("1"),
						},
					},
				},
			}}
			if !isManagedNodeClaim {
				nodeClaimOpts = append(nodeClaimOpts, v1.NodeClaim{
					Spec: v1.NodeClaimSpec{
						NodeClassRef: &v1.NodeClassReference{
							Group: "karpenter.k8s.aws",
							Kind:  "EC2NodeClass",
							Name:  "default",
						},
					},
				})
			}
			nodeClaim := test.NodeClaim(nodeClaimOpts...)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

			// If the node hasn't registered in the registration timeframe, then we deprovision the NodeClaim
			fakeClock.Step(time.Minute * 20)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
			ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
			if isManagedNodeClaim {
				ExpectNotFound(ctx, env.Client, nodeClaim)
			} else {
				ExpectExists(ctx, env.Client, nodeClaim)
			}
		},
		Entry("should delete the nodeClaim when the Node hasn't registered past the registration ttl", true),
		Entry("should ignore NodeClaims not managed by this Karpenter instance", false),
	)
	It("shouldn't delete the nodeClaim when the node has registered past the registration ttl", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1.NodeClaimSpec{
				Resources: v1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:      resource.MustParse("2"),
						corev1.ResourceMemory:   resource.MustParse("50Mi"),
						corev1.ResourcePods:     resource.MustParse("5"),
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		node := test.NodeClaimLinkedNode(nodeClaim)
		ExpectApplied(ctx, env.Client, node)

		// Node and nodeClaim should still exist
		fakeClock.Step(time.Minute * 20)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		ExpectExists(ctx, env.Client, nodeClaim)
		ExpectExists(ctx, env.Client, node)
	})
	It("should delete the NodeClaim when the NodeClaim hasn't launched past the registration ttl", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1.NodeClaimSpec{
				Resources: v1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:      resource.MustParse("2"),
						corev1.ResourceMemory:   resource.MustParse("50Mi"),
						corev1.ResourcePods:     resource.MustParse("5"),
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
				},
			},
		})
		cloudProvider.AllowedCreateCalls = 0 // Don't allow Create() calls to succeed
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		_ = ExpectObjectReconcileFailed(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		// If the node hasn't registered in the registration timeframe, then we deprovision the nodeClaim
		fakeClock.Step(time.Minute * 20)
		_ = ExpectObjectReconcileFailed(ctx, env.Client, nodeClaimController, nodeClaim)
		ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
		ExpectNotFound(ctx, env.Client, nodeClaim)
	})
})
