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

package provisioner_test

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
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/metrics/provisioner"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var provisionerController controller.Controller
var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers/Metrics/Provisioner")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	provisionerController = provisioner.NewController(env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Provisioner Metrics", func() {
	It("should update the provisioner limit metrics", func() {
		limits := v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("10"),
			v1.ResourceMemory:           resource.MustParse("10Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		provisioner := test.Provisioner(test.ProvisionerOptions{
			Limits: limits,
		})
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))

		for k, v := range limits {
			m, found := FindMetricWithLabelValues("karpenter_provisioner_limit", map[string]string{
				"provisioner":   provisioner.GetName(),
				"resource_type": strings.ReplaceAll(k.String(), "-", "_"),
			})
			Expect(found).To(BeTrue())
			Expect(m.GetGauge().GetValue()).To(BeNumerically("~", v.AsApproximateFloat64()))
		}
	})
	It("should update the provisioner usage metrics", func() {
		resources := v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("10"),
			v1.ResourceMemory:           resource.MustParse("10Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}

		provisioner := test.Provisioner(test.ProvisionerOptions{
			Status: v1alpha5.ProvisionerStatus{
				Resources: resources,
			},
		})
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))

		for k, v := range resources {
			m, found := FindMetricWithLabelValues("karpenter_provisioner_usage", map[string]string{
				"provisioner":   provisioner.GetName(),
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
		limits := v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("100"),
			v1.ResourceMemory:           resource.MustParse("100Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("1000Gi"),
		}

		provisioner := test.Provisioner(test.ProvisionerOptions{
			Limits: limits,
			Status: v1alpha5.ProvisionerStatus{
				Resources: resources,
			},
		})
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))

		for k := range resources {
			m, found := FindMetricWithLabelValues("karpenter_provisioner_usage_pct", map[string]string{
				"provisioner":   provisioner.GetName(),
				"resource_type": strings.ReplaceAll(k.String(), "-", "_"),
			})
			Expect(found).To(BeTrue())
			Expect(m.GetGauge().GetValue()).To(BeNumerically("~", 10))
		}
	})
	It("should delete the provisioner state metrics on provisioner delete", func() {
		expectedMetrics := []string{"karpenter_provisioner_limit", "karpenter_provisioner_usage", "karpenter_provisioner_usage_pct"}
		provisioner := test.Provisioner(test.ProvisionerOptions{
			Limits: v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("100"),
				v1.ResourceMemory:           resource.MustParse("100Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("1000Gi"),
			},
			Status: v1alpha5.ProvisionerStatus{
				Resources: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("10"),
					v1.ResourceMemory:           resource.MustParse("10Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))

		for _, name := range expectedMetrics {
			_, found := FindMetricWithLabelValues(name, map[string]string{
				"provisioner": provisioner.GetName(),
			})
			Expect(found).To(BeTrue())
		}

		ExpectDeleted(ctx, env.Client, provisioner)
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))

		for _, name := range expectedMetrics {
			_, found := FindMetricWithLabelValues(name, map[string]string{
				"provisioner": provisioner.GetName(),
			})
			Expect(found).To(BeFalse())
		}
	})
})
