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
	"time"

	"k8s.io/apimachinery/pkg/types"
	clock "k8s.io/utils/clock/testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/controllers/metrics/node"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/controller"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"

	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var ctx context.Context
var fakeClock *clock.FakeClock
var env *test.Environment
var cluster *state.Cluster
var nodeController controller.Controller
var metricsStateController controller.Controller
var cloudProvider *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodeMetrics")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))

	ctx = options.ToContext(ctx, test.Options())
	cloudProvider = fake.NewCloudProvider()
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeController = informer.NewNodeController(env.Client, cluster)
	metricsStateController = node.NewController(cluster)
})

var _ = AfterSuite(func() {
	ExpectCleanedUp(ctx, env.Client)
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Node Metrics", func() {
	It("should update the allocatable metric", func() {
		resources := v1.ResourceList{
			v1.ResourcePods:   resource.MustParse("100"),
			v1.ResourceCPU:    resource.MustParse("5000"),
			v1.ResourceMemory: resource.MustParse("32Gi"),
		}

		node := test.Node(test.NodeOptions{Allocatable: resources})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, metricsStateController, types.NamespacedName{})

		for k, v := range resources {
			metric, found := FindMetricWithLabelValues("karpenter_nodes_allocatable", map[string]string{
				"node_name":     node.GetName(),
				"resource_type": k.String(),
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetGauge().GetValue()).To(BeNumerically("~", v.AsApproximateFloat64()))
		}
	})
	It("should remove the node metric gauge when the node is deleted", func() {
		resources := v1.ResourceList{
			v1.ResourcePods:   resource.MustParse("100"),
			v1.ResourceCPU:    resource.MustParse("5000"),
			v1.ResourceMemory: resource.MustParse("32Gi"),
		}

		node := test.Node(test.NodeOptions{Allocatable: resources})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, metricsStateController, types.NamespacedName{})

		_, found := FindMetricWithLabelValues("karpenter_nodes_allocatable", map[string]string{
			"node_name": node.GetName(),
		})
		Expect(found).To(BeTrue())

		ExpectDeleted(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, metricsStateController, types.NamespacedName{})

		_, found = FindMetricWithLabelValues("karpenter_nodes_allocatable", map[string]string{
			"node_name": node.GetName(),
		})
		Expect(found).To(BeFalse())
	})
})
