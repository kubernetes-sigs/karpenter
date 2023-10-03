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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/controllers/metrics/nodepool"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var nodePoolController controller.Controller
var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodePoolMetrics")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	nodePoolController = nodepool.NewController(env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Metrics", func() {
	var nodePool *v1beta1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool(v1beta1.NodePool{
			Spec: v1beta1.NodePoolSpec{
				Template: v1beta1.NodeClaimTemplate{
					Spec: v1beta1.NodeClaimSpec{
						NodeClassRef: &v1beta1.NodeClassReference{
							Name: "default",
						},
					},
				},
			},
		})
	})
	It("should update the nodepool limit metrics", func() {
		limits := v1beta1.Limits{
			v1.ResourceCPU:              resource.MustParse("10"),
			v1.ResourceMemory:           resource.MustParse("10Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		nodePool.Spec.Limits = limits
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

		for k, v := range limits {
			m, found := FindMetricWithLabelValues("karpenter_nodepool_limit", map[string]string{
				"nodepool":      nodePool.GetName(),
				"resource_type": strings.ReplaceAll(k.String(), "-", "_"),
			})
			Expect(found).To(BeTrue())
			Expect(m.GetGauge().GetValue()).To(BeNumerically("~", v.AsApproximateFloat64()))
		}
	})
	It("should update the nodepool usage metrics", func() {
		resources := v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("10"),
			v1.ResourceMemory:           resource.MustParse("10Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		nodePool.Status.Resources = resources

		ExpectApplied(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

		for k, v := range resources {
			m, found := FindMetricWithLabelValues("karpenter_nodepool_usage", map[string]string{
				"nodepool":      nodePool.GetName(),
				"resource_type": strings.ReplaceAll(k.String(), "-", "_"),
			})
			Expect(found).To(BeTrue())
			Expect(m.GetGauge().GetValue()).To(BeNumerically("~", v.AsApproximateFloat64()))
		}
	})
	It("should update the usage percentage metrics correctly", func() {
		resources := v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("10"),
			v1.ResourceMemory:           resource.MustParse("10Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		limits := v1beta1.Limits{
			v1.ResourceCPU:              resource.MustParse("100"),
			v1.ResourceMemory:           resource.MustParse("100Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("1000Gi"),
		}
		nodePool.Spec.Limits = limits
		nodePool.Status.Resources = resources

		ExpectApplied(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

		for k := range resources {
			m, found := FindMetricWithLabelValues("karpenter_nodepool_usage_pct", map[string]string{
				"nodepool":      nodePool.GetName(),
				"resource_type": strings.ReplaceAll(k.String(), "-", "_"),
			})
			Expect(found).To(BeTrue())
			Expect(m.GetGauge().GetValue()).To(BeNumerically("~", 10))
		}
	})
	It("should delete the nodepool state metrics on nodepool delete", func() {
		expectedMetrics := []string{"karpenter_nodepool_limit", "karpenter_nodepool_usage", "karpenter_nodepool_usage_pct"}
		nodePool.Spec.Limits = v1beta1.Limits{
			v1.ResourceCPU:              resource.MustParse("100"),
			v1.ResourceMemory:           resource.MustParse("100Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("1000Gi"),
		}
		nodePool.Status.Resources = v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("10"),
			v1.ResourceMemory:           resource.MustParse("10Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		ExpectApplied(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

		for _, name := range expectedMetrics {
			_, found := FindMetricWithLabelValues(name, map[string]string{
				"nodepool": nodePool.GetName(),
			})
			Expect(found).To(BeTrue())
		}

		ExpectDeleted(ctx, env.Client, nodePool)
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))

		for _, name := range expectedMetrics {
			_, found := FindMetricWithLabelValues(name, map[string]string{
				"nodepool": nodePool.GetName(),
			})
			Expect(found).To(BeFalse())
		}
	})
})
