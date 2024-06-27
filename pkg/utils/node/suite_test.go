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

package node_test

import (
	"context"
	"testing"

	"sigs.k8s.io/karpenter/pkg/utils/node"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	. "knative.dev/pkg/logging/testing"

	"k8s.io/client-go/kubernetes/scheme"

	"sigs.k8s.io/karpenter/pkg/apis"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/test"
)

var (
	ctx context.Context
	env *test.Environment
)

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodeUtils")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...), test.WithFieldIndexers(test.NodeClaimFieldIndexer(ctx)))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("NodeUtils", func() {
	var testNode *v1.Node
	var nodeClaim *v1beta1.NodeClaim
	BeforeEach(func() {
		nodeClaim = test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClassRef: &v1beta1.NodeClassReference{
					Kind:       "NodeClassRef",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
	})
	It("should return nodeClaim for node which has the same provider ID", func() {
		testNode = test.NodeClaimLinkedNode(nodeClaim)
		ExpectApplied(ctx, env.Client, testNode, nodeClaim)

		nodeClaims, err := node.GetNodeClaims(ctx, testNode, env.Client)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodeClaims).To(HaveLen(1))
		for _, nc := range nodeClaims {
			Expect(nc.Status.ProviderID).To(BeEquivalentTo(testNode.Spec.ProviderID))
		}
	})
	It("should not return nodeClaim for node since the node supplied here has different provider ID", func() {
		testNode = test.Node(test.NodeOptions{
			ProviderID: "testID",
		})
		ExpectApplied(ctx, env.Client, testNode, nodeClaim)

		nodeClaims, err := node.GetNodeClaims(ctx, testNode, env.Client)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodeClaims).To(HaveLen(0))
	})
})
