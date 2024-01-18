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

package v1beta1_test

import (
	"context"
	"math/rand"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	. "knative.dev/pkg/logging/testing"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "v1beta1")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("OrderByWeight", func() {
	It("should order the NodePools by weight", func() {
		// Generate 10 NodePools that have random weights, some might have the same weights
		var nodePools []v1beta1.NodePool
		for i := 0; i < 10; i++ {
			np := test.NodePool(v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Weight: lo.ToPtr[int32](int32(rand.Intn(100) + 1)), //nolint:gosec
				},
			})
			nodePools = append(nodePools, *np)
		}

		nodePools = lo.Shuffle(nodePools)
		nodePoolList := v1beta1.NodePoolList{Items: nodePools}
		nodePoolList.OrderByWeight()

		lastWeight := 101 // This is above the allowed weight values
		for _, np := range nodePoolList.Items {
			Expect(lo.FromPtr(np.Spec.Weight)).To(BeNumerically("<=", lastWeight))
			lastWeight = int(lo.FromPtr(np.Spec.Weight))
		}
	})
	It("should order the NodePools by name when the weights are the same", func() {
		// Generate 10 NodePools with the same weight
		var nodePools []v1beta1.NodePool
		for i := 0; i < 10; i++ {
			np := test.NodePool(v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Weight: lo.ToPtr[int32](10),
				},
			})
			nodePools = append(nodePools, *np)
		}

		nodePools = lo.Shuffle(nodePools)
		nodePoolList := v1beta1.NodePoolList{Items: nodePools}
		nodePoolList.OrderByWeight()

		lastName := "zzzzzzzzzzzzzzzzzzzzzzzz" // large string value
		for _, np := range nodePoolList.Items {
			Expect(np.Name < lastName).To(BeTrue())
			lastName = np.Name
		}
	})
})
