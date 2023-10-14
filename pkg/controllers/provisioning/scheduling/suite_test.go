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
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/options"
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
	ctx = options.ToContext(ctx, test.Options())
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
	GinkgoHelper()
	nodes := &v1.NodeList{}
	Expect(c.List(ctx, nodes)).To(Succeed())
	pods := &v1.PodList{}
	Expect(c.List(ctx, pods, scheduling.TopologyListOptions(namespace, constraint.LabelSelector))).To(Succeed())
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

func ExpectDeleteAllUnscheduledPods(ctx2 context.Context, c client.Client) {
	var pods v1.PodList
	Expect(c.List(ctx2, &pods)).To(Succeed())
	for i := range pods.Items {
		if pods.Items[i].Spec.NodeName == "" {
			ExpectDeleted(ctx2, c, &pods.Items[i])
		}
	}
}

// Functions below this line are used for the instance type selection testing
// -----------
func supportedInstanceTypes(nodeClaim *v1beta1.NodeClaim) (res []*cloudprovider.InstanceType) {
	reqs := pscheduling.NewNodeSelectorRequirements(nodeClaim.Spec.Requirements...)
	return lo.Filter(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
		return reqs.Get(v1.LabelInstanceTypeStable).Has(i.Name)
	})
}

func getInstanceTypeMap(its []*cloudprovider.InstanceType) map[string]*cloudprovider.InstanceType {
	return lo.SliceToMap(its, func(it *cloudprovider.InstanceType) (string, *cloudprovider.InstanceType) {
		return it.Name, it
	})
}

func getMinPrice(its []*cloudprovider.InstanceType) float64 {
	minPrice := math.MaxFloat64
	for _, it := range its {
		for _, of := range it.Offerings {
			minPrice = math.Min(minPrice, of.Price)
		}
	}
	return minPrice
}

func filterInstanceTypes(types []*cloudprovider.InstanceType, pred func(i *cloudprovider.InstanceType) bool) []*cloudprovider.InstanceType {
	var ret []*cloudprovider.InstanceType
	for _, it := range types {
		if pred(it) {
			ret = append(ret, it)
		}
	}
	return ret
}

func ExpectInstancesWithOffering(instanceTypes []*cloudprovider.InstanceType, capacityType string, zone string) {
	for _, it := range instanceTypes {
		matched := false
		for _, offering := range it.Offerings {
			if offering.CapacityType == capacityType && offering.Zone == zone {
				matched = true
			}
		}
		Expect(matched).To(BeTrue(), fmt.Sprintf("expected to find zone %s / capacity type %s in an offering", zone, capacityType))
	}
}

func ExpectInstancesWithLabel(instanceTypes []*cloudprovider.InstanceType, label string, value string) {
	for _, it := range instanceTypes {
		switch label {
		case v1.LabelArchStable:
			Expect(it.Requirements.Get(v1.LabelArchStable).Has(value)).To(BeTrue(), fmt.Sprintf("expected to find an arch of %s", value))
		case v1.LabelOSStable:
			Expect(it.Requirements.Get(v1.LabelOSStable).Has(value)).To(BeTrue(), fmt.Sprintf("expected to find an OS of %s", value))
		case v1.LabelTopologyZone:
			{
				matched := false
				for _, offering := range it.Offerings {
					if offering.Zone == value {
						matched = true
						break
					}
				}
				Expect(matched).To(BeTrue(), fmt.Sprintf("expected to find zone %s in an offering", value))
			}
		case v1beta1.CapacityTypeLabelKey:
			{
				matched := false
				for _, offering := range it.Offerings {
					if offering.CapacityType == value {
						matched = true
						break
					}
				}
				Expect(matched).To(BeTrue(), fmt.Sprintf("expected to find caapacity type %s in an offering", value))
			}
		default:
			Fail(fmt.Sprintf("unsupported label %s in test", label))
		}
	}
}
