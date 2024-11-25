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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Hydration", func() {
	DescribeTable(
		"Hydration",
		func(isNodeClaimManaged bool) {
			nodeClassRef := lo.Ternary(isNodeClaimManaged, &v1.NodeClassReference{
				Group: "karpenter.test.sh",
				Kind:  "TestNodeClass",
				Name:  "default",
			}, &v1.NodeClassReference{
				Group: "karpenter.test.sh",
				Kind:  "UnmanagedNodeClass",
				Name:  "default",
			})
			nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
				Spec: v1.NodeClaimSpec{
					NodeClassRef: nodeClassRef,
				},
			})
			delete(nodeClaim.Labels, v1.NodeClassLabelKey(nodeClassRef.GroupKind()))
			delete(node.Labels, v1.NodeClassLabelKey(nodeClassRef.GroupKind()))
			// Launch the NodeClaim to ensure the lifecycle controller doesn't override the provider-id and break the
			// link between the Node and NodeClaim.
			nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
			ExpectApplied(ctx, env.Client, nodeClaim, node)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)

			// The missing NodeClass label should have been propagated to both the Node and NodeClaim
			node = ExpectExists(ctx, env.Client, node)
			fmt.Printf("provider id: %s\n", node.Spec.ProviderID)
			value, ok := node.Labels[v1.NodeClassLabelKey(nodeClassRef.GroupKind())]
			Expect(ok).To(Equal(isNodeClaimManaged))
			if isNodeClaimManaged {
				Expect(value).To(Equal(nodeClassRef.Name))
			}

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			value, ok = nodeClaim.Labels[v1.NodeClassLabelKey(nodeClassRef.GroupKind())]
			Expect(ok).To(Equal(isNodeClaimManaged))
			if isNodeClaimManaged {
				Expect(value).To(Equal(nodeClassRef.Name))
			}
		},
		Entry("should hydrate missing metadata onto the NodeClaim and Node", true),
		Entry("should ignore NodeClaims which aren't managed by this Karpenter instance", false),
	)
})
