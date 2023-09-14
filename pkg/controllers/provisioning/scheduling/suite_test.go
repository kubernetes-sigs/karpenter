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

//nolint:gosec
package scheduling_test

import (
	"context"
	"math"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	pscheduling "github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var prov *provisioning.Provisioner
var env *test.Environment
var fakeClock *clock.FakeClock
var cluster *state.Cluster
var cloudProvider *fake.CloudProvider
var nodeStateController controller.Controller
var machineStateController controller.Controller
var nodeClaimStateController controller.Controller
var podStateController controller.Controller

const csiProvider = "fake.csi.provider"

func TestScheduling(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers/Scheduling")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	ctx = settings.ToContext(ctx, test.Settings())
	cloudProvider = fake.NewCloudProvider()
	instanceTypes, _ := cloudProvider.GetInstanceTypes(ctx, nil)
	// set these on the cloud provider, so we can manipulate them if needed
	cloudProvider.InstanceTypes = instanceTypes
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	machineStateController = informer.NewMachineController(env.Client, cluster)
	nodeStateController = informer.NewNodeController(env.Client, cluster)
	machineStateController = informer.NewMachineController(env.Client, cluster)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cluster)
	podStateController = informer.NewPodController(env.Client, cluster)
	prov = provisioning.NewProvisioner(env.Client, env.KubernetesInterface.CoreV1(), events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster)

})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	// reset instance types
	newCP := fake.CloudProvider{}
	cloudProvider.InstanceTypes, _ = newCP.GetInstanceTypes(ctx, nil)
	cloudProvider.CreateCalls = nil
	pscheduling.ResetDefaultStorageClass()
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cluster.Reset()
})

// nolint:gocyclo
func ExpectMaxSkew(ctx context.Context, c client.Client, namespace string, constraint *v1.TopologySpreadConstraint) Assertion {
	nodes := &v1.NodeList{}
	ExpectWithOffset(1, c.List(ctx, nodes)).To(Succeed())
	pods := &v1.PodList{}
	ExpectWithOffset(1, c.List(ctx, pods, scheduling.TopologyListOptions(namespace, constraint.LabelSelector))).To(Succeed())
	skew := map[string]int{}

	nodeMap := map[string]*v1.Node{}
	for i, node := range nodes.Items {
		nodeMap[node.Name] = &nodes.Items[i]
	}

	for i, pod := range pods.Items {
		if scheduling.IgnoredForTopology(&pods.Items[i]) {
			continue
		}
		node := nodeMap[pod.Spec.NodeName]
		if pod.Spec.NodeName == node.Name {
			if constraint.TopologyKey == v1.LabelHostname {
				skew[node.Name]++ // Check node name since hostname labels aren't applied
			}
			if constraint.TopologyKey == v1.LabelTopologyZone {
				if key, ok := node.Labels[constraint.TopologyKey]; ok {
					skew[key]++
				}
			}
			if constraint.TopologyKey == v1alpha5.LabelCapacityType {
				if key, ok := node.Labels[constraint.TopologyKey]; ok {
					skew[key]++
				}
			}
		}
	}

	var minCount = math.MaxInt
	var maxCount = math.MinInt
	for _, count := range skew {
		if count < minCount {
			minCount = count
		}
		if count > maxCount {
			maxCount = count
		}
	}
	return Expect(maxCount - minCount)
}
