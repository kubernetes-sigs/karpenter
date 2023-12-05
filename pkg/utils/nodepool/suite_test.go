/*
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

package nodepool_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	. "knative.dev/pkg/logging/testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	karpenterapis "sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	nodepoolutil "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodePoolUtils")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(karpenterapis.CRDs...))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("NodePoolUtils", func() {
	It("should patch the status on a NodePool", func() {
		nodePool := test.NodePool(v1beta1.NodePool{
			Spec: v1beta1.NodePoolSpec{
				Template: v1beta1.NodeClaimTemplate{
					Spec: v1beta1.NodeClaimSpec{
						NodeClassRef: &v1beta1.NodeClassReference{
							Kind:       "NodeClassRef",
							APIVersion: "test.cloudprovider/v1",
							Name:       "default",
						},
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)

		stored := nodePool.DeepCopy()
		nodePool.Status.Resources = v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("10"),
			v1.ResourceMemory:           resource.MustParse("10Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		Expect(nodepoolutil.PatchStatus(ctx, env.Client, stored, nodePool)).To(Succeed())

		retrieved := &v1beta1.NodePool{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodePool), retrieved)).To(Succeed())
		ExpectResources(retrieved.Status.Resources, nodePool.Status.Resources)
	})
})
