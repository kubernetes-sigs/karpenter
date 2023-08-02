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
	"math/rand"
	"testing"
	"time"

	"github.com/samber/lo"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	cloudproviderapi "k8s.io/cloud-provider/api"
	"k8s.io/csi-translation-lib/plugins"
	clock "k8s.io/utils/clock/testing"
	"knative.dev/pkg/ptr"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
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
var provisioner *v1alpha5.Provisioner
var prov *provisioning.Provisioner
var env *test.Environment
var fakeClock *clock.FakeClock
var cluster *state.Cluster
var cloudProvider *fake.CloudProvider
var nodeStateController controller.Controller
var machineStateController controller.Controller
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
	podStateController = informer.NewPodController(env.Client, cluster)
	prov = provisioning.NewProvisioner(env.Client, env.KubernetesInterface.CoreV1(), events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster)

})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	provisioner = test.Provisioner(test.ProvisionerOptions{Requirements: []v1.NodeSelectorRequirement{{
		Key:      v1alpha5.LabelCapacityType,
		Operator: v1.NodeSelectorOpIn,
		Values:   []string{v1alpha5.CapacityTypeSpot, v1alpha5.CapacityTypeOnDemand},
	}}})
	// reset instance types
	newCP := fake.CloudProvider{}
	cloudProvider.InstanceTypes, _ = newCP.GetInstanceTypes(context.Background(), nil)
	cloudProvider.CreateCalls = nil
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cluster.Reset()
})

var _ = Describe("Custom Constraints", func() {
	Context("Provisioner with Labels", func() {
		It("should schedule unconstrained pods that don't have matching node selectors", func() {
			provisioner.Spec.Labels = map[string]string{"test-key": "test-value"}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue("test-key", "test-value"))
		})
		It("should not schedule pods that have conflicting node selectors", func() {
			provisioner.Spec.Labels = map[string]string{"test-key": "test-value"}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeSelector: map[string]string{"test-key": "different-value"}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule pods that have node selectors with undefined key", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeSelector: map[string]string{"test-key": "test-value"}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule pods that have matching requirements", func() {
			provisioner.Spec.Labels = map[string]string{"test-key": "test-value"}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value", "another-value"}},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue("test-key", "test-value"))
		})
		It("should not schedule pods that have conflicting requirements", func() {
			provisioner.Spec.Labels = map[string]string{"test-key": "test-value"}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"another-value"}},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
	})
	Context("Well Known Labels", func() {
		It("should use provisioner constraints", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-2"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-2"))
		})
		It("should use node selectors", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-2"}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-2"))
		})
		It("should not schedule nodes with a hostname selector", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeSelector: map[string]string{v1.LabelHostname: "red-node"}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule the pod if nodeselector unknown", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyZone: "unknown"}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule if node selector outside of provisioner constraints", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-2"}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule compatible requirements with Operator=In", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-3"}},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should schedule compatible requirements with Operator=Gt", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
				Key: fake.IntegerInstanceLabelKey, Operator: v1.NodeSelectorOpGt, Values: []string{"8"},
			}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(fake.IntegerInstanceLabelKey, "16"))
		})
		It("should schedule compatible requirements with Operator=Lt", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
				Key: fake.IntegerInstanceLabelKey, Operator: v1.NodeSelectorOpLt, Values: []string{"8"},
			}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(fake.IntegerInstanceLabelKey, "2"))
		})
		It("should not schedule incompatible preferences and requirements with Operator=In", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"unknown"}},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule compatible requirements with Operator=NotIn", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-zone-1", "test-zone-2", "unknown"}},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should not schedule incompatible preferences and requirements with Operator=NotIn", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3", "unknown"}},
					}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule compatible preferences and requirements with Operator=In", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3", "unknown"}}},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-2", "unknown"}}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-2"))
		})
		It("should schedule incompatible preferences and requirements with Operator=In", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3", "unknown"}}},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"unknown"}}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule compatible preferences and requirements with Operator=NotIn", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3", "unknown"}}},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-zone-1", "test-zone-3"}}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-2"))
		})
		It("should schedule incompatible preferences and requirements with Operator=NotIn", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3", "unknown"}}},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3"}}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule compatible node selectors, preferences and requirements", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-3"},
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3"}}},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3"}}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should combine multidimensional node selectors, preferences and requirements", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeSelector: map[string]string{
						v1.LabelTopologyZone:       "test-zone-3",
						v1.LabelInstanceTypeStable: "arm-instance-type",
					},
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-3"}},
						{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"default-instance-type", "arm-instance-type"}},
					},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"unknown"}},
						{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpNotIn, Values: []string{"unknown"}},
					},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelInstanceTypeStable, "arm-instance-type"))
		})
	})
	Context("Constraints Validation", func() {
		It("should not schedule pods that have node selectors with restricted labels", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			for label := range v1alpha5.RestrictedLabels {
				pod := test.UnschedulablePod(
					test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: label, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
					}})
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectNotScheduled(ctx, env.Client, pod)
			}
		})
		It("should not schedule pods that have node selectors with restricted domains", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			for domain := range v1alpha5.RestrictedLabelDomains {
				pod := test.UnschedulablePod(
					test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: domain + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
					}})
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectNotScheduled(ctx, env.Client, pod)
			}
		})
		It("should schedule pods that have node selectors with label in restricted domains exceptions list", func() {
			var requirements []v1.NodeSelectorRequirement
			for domain := range v1alpha5.LabelDomainExceptions {
				requirements = append(requirements, v1.NodeSelectorRequirement{Key: domain + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}})
			}
			provisioner.Spec.Requirements = requirements
			ExpectApplied(ctx, env.Client, provisioner)
			for domain := range v1alpha5.LabelDomainExceptions {
				pod := test.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				node := ExpectScheduled(ctx, env.Client, pod)
				Expect(node.Labels).To(HaveKeyWithValue(domain+"/test", "test-value"))
			}
		})
		It("should schedule pods that have node selectors with label in wellknown label list", func() {
			schedulable := []*v1.Pod{
				// Constrained by zone
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-1"}}),
				// Constrained by instanceType
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelInstanceTypeStable: "default-instance-type"}}),
				// Constrained by architecture
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelArchStable: "arm64"}}),
				// Constrained by operatingSystem
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelOSStable: string(v1.Linux)}}),
				// Constrained by capacity type
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1alpha5.LabelCapacityType: "spot"}}),
			}
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, schedulable...)
			for _, pod := range schedulable {
				ExpectScheduled(ctx, env.Client, pod)
			}
		})
	})
	Context("Scheduling Logic", func() {
		It("should not schedule pods that have node selectors with In operator and undefined key", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule pods that have node selectors with NotIn operator and undefined key", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).ToNot(HaveKeyWithValue("test-key", "test-value"))
		})
		It("should not schedule pods that have node selectors with Exists operator and undefined key", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpExists},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule pods that with DoesNotExists operator and undefined key", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpDoesNotExist},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).ToNot(HaveKey("test-key"))
		})
		It("should schedule unconstrained pods that don't have matching node selectors", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue("test-key", "test-value"))
		})
		It("should schedule pods that have node selectors with matching value and In operator", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue("test-key", "test-value"))
		})
		It("should not schedule pods that have node selectors with matching value and NotIn operator", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule the pod with Exists operator and defined key", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpExists},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should not schedule the pod with DoesNotExists operator and defined key", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpDoesNotExist},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule pods that have node selectors with different value and In operator", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"another-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule pods that have node selectors with different value and NotIn operator", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"another-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue("test-key", "test-value"))
		})
		It("should schedule compatible pods to the same node", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value", "another-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pods := []*v1.Pod{
				test.UnschedulablePod(
					test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}},
					}}),
				test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"another-value"}},
				}}),
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
			node1 := ExpectScheduled(ctx, env.Client, pods[0])
			node2 := ExpectScheduled(ctx, env.Client, pods[1])
			Expect(node1.Labels).To(HaveKeyWithValue("test-key", "test-value"))
			Expect(node2.Labels).To(HaveKeyWithValue("test-key", "test-value"))
			Expect(node1.Name).To(Equal(node2.Name))
		})
		It("should schedule incompatible pods to the different node", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value", "another-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pods := []*v1.Pod{
				test.UnschedulablePod(
					test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}},
					}}),
				test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"another-value"}},
				}}),
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
			node1 := ExpectScheduled(ctx, env.Client, pods[0])
			node2 := ExpectScheduled(ctx, env.Client, pods[1])
			Expect(node1.Labels).To(HaveKeyWithValue("test-key", "test-value"))
			Expect(node2.Labels).To(HaveKeyWithValue("test-key", "another-value"))
			Expect(node1.Name).ToNot(Equal(node2.Name))
		})
		It("Exists operator should not overwrite the existing value", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"non-existent-zone"}},
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpExists},
					}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
	})
	Context("Well Known Labels", func() {
		It("should use provisioner constraints", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-2"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-2"))
		})
		It("should use node selectors", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-2"}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-2"))
		})
		It("should not schedule nodes with a hostname selector", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeSelector: map[string]string{v1.LabelHostname: "red-node"}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule the pod if nodeselector unknown", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyZone: "unknown"}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule if node selector outside of provisioner constraints", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-2"}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule compatible requirements with Operator=In", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-3"}},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should schedule compatible requirements with Operator=Gt", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
				Key: fake.IntegerInstanceLabelKey, Operator: v1.NodeSelectorOpGt, Values: []string{"8"},
			}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(fake.IntegerInstanceLabelKey, "16"))
		})
		It("should schedule compatible requirements with Operator=Lt", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
				Key: fake.IntegerInstanceLabelKey, Operator: v1.NodeSelectorOpLt, Values: []string{"8"},
			}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(fake.IntegerInstanceLabelKey, "2"))
		})
		It("should not schedule incompatible preferences and requirements with Operator=In", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"unknown"}},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule compatible requirements with Operator=NotIn", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-zone-1", "test-zone-2", "unknown"}},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should not schedule incompatible preferences and requirements with Operator=NotIn", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3", "unknown"}},
					}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule compatible preferences and requirements with Operator=In", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3", "unknown"}}},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-2", "unknown"}}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-2"))
		})
		It("should schedule incompatible preferences and requirements with Operator=In", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3", "unknown"}}},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"unknown"}}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule compatible preferences and requirements with Operator=NotIn", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3", "unknown"}}},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-zone-1", "test-zone-3"}}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-2"))
		})
		It("should schedule incompatible preferences and requirements with Operator=NotIn", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3", "unknown"}}},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3"}}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule compatible node selectors, preferences and requirements", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-3"},
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3"}}},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2", "test-zone-3"}}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should combine multidimensional node selectors, preferences and requirements", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeSelector: map[string]string{
						v1.LabelTopologyZone:       "test-zone-3",
						v1.LabelInstanceTypeStable: "arm-instance-type",
					},
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-3"}},
						{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"default-instance-type", "arm-instance-type"}},
					},
					NodePreferences: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"unknown"}},
						{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpNotIn, Values: []string{"unknown"}},
					},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelInstanceTypeStable, "arm-instance-type"))
		})
	})
	Context("Constraints Validation", func() {
		It("should not schedule pods that have node selectors with restricted labels", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			for label := range v1alpha5.RestrictedLabels {
				pod := test.UnschedulablePod(
					test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: label, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
					}})
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectNotScheduled(ctx, env.Client, pod)
			}
		})
		It("should not schedule pods that have node selectors with restricted domains", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			for domain := range v1alpha5.RestrictedLabelDomains {
				pod := test.UnschedulablePod(
					test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: domain + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
					}})
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectNotScheduled(ctx, env.Client, pod)
			}
		})
		It("should schedule pods that have node selectors with label in restricted domains exceptions list", func() {
			var requirements []v1.NodeSelectorRequirement
			for domain := range v1alpha5.LabelDomainExceptions {
				requirements = append(requirements, v1.NodeSelectorRequirement{Key: domain + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}})
			}
			provisioner.Spec.Requirements = requirements
			ExpectApplied(ctx, env.Client, provisioner)
			for domain := range v1alpha5.LabelDomainExceptions {
				pod := test.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				node := ExpectScheduled(ctx, env.Client, pod)
				Expect(node.Labels).To(HaveKeyWithValue(domain+"/test", "test-value"))
			}
		})
		It("should schedule pods that have node selectors with label in wellknown label list", func() {
			schedulable := []*v1.Pod{
				// Constrained by zone
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-1"}}),
				// Constrained by instanceType
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelInstanceTypeStable: "default-instance-type"}}),
				// Constrained by architecture
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelArchStable: "arm64"}}),
				// Constrained by operatingSystem
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelOSStable: string(v1.Linux)}}),
				// Constrained by capacity type
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1alpha5.LabelCapacityType: "spot"}}),
			}
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, schedulable...)
			for _, pod := range schedulable {
				ExpectScheduled(ctx, env.Client, pod)
			}
		})
	})
	Context("Scheduling Logic", func() {
		It("should not schedule pods that have node selectors with In operator and undefined key", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule pods that have node selectors with NotIn operator and undefined key", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).ToNot(HaveKeyWithValue("test-key", "test-value"))
		})
		It("should not schedule pods that have node selectors with Exists operator and undefined key", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpExists},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule pods that with DoesNotExists operator and undefined key", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpDoesNotExist},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).ToNot(HaveKey("test-key"))
		})
		It("should schedule unconstrained pods that don't have matching node selectors", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue("test-key", "test-value"))
		})
		It("should schedule pods that have node selectors with matching value and In operator", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue("test-key", "test-value"))
		})
		It("should not schedule pods that have node selectors with matching value and NotIn operator", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule the pod with Exists operator and defined key", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpExists},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should not schedule the pod with DoesNotExists operator and defined key", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpDoesNotExist},
				}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule pods that have node selectors with different value and In operator", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"another-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule pods that have node selectors with different value and NotIn operator", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"another-value"}},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue("test-key", "test-value"))
		})
		It("should schedule compatible pods to the same node", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value", "another-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pods := []*v1.Pod{
				test.UnschedulablePod(
					test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}},
					}}),
				test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
					{Key: "test-key", Operator: v1.NodeSelectorOpNotIn, Values: []string{"another-value"}},
				}}),
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
			node1 := ExpectScheduled(ctx, env.Client, pods[0])
			node2 := ExpectScheduled(ctx, env.Client, pods[1])
			Expect(node1.Labels).To(HaveKeyWithValue("test-key", "test-value"))
			Expect(node2.Labels).To(HaveKeyWithValue("test-key", "test-value"))
			Expect(node1.Name).To(Equal(node2.Name))
		})
		It("should schedule incompatible pods to the different node", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value", "another-value"}}}
			ExpectApplied(ctx, env.Client, provisioner)
			pods := []*v1.Pod{
				test.UnschedulablePod(
					test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}},
					}}),
				test.UnschedulablePod(
					test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: "test-key", Operator: v1.NodeSelectorOpIn, Values: []string{"another-value"}},
					}}),
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
			node1 := ExpectScheduled(ctx, env.Client, pods[0])
			node2 := ExpectScheduled(ctx, env.Client, pods[1])
			Expect(node1.Labels).To(HaveKeyWithValue("test-key", "test-value"))
			Expect(node2.Labels).To(HaveKeyWithValue("test-key", "another-value"))
			Expect(node1.Name).ToNot(Equal(node2.Name))
		})
		It("Exists operator should not overwrite the existing value", func() {
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"non-existent-zone"}},
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpExists},
					}},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
	})
})

var _ = Describe("Preferential Fallback", func() {
	Context("Required", func() {
		It("should not relax the final term", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}},
				{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"default-instance-type"}},
			}
			pod := test.UnschedulablePod()
			pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{
				{MatchExpressions: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}}, // Should not be relaxed
				}},
			}}}}
			// Don't relax
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should relax multiple terms", func() {
			pod := test.UnschedulablePod()
			pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{
				{MatchExpressions: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
				}},
				{MatchExpressions: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
				}},
				{MatchExpressions: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}},
				}},
				{MatchExpressions: []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-2"}}, // OR operator, never get to this one
				}},
			}}}}
			// Success
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-1"))
		})
	})
	Context("Preferred", func() {
		It("should relax all terms", func() {
			pod := test.UnschedulablePod()
			pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{
				{
					Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
					}},
				},
				{
					Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
					}},
				},
			}}}
			// Success
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should relax to use lighter weights", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2"}}}
			pod := test.UnschedulablePod()
			pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{
				{
					Weight: 100, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-3"}},
					}},
				},
				{
					Weight: 50, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-2"}},
					}},
				},
				{
					Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{ // OR operator, never get to this one
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}},
					}},
				},
			}}}
			// Success
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-2"))
		})
		It("should schedule even if preference is conflicting with requirement", func() {
			pod := test.UnschedulablePod()
			pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{
				{
					Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-zone-3"}},
					}},
				},
			},
				RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{
					{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-3"}}, // Should not be relaxed
					}},
				}},
			}}
			// Success
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should schedule even if preference requirements are conflicting", func() {
			pod := test.UnschedulablePod(test.PodOptions{NodePreferences: []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"invalid"}},
			}})
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
	})
})

var _ = Describe("Instance Type Compatibility", func() {
	It("should not schedule if requesting more resources than any instance type has", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("512"),
				}},
		})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
	})
	It("should launch pods with different archs on different instances", func() {
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
			Key:      v1.LabelArchStable,
			Operator: v1.NodeSelectorOpIn,
			Values:   []string{v1alpha5.ArchitectureArm64, v1alpha5.ArchitectureAmd64},
		}}
		nodeNames := sets.NewString()
		ExpectApplied(ctx, env.Client, provisioner)
		pods := []*v1.Pod{
			test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{v1.LabelArchStable: v1alpha5.ArchitectureAmd64},
			}),
			test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{v1.LabelArchStable: v1alpha5.ArchitectureArm64},
			}),
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		for _, pod := range pods {
			node := ExpectScheduled(ctx, env.Client, pod)
			nodeNames.Insert(node.Name)
		}
		Expect(nodeNames.Len()).To(Equal(2))
	})
	It("should exclude instance types that are not supported by the pod constraints (node affinity/instance type)", func() {
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
			Key:      v1.LabelArchStable,
			Operator: v1.NodeSelectorOpIn,
			Values:   []string{v1alpha5.ArchitectureAmd64},
		}}
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(test.PodOptions{
			NodeRequirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"arm-instance-type"},
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		// arm instance type conflicts with the provisioner limitation of AMD only
		ExpectNotScheduled(ctx, env.Client, pod)
	})
	It("should exclude instance types that are not supported by the pod constraints (node affinity/operating system)", func() {
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
			Key:      v1.LabelArchStable,
			Operator: v1.NodeSelectorOpIn,
			Values:   []string{v1alpha5.ArchitectureAmd64},
		}}
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(test.PodOptions{
			NodeRequirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelOSStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"ios"},
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		// there's an instance with an OS of ios, but it has an arm processor so the provider requirements will
		// exclude it
		ExpectNotScheduled(ctx, env.Client, pod)
	})
	It("should exclude instance types that are not supported by the provider constraints (arch)", func() {
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
			Key:      v1.LabelArchStable,
			Operator: v1.NodeSelectorOpIn,
			Values:   []string{v1alpha5.ArchitectureAmd64},
		}}
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("14")}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		// only the ARM instance has enough CPU, but it's not allowed per the provisioner
		ExpectNotScheduled(ctx, env.Client, pod)
	})
	It("should launch pods with different operating systems on different instances", func() {
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
			Key:      v1.LabelArchStable,
			Operator: v1.NodeSelectorOpIn,
			Values:   []string{v1alpha5.ArchitectureArm64, v1alpha5.ArchitectureAmd64},
		}}
		nodeNames := sets.NewString()
		ExpectApplied(ctx, env.Client, provisioner)
		pods := []*v1.Pod{
			test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{v1.LabelOSStable: string(v1.Linux)},
			}),
			test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{v1.LabelOSStable: string(v1.Windows)},
			}),
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		for _, pod := range pods {
			node := ExpectScheduled(ctx, env.Client, pod)
			nodeNames.Insert(node.Name)
		}
		Expect(nodeNames.Len()).To(Equal(2))
	})
	It("should launch pods with different instance type node selectors on different instances", func() {
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
			Key:      v1.LabelArchStable,
			Operator: v1.NodeSelectorOpIn,
			Values:   []string{v1alpha5.ArchitectureArm64, v1alpha5.ArchitectureAmd64},
		}}
		nodeNames := sets.NewString()
		ExpectApplied(ctx, env.Client, provisioner)
		pods := []*v1.Pod{
			test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{v1.LabelInstanceType: "small-instance-type"},
			}),
			test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{v1.LabelInstanceTypeStable: "default-instance-type"},
			}),
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		for _, pod := range pods {
			node := ExpectScheduled(ctx, env.Client, pod)
			nodeNames.Insert(node.Name)
		}
		Expect(nodeNames.Len()).To(Equal(2))
	})
	It("should launch pods with different zone selectors on different instances", func() {
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{{
			Key:      v1.LabelArchStable,
			Operator: v1.NodeSelectorOpIn,
			Values:   []string{v1alpha5.ArchitectureArm64, v1alpha5.ArchitectureAmd64},
		}}
		nodeNames := sets.NewString()
		ExpectApplied(ctx, env.Client, provisioner)
		pods := []*v1.Pod{
			test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-1"},
			}),
			test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-2"},
			}),
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		for _, pod := range pods {
			node := ExpectScheduled(ctx, env.Client, pod)
			nodeNames.Insert(node.Name)
		}
		Expect(nodeNames.Len()).To(Equal(2))
	})
	It("should launch pods with resources that aren't on any single instance type on different instances", func() {
		cloudProvider.InstanceTypes = fake.InstanceTypes(5)
		const fakeGPU1 = "karpenter.sh/super-great-gpu"
		const fakeGPU2 = "karpenter.sh/even-better-gpu"
		cloudProvider.InstanceTypes[0].Capacity[fakeGPU1] = resource.MustParse("25")
		cloudProvider.InstanceTypes[1].Capacity[fakeGPU2] = resource.MustParse("25")

		nodeNames := sets.NewString()
		ExpectApplied(ctx, env.Client, provisioner)
		pods := []*v1.Pod{
			test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits: v1.ResourceList{fakeGPU1: resource.MustParse("1")},
				},
			}),
			// Should pack onto a different instance since no instance type has both GPUs
			test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Limits: v1.ResourceList{fakeGPU2: resource.MustParse("1")},
				},
			}),
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		for _, pod := range pods {
			node := ExpectScheduled(ctx, env.Client, pod)
			nodeNames.Insert(node.Name)
		}
		Expect(nodeNames.Len()).To(Equal(2))
	})
	It("should fail to schedule a pod with resources requests that aren't on a single instance type", func() {
		cloudProvider.InstanceTypes = fake.InstanceTypes(5)
		const fakeGPU1 = "karpenter.sh/super-great-gpu"
		const fakeGPU2 = "karpenter.sh/even-better-gpu"
		cloudProvider.InstanceTypes[0].Capacity[fakeGPU1] = resource.MustParse("25")
		cloudProvider.InstanceTypes[1].Capacity[fakeGPU2] = resource.MustParse("25")

		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Limits: v1.ResourceList{
					fakeGPU1: resource.MustParse("1"),
					fakeGPU2: resource.MustParse("1")},
			},
		})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
	})
	Context("Provider Specific Labels", func() {
		It("should filter instance types that match labels", func() {
			cloudProvider.InstanceTypes = fake.InstanceTypes(5)
			ExpectApplied(ctx, env.Client, provisioner)
			pods := []*v1.Pod{
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{fake.LabelInstanceSize: "large"}}),
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{fake.LabelInstanceSize: "small"}}),
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
			node := ExpectScheduled(ctx, env.Client, pods[0])
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelInstanceTypeStable, "fake-it-4"))
			node = ExpectScheduled(ctx, env.Client, pods[1])
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelInstanceTypeStable, "fake-it-0"))
		})
		It("should not schedule with incompatible labels", func() {
			cloudProvider.InstanceTypes = fake.InstanceTypes(5)
			ExpectApplied(ctx, env.Client, provisioner)
			pods := []*v1.Pod{
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{
					fake.LabelInstanceSize:     "large",
					v1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[0].Name,
				}}),
				test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{
					fake.LabelInstanceSize:     "small",
					v1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[4].Name,
				}}),
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
			ExpectNotScheduled(ctx, env.Client, pods[0])
			ExpectNotScheduled(ctx, env.Client, pods[1])
		})
		It("should schedule optional labels", func() {
			cloudProvider.InstanceTypes = fake.InstanceTypes(5)
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
				// Only some instance types have this key
				{Key: fake.ExoticInstanceLabelKey, Operator: v1.NodeSelectorOpExists},
			}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKey(fake.ExoticInstanceLabelKey))
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelInstanceTypeStable, cloudProvider.InstanceTypes[4].Name))
		})
		It("should schedule without optional labels if disallowed", func() {
			cloudProvider.InstanceTypes = fake.InstanceTypes(5)
			ExpectApplied(ctx, env.Client, test.Provisioner())
			pod := test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
				// Only some instance types have this key
				{Key: fake.ExoticInstanceLabelKey, Operator: v1.NodeSelectorOpDoesNotExist},
			}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).ToNot(HaveKey(fake.ExoticInstanceLabelKey))
		})
	})
})

var _ = Describe("Binpacking", func() {
	It("should schedule a small pod on the smallest instance", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(
			test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceMemory: resource.MustParse("100M"),
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("small-instance-type"))
	})
	It("should schedule a small pod on the smallest possible instance type", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(
			test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceMemory: resource.MustParse("2000M"),
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("small-instance-type"))
	})
	It("should take pod runtime class into consideration", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(
			test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1"),
				},
			}})
		// the pod has overhead of 2 CPUs
		runtimeClass := &nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: "my-runtime-class",
			},
			Handler: "default",
			Overhead: &nodev1.Overhead{
				PodFixed: v1.ResourceList{
					v1.ResourceCPU: resource.MustParse("2"),
				},
			},
		}
		pod.Spec.RuntimeClassName = &runtimeClass.Name
		ExpectApplied(ctx, env.Client, runtimeClass)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		// overhead of 2 + request of 1 = at least 3 CPUs, so it won't fit on small-instance-type which it otherwise
		// would
		Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("default-instance-type"))
	})
	It("should schedule multiple small pods on the smallest possible instance type", func() {
		opts := test.PodOptions{
			Conditions: []v1.PodCondition{{Type: v1.PodScheduled, Reason: v1.PodReasonUnschedulable, Status: v1.ConditionFalse}},
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceMemory: resource.MustParse("10M"),
				},
			}}
		pods := test.Pods(5, opts)
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		nodeNames := sets.NewString()
		for _, p := range pods {
			node := ExpectScheduled(ctx, env.Client, p)
			nodeNames.Insert(node.Name)
			Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("small-instance-type"))
		}
		Expect(nodeNames).To(HaveLen(1))
	})
	It("should create new nodes when a node is at capacity", func() {
		opts := test.PodOptions{
			NodeSelector: map[string]string{v1.LabelArchStable: "amd64"},
			Conditions:   []v1.PodCondition{{Type: v1.PodScheduled, Reason: v1.PodReasonUnschedulable, Status: v1.ConditionFalse}},
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceMemory: resource.MustParse("1.8G"),
				},
			}}
		ExpectApplied(ctx, env.Client, provisioner)
		pods := test.Pods(40, opts)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		nodeNames := sets.NewString()
		for _, p := range pods {
			node := ExpectScheduled(ctx, env.Client, p)
			nodeNames.Insert(node.Name)
			Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("default-instance-type"))
		}
		Expect(nodeNames).To(HaveLen(20))
	})
	It("should pack small and large pods together", func() {
		largeOpts := test.PodOptions{
			NodeSelector: map[string]string{v1.LabelArchStable: "amd64"},
			Conditions:   []v1.PodCondition{{Type: v1.PodScheduled, Reason: v1.PodReasonUnschedulable, Status: v1.ConditionFalse}},
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceMemory: resource.MustParse("1.8G"),
				},
			}}
		smallOpts := test.PodOptions{
			NodeSelector: map[string]string{v1.LabelArchStable: "amd64"},
			Conditions:   []v1.PodCondition{{Type: v1.PodScheduled, Reason: v1.PodReasonUnschedulable, Status: v1.ConditionFalse}},
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceMemory: resource.MustParse("400M"),
				},
			}}

		// Two large pods are all that will fit on the default-instance type (the largest instance type) which will create
		// twenty nodes. This leaves just enough room on each of those newNodes for one additional small pod per node, so we
		// should only end up with 20 newNodes total.
		provPods := append(test.Pods(40, largeOpts), test.Pods(20, smallOpts)...)
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, provPods...)
		nodeNames := sets.NewString()
		for _, p := range provPods {
			node := ExpectScheduled(ctx, env.Client, p)
			nodeNames.Insert(node.Name)
			Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("default-instance-type"))
		}
		Expect(nodeNames).To(HaveLen(20))
	})
	It("should pack newNodes tightly", func() {
		cloudProvider.InstanceTypes = fake.InstanceTypes(5)
		var nodes []*v1.Node
		ExpectApplied(ctx, env.Client, provisioner)
		pods := []*v1.Pod{
			test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4.5")},
				},
			}),
			test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
				},
			}),
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		for _, pod := range pods {
			node := ExpectScheduled(ctx, env.Client, pod)
			nodes = append(nodes, node)
		}
		Expect(nodes).To(HaveLen(2))
		// the first pod consumes nearly all CPU of the largest instance type with no room for the second pod, the
		// second pod is much smaller in terms of resources and should get a smaller node
		Expect(nodes[0].Labels[v1.LabelInstanceTypeStable]).ToNot(Equal(nodes[1].Labels[v1.LabelInstanceTypeStable]))
	})
	It("should handle zero-quantity resource requests", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{"foo.com/weird-resources": resource.MustParse("0")},
				Limits:   v1.ResourceList{"foo.com/weird-resources": resource.MustParse("0")},
			},
		})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		// requesting a resource of quantity zero of a type unsupported by any instance is fine
		ExpectScheduled(ctx, env.Client, pod)
	})
	It("should not schedule pods that exceed every instance type's capacity", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(
			test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceMemory: resource.MustParse("2Ti"),
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
	})
	It("should create new newNodes when a node is at capacity due to pod limits per node", func() {
		opts := test.PodOptions{
			NodeSelector: map[string]string{v1.LabelArchStable: "amd64"},
			Conditions:   []v1.PodCondition{{Type: v1.PodScheduled, Reason: v1.PodReasonUnschedulable, Status: v1.ConditionFalse}},
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceMemory: resource.MustParse("1m"),
					v1.ResourceCPU:    resource.MustParse("1m"),
				},
			}}
		ExpectApplied(ctx, env.Client, provisioner)
		pods := test.Pods(25, opts)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		nodeNames := sets.NewString()
		// all of the test instance types support 5 pods each, so we use the 5 instances of the smallest one for our 25 pods
		for _, p := range pods {
			node := ExpectScheduled(ctx, env.Client, p)
			nodeNames.Insert(node.Name)
			Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("small-instance-type"))
		}
		Expect(nodeNames).To(HaveLen(5))
	})
	It("should take into account initContainer resource requests when binpacking", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(
			test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceMemory: resource.MustParse("1Gi"),
					v1.ResourceCPU:    resource.MustParse("1"),
				},
			},
				InitImage: "pause",
				InitResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceMemory: resource.MustParse("1Gi"),
						v1.ResourceCPU:    resource.MustParse("2"),
					},
				},
			})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("default-instance-type"))
	})
	It("should not schedule pods when initContainer resource requests are greater than available instance types", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(
			test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceMemory: resource.MustParse("1Gi"),
					v1.ResourceCPU:    resource.MustParse("1"),
				},
			},
				InitImage: "pause",
				InitResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceMemory: resource.MustParse("1Ti"),
						v1.ResourceCPU:    resource.MustParse("2"),
					},
				},
			})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
	})
	It("should select for valid instance types, regardless of price", func() {
		// capacity sizes and prices don't correlate here, regardless we should filter and see that all three instance types
		// are valid before preferring the cheapest one 'large'
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "medium",
				Resources: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("2Gi"),
				},
				Offerings: []cloudprovider.Offering{
					{
						CapacityType: v1alpha5.CapacityTypeOnDemand,
						Zone:         "test-zone-1a",
						Price:        3.00,
						Available:    true,
					},
				},
			}),
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "small",
				Resources: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Offerings: []cloudprovider.Offering{
					{
						CapacityType: v1alpha5.CapacityTypeOnDemand,
						Zone:         "test-zone-1a",
						Price:        2.00,
						Available:    true,
					},
				},
			}),
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "large",
				Resources: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("4Gi"),
				},
				Offerings: []cloudprovider.Offering{
					{
						CapacityType: v1alpha5.CapacityTypeOnDemand,
						Zone:         "test-zone-1a",
						Price:        1.00,
						Available:    true,
					},
				},
			}),
		}
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(
			test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Limits: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:    resource.MustParse("1m"),
					v1.ResourceMemory: resource.MustParse("1Mi"),
				},
			}},
		)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		// large is the cheapest, so we should pick it, but the other two types are also valid options
		Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("large"))
		// all three options should be passed to the cloud provider
		possibleInstanceType := sets.NewString(pscheduling.NewNodeSelectorRequirements(cloudProvider.CreateCalls[0].Spec.Requirements...).Get(v1.LabelInstanceTypeStable).Values()...)
		Expect(possibleInstanceType).To(Equal(sets.NewString("small", "medium", "large")))
	})
})

var _ = Describe("In-Flight Nodes", func() {
	It("should not launch a second node if there is an in-flight node that can support the pod", func() {
		opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		}}
		ExpectApplied(ctx, env.Client, provisioner)
		initialPod := test.UnschedulablePod(opts)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node1 := ExpectScheduled(ctx, env.Client, initialPod)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

		secondPod := test.UnschedulablePod(opts)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
		node2 := ExpectScheduled(ctx, env.Client, secondPod)
		Expect(node1.Name).To(Equal(node2.Name))
	})
	It("should not launch a second node if there is an in-flight node that can support the pod (node selectors)", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		initialPod := test.UnschedulablePod(test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		},
			NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1.LabelTopologyZone,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"test-zone-2"},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node1 := ExpectScheduled(ctx, env.Client, initialPod)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

		// the node gets created in test-zone-2
		secondPod := test.UnschedulablePod(test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		},
			NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1.LabelTopologyZone,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"test-zone-1", "test-zone-2"},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
		// test-zone-2 is in the intersection of their node selectors and the node has capacity, so we shouldn't create a new node
		node2 := ExpectScheduled(ctx, env.Client, secondPod)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		Expect(node1.Name).To(Equal(node2.Name))

		// the node gets created in test-zone-2
		thirdPod := test.UnschedulablePod(test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		},
			NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1.LabelTopologyZone,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"test-zone-1", "test-zone-3"},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, thirdPod)
		// node is in test-zone-2, so this pod needs a new node
		node3 := ExpectScheduled(ctx, env.Client, thirdPod)
		Expect(node1.Name).ToNot(Equal(node3.Name))
	})
	It("should launch a second node if a pod won't fit on the existingNodes node", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("1001m"),
			},
		}}
		initialPod := test.UnschedulablePod(opts)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node1 := ExpectScheduled(ctx, env.Client, initialPod)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

		// the node will have 2000m CPU, so these two pods can't both fit on it
		opts.ResourceRequirements.Limits[v1.ResourceCPU] = resource.MustParse("1")
		secondPod := test.UnschedulablePod(opts)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
		node2 := ExpectScheduled(ctx, env.Client, secondPod)
		Expect(node1.Name).ToNot(Equal(node2.Name))
	})
	It("should launch a second node if a pod isn't compatible with the existingNodes node (node selector)", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		}}
		initialPod := test.UnschedulablePod(opts)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node1 := ExpectScheduled(ctx, env.Client, initialPod)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

		secondPod := test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelArchStable: "arm64"}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
		node2 := ExpectScheduled(ctx, env.Client, secondPod)
		Expect(node1.Name).ToNot(Equal(node2.Name))
	})
	It("should launch a second node if an in-flight node is terminating", func() {
		opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		}}
		ExpectApplied(ctx, env.Client, provisioner)
		initialPod := test.UnschedulablePod(opts)
		bindings := ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		ExpectScheduled(ctx, env.Client, initialPod)

		// delete the node/machine
		machine1 := bindings.Get(initialPod).Machine
		node1 := bindings.Get(initialPod).Node
		machine1.Finalizers = nil
		node1.Finalizers = nil
		ExpectApplied(ctx, env.Client, machine1, node1)
		ExpectDeleted(ctx, env.Client, machine1, node1)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machine1))

		secondPod := test.UnschedulablePod(opts)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
		node2 := ExpectScheduled(ctx, env.Client, secondPod)
		Expect(node1.Name).ToNot(Equal(node2.Name))
	})
	Context("Ordering Existing Machines", func() {
		It("should order initialized nodes for scheduling un-initialized nodes", func() {
			ExpectApplied(ctx, env.Client, provisioner)

			var machines []*v1alpha5.NodeClaim
			var nodes []*v1.Node
			for i := 0; i < 100; i++ {
				m := test.Machine(v1alpha5.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						},
					},
				})
				ExpectApplied(ctx, env.Client, m)
				m, n := ExpectMachineDeployed(ctx, env.Client, cluster, cloudProvider, m)
				machines = append(machines, m)
				nodes = append(nodes, n)
			}

			// Make one of the nodes and machines initialized
			elem := rand.Intn(100) //nolint:gosec
			ExpectMakeMachinesInitialized(ctx, env.Client, machines[elem])
			ExpectMakeNodesInitialized(ctx, env.Client, nodes[elem])
			ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machines[elem]))
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[elem]))

			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			scheduledNode := ExpectScheduled(ctx, env.Client, pod)

			// Expect that the scheduled node is equal to the ready node since it's initialized
			Expect(scheduledNode.Name).To(Equal(nodes[elem].Name))
		})
		It("should order initialized nodes for scheduling un-initialized nodes when all other nodes are inflight", func() {
			ExpectApplied(ctx, env.Client, provisioner)

			var machines []*v1alpha5.NodeClaim
			var node *v1.Node
			elem := rand.Intn(100) // The machine/node that will be marked as initialized
			for i := 0; i < 100; i++ {
				m := test.Machine(v1alpha5.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						},
					},
				})
				ExpectApplied(ctx, env.Client, m)
				if i == elem {
					m, node = ExpectMachineDeployed(ctx, env.Client, cluster, cloudProvider, m)
				} else {
					var err error
					m, err = ExpectMachineDeployedNoNode(ctx, env.Client, cluster, cloudProvider, m)
					Expect(err).ToNot(HaveOccurred())
				}
				machines = append(machines, m)
			}

			// Make one of the nodes and machines initialized
			ExpectMakeMachinesInitialized(ctx, env.Client, machines[elem])
			ExpectMakeNodesInitialized(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machines[elem]))
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			scheduledNode := ExpectScheduled(ctx, env.Client, pod)

			// Expect that the scheduled node is equal to node3 since it's initialized
			Expect(scheduledNode.Name).To(Equal(node.Name))
		})
	})
	Context("Topology", func() {
		It("should balance pods across zones with in-flight newNodes", func() {
			labels := map[string]string{"foo": "bar"}
			topology := []v1.TopologySpreadConstraint{{
				TopologyKey:       v1.LabelTopologyZone,
				WhenUnsatisfiable: v1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
				MaxSkew:           1,
			}}
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov,
				test.UnschedulablePods(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: labels}, TopologySpreadConstraints: topology}, 4)...,
			)
			ExpectSkew(ctx, env.Client, "default", &topology[0]).To(ConsistOf(1, 1, 2))

			// reconcile our newNodes with the cluster state so they'll show up as in-flight
			var nodeList v1.NodeList
			Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
			for _, node := range nodeList.Items {
				ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKey{Name: node.Name})
			}

			firstRoundNumNodes := len(nodeList.Items)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov,
				test.UnschedulablePods(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: labels}, TopologySpreadConstraints: topology}, 5)...,
			)
			ExpectSkew(ctx, env.Client, "default", &topology[0]).To(ConsistOf(3, 3, 3))
			Expect(env.Client.List(ctx, &nodeList)).To(Succeed())

			// shouldn't create any new newNodes as the in-flight ones can support the pods
			Expect(nodeList.Items).To(HaveLen(firstRoundNumNodes))
		})
		It("should balance pods across hostnames with in-flight newNodes", func() {
			labels := map[string]string{"foo": "bar"}
			topology := []v1.TopologySpreadConstraint{{
				TopologyKey:       v1.LabelHostname,
				WhenUnsatisfiable: v1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
				MaxSkew:           1,
			}}
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov,
				test.UnschedulablePods(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: labels}, TopologySpreadConstraints: topology}, 4)...,
			)
			ExpectSkew(ctx, env.Client, "default", &topology[0]).To(ConsistOf(1, 1, 1, 1))

			// reconcile our newNodes with the cluster state so they'll show up as in-flight
			var nodeList v1.NodeList
			Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
			for _, node := range nodeList.Items {
				ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKey{Name: node.Name})
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov,
				test.UnschedulablePods(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: labels}, TopologySpreadConstraints: topology}, 5)...,
			)
			// we prefer to launch new newNodes to satisfy the topology spread even though we could technically schedule against existingNodes
			ExpectSkew(ctx, env.Client, "default", &topology[0]).To(ConsistOf(1, 1, 1, 1, 1, 1, 1, 1, 1))
		})
	})
	Context("Taints", func() {
		It("should assume pod will schedule to a tainted node with no taints", func() {
			opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Limits: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("8"),
				},
			}}
			ExpectApplied(ctx, env.Client, provisioner)
			initialPod := test.UnschedulablePod(opts)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
			node1 := ExpectScheduled(ctx, env.Client, initialPod)

			// delete the pod so that the node is empty
			ExpectDeleted(ctx, env.Client, initialPod)
			node1.Spec.Taints = nil
			ExpectApplied(ctx, env.Client, node1)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

			secondPod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
			node2 := ExpectScheduled(ctx, env.Client, secondPod)
			Expect(node1.Name).To(Equal(node2.Name))
		})
		It("should not assume pod will schedule to a tainted node", func() {
			opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Limits: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("8"),
				},
			}}
			ExpectApplied(ctx, env.Client, provisioner)
			initialPod := test.UnschedulablePod(opts)
			bindings := ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
			ExpectScheduled(ctx, env.Client, initialPod)

			machine1 := bindings.Get(initialPod).Machine
			node1 := bindings.Get(initialPod).Node
			machine1.StatusConditions().MarkTrue(v1alpha5.MachineInitialized)
			node1.Labels = lo.Assign(node1.Labels, map[string]string{v1alpha5.LabelNodeInitialized: "true"})

			// delete the pod so that the node is empty
			ExpectDeleted(ctx, env.Client, initialPod)
			// and taint it
			node1.Spec.Taints = append(node1.Spec.Taints, v1.Taint{
				Key:    "foo.com/taint",
				Value:  "tainted",
				Effect: v1.TaintEffectNoSchedule,
			})
			ExpectApplied(ctx, env.Client, machine1, node1)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

			secondPod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
			node2 := ExpectScheduled(ctx, env.Client, secondPod)
			Expect(node1.Name).ToNot(Equal(node2.Name))
		})
		It("should assume pod will schedule to a tainted node with a custom startup taint", func() {
			opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Limits: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("8"),
				},
			}}
			provisioner.Spec.StartupTaints = append(provisioner.Spec.StartupTaints, v1.Taint{
				Key:    "foo.com/taint",
				Value:  "tainted",
				Effect: v1.TaintEffectNoSchedule,
			})
			ExpectApplied(ctx, env.Client, provisioner)
			initialPod := test.UnschedulablePod(opts)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
			node1 := ExpectScheduled(ctx, env.Client, initialPod)

			// delete the pod so that the node is empty
			ExpectDeleted(ctx, env.Client, initialPod)
			// startup taint + node not ready taint = 2
			Expect(node1.Spec.Taints).To(HaveLen(2))
			Expect(node1.Spec.Taints).To(ContainElement(v1.Taint{
				Key:    "foo.com/taint",
				Value:  "tainted",
				Effect: v1.TaintEffectNoSchedule,
			}))
			ExpectApplied(ctx, env.Client, node1)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

			secondPod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
			node2 := ExpectScheduled(ctx, env.Client, secondPod)
			Expect(node1.Name).To(Equal(node2.Name))
		})
		It("should not assume pod will schedule to a node with startup taints after initialization", func() {
			startupTaint := v1.Taint{Key: "ignore-me", Value: "nothing-to-see-here", Effect: v1.TaintEffectNoSchedule}
			provisioner.Spec.StartupTaints = []v1.Taint{startupTaint}
			ExpectApplied(ctx, env.Client, provisioner)
			initialPod := test.UnschedulablePod()
			bindings := ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
			ExpectScheduled(ctx, env.Client, initialPod)

			// delete the pod so that the node is empty
			ExpectDeleted(ctx, env.Client, initialPod)

			// Mark it initialized which only occurs once the startup taint was removed and re-apply only the startup taint.
			// We also need to add resource capacity as after initialization we assume that kubelet has recorded them.

			machine1 := bindings.Get(initialPod).Machine
			node1 := bindings.Get(initialPod).Node
			machine1.StatusConditions().MarkTrue(v1alpha5.MachineInitialized)
			node1.Labels = lo.Assign(node1.Labels, map[string]string{v1alpha5.LabelNodeInitialized: "true"})

			node1.Spec.Taints = []v1.Taint{startupTaint}
			node1.Status.Capacity = v1.ResourceList{v1.ResourcePods: resource.MustParse("10")}
			ExpectApplied(ctx, env.Client, machine1, node1)

			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

			// we should launch a new node since the startup taint is there, but was gone at some point
			secondPod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
			node2 := ExpectScheduled(ctx, env.Client, secondPod)
			Expect(node1.Name).ToNot(Equal(node2.Name))
		})
		It("should consider a tainted NotReady node as in-flight even if initialized", func() {
			opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("10m")},
			}}
			ExpectApplied(ctx, env.Client, provisioner)

			// Schedule to New NodeClaim
			pod := test.UnschedulablePod(opts)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node1 := ExpectScheduled(ctx, env.Client, pod)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
			// Mark Initialized
			node1.Labels[v1alpha5.LabelNodeInitialized] = "true"
			node1.Spec.Taints = []v1.Taint{
				{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoSchedule},
				{Key: v1.TaintNodeUnreachable, Effect: v1.TaintEffectNoSchedule},
				{Key: cloudproviderapi.TaintExternalCloudProvider, Effect: v1.TaintEffectNoSchedule, Value: "true"},
			}
			ExpectApplied(ctx, env.Client, node1)
			// Schedule to In Flight NodeClaim
			pod = test.UnschedulablePod(opts)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node2 := ExpectScheduled(ctx, env.Client, pod)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node2))

			Expect(node1.Name).To(Equal(node2.Name))
		})
	})
	Context("Daemonsets", func() {
		It("should track daemonset usage separately so we know how many DS resources are remaining to be scheduled", func() {
			ds := test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi")}},
				}},
			)
			ExpectApplied(ctx, env.Client, provisioner, ds)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(ds), ds)).To(Succeed())

			opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Limits: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("8"),
				},
			}}
			initialPod := test.UnschedulablePod(opts)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
			node1 := ExpectScheduled(ctx, env.Client, initialPod)

			// create our daemonset pod and manually bind it to the node
			dsPod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					}},
			})
			dsPod.OwnerReferences = append(dsPod.OwnerReferences, metav1.OwnerReference{
				APIVersion:         "apps/v1",
				Kind:               "DaemonSet",
				Name:               ds.Name,
				UID:                ds.UID,
				Controller:         ptr.Bool(true),
				BlockOwnerDeletion: ptr.Bool(true),
			})

			// delete the pod so that the node is empty
			ExpectDeleted(ctx, env.Client, initialPod)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

			ExpectApplied(ctx, env.Client, provisioner, dsPod)
			cluster.ForEachNode(func(f *state.StateNode) bool {
				dsRequests := f.DaemonSetRequests()
				available := f.Available()
				Expect(dsRequests.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 0))
				// no pods so we have the full (16 cpu - 100m overhead)
				Expect(available.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 15.9))
				return true
			})
			ExpectManualBinding(ctx, env.Client, dsPod, node1)
			ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(dsPod))

			cluster.ForEachNode(func(f *state.StateNode) bool {
				dsRequests := f.DaemonSetRequests()
				available := f.Available()
				Expect(dsRequests.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 1))
				// only the DS pod is bound, so available is reduced by one and the DS requested is incremented by one
				Expect(available.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 14.9))
				return true
			})

			opts = test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Limits: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("14.9"),
				},
			}}
			// this pod should schedule on the existingNodes node as the daemonset pod has already bound, meaning that the
			// remaining daemonset resources should be zero leaving 14.9 CPUs for the pod
			secondPod := test.UnschedulablePod(opts)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
			node2 := ExpectScheduled(ctx, env.Client, secondPod)
			Expect(node1.Name).To(Equal(node2.Name))
		})
		It("should handle unexpected daemonset pods binding to the node", func() {
			ds1 := test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					NodeSelector: map[string]string{
						"my-node-label": "value",
					},
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi")}},
				}},
			)
			ds2 := test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("1m"),
					}}}})
			ExpectApplied(ctx, env.Client, provisioner, ds1, ds2)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(ds1), ds1)).To(Succeed())

			opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Limits: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("8"),
				},
			}}
			initialPod := test.UnschedulablePod(opts)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
			node1 := ExpectScheduled(ctx, env.Client, initialPod)
			// this label appears on the node for some reason that Karpenter can't track
			node1.Labels["my-node-label"] = "value"
			ExpectApplied(ctx, env.Client, node1)

			// create our daemonset pod and manually bind it to the node
			dsPod := test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{
					"my-node-label": "value",
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					}},
			})
			dsPod.OwnerReferences = append(dsPod.OwnerReferences, metav1.OwnerReference{
				APIVersion:         "apps/v1",
				Kind:               "DaemonSet",
				Name:               ds1.Name,
				UID:                ds1.UID,
				Controller:         ptr.Bool(true),
				BlockOwnerDeletion: ptr.Bool(true),
			})

			// delete the pod so that the node is empty
			ExpectDeleted(ctx, env.Client, initialPod)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

			ExpectApplied(ctx, env.Client, provisioner, dsPod)
			cluster.ForEachNode(func(f *state.StateNode) bool {
				dsRequests := f.DaemonSetRequests()
				available := f.Available()
				Expect(dsRequests.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 0))
				// no pods, so we have the full (16 CPU - 100m overhead)
				Expect(available.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 15.9))
				return true
			})
			ExpectManualBinding(ctx, env.Client, dsPod, node1)
			ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(dsPod))

			cluster.ForEachNode(func(f *state.StateNode) bool {
				dsRequests := f.DaemonSetRequests()
				available := f.Available()
				Expect(dsRequests.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 1))
				// only the DS pod is bound, so available is reduced by one and the DS requested is incremented by one
				Expect(available.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 14.9))
				return true
			})

			opts = test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Limits: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("15.5"),
				},
			}}
			// This pod should not schedule on the inflight node as it requires more CPU than we have.  This verifies
			// we don't reintroduce a bug where more daemonsets scheduled than anticipated due to unexepected labels
			// appearing on the node which caused us to compute a negative amount of resources remaining for daemonsets
			// which in turn caused us to mis-calculate the amount of resources that were free on the node.
			secondPod := test.UnschedulablePod(opts)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
			node2 := ExpectScheduled(ctx, env.Client, secondPod)
			// must create a new node
			Expect(node1.Name).ToNot(Equal(node2.Name))
		})

	})
	// nolint:gosec
	It("should pack in-flight newNodes before launching new newNodes", func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "medium",
				Resources: v1.ResourceList{
					// enough CPU for four pods + a bit of overhead
					v1.ResourceCPU:  resource.MustParse("4.25"),
					v1.ResourcePods: resource.MustParse("4"),
				},
			}),
		}
		opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("1"),
			},
		}}

		ExpectApplied(ctx, env.Client, provisioner)

		// scheduling in multiple batches random sets of pods
		for i := 0; i < 10; i++ {
			initialPods := test.UnschedulablePods(opts, rand.Intn(10))
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPods...)
			for _, pod := range initialPods {
				node := ExpectScheduled(ctx, env.Client, pod)
				ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
			}
		}

		// due to the in-flight node support, we should pack existing newNodes before launching new node. The end result
		// is that we should only have some spare capacity on our final node
		nodesWithCPUFree := 0
		cluster.ForEachNode(func(n *state.StateNode) bool {
			available := n.Available()
			if available.Cpu().AsApproximateFloat64() >= 1 {
				nodesWithCPUFree++
			}
			return true
		})
		Expect(nodesWithCPUFree).To(BeNumerically("<=", 1))
	})
	It("should not launch a second node if there is an in-flight node that can support the pod (#2011)", func() {
		opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		}}

		// there was a bug in cluster state where we failed to identify the instance type resources when using a
		// MachineTemplateRef so modify our provisioner to use the MachineTemplateRef and ensure that the second pod schedules
		// to the existingNodes node
		provisioner.Spec.Provider = nil
		provisioner.Spec.ProviderRef = &v1alpha5.MachineTemplateRef{}

		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(opts)
		ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, pod)
		var nodes v1.NodeList
		Expect(env.Client.List(ctx, &nodes)).To(Succeed())
		Expect(nodes.Items).To(HaveLen(1))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(&nodes.Items[0]))

		pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodScheduled, Reason: v1.PodReasonUnschedulable, Status: v1.ConditionFalse}}
		ExpectApplied(ctx, env.Client, pod)
		ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(env.Client.List(ctx, &nodes)).To(Succeed())
		// shouldn't create a second node
		Expect(nodes.Items).To(HaveLen(1))
	})
	It("should order initialized nodes for scheduling un-initialized nodes when all other nodes are inflight", func() {
		ExpectApplied(ctx, env.Client, provisioner)

		var machines []*v1alpha5.Machine
		var node *v1.Node
		elem := rand.Intn(100) // The machine/node that will be marked as initialized
		for i := 0; i < 100; i++ {
			m := test.Machine(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					},
				},
			})
			ExpectApplied(ctx, env.Client, m)
			if i == elem {
				m, node = ExpectMachineDeployed(ctx, env.Client, cluster, cloudProvider, m)
			} else {
				var err error
				m, err = ExpectMachineDeployedNoNode(ctx, env.Client, cluster, cloudProvider, m)
				Expect(err).ToNot(HaveOccurred())
			}
			machines = append(machines, m)
		}

		// Make one of the nodes and machines initialized
		ExpectMakeMachinesInitialized(ctx, env.Client, machines[elem])
		ExpectMakeNodesInitialized(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machines[elem]))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		scheduledNode := ExpectScheduled(ctx, env.Client, pod)

		// Expect that the scheduled node is equal to node3 since it's initialized
		Expect(scheduledNode.Name).To(Equal(node.Name))
	})
})

var _ = Describe("Existing Nodes", func() {
	It("should schedule a pod to an existing node unowned by Karpenter", func() {
		node := test.Node(test.NodeOptions{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("10Gi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Requests: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		}}
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(opts)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		scheduledNode := ExpectScheduled(ctx, env.Client, pod)
		Expect(node.Name).To(Equal(scheduledNode.Name))
	})
	It("should schedule multiple pods to an existing node unowned by Karpenter", func() {
		node := test.Node(test.NodeOptions{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("10"),
				v1.ResourceMemory: resource.MustParse("100Gi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Requests: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		}}
		ExpectApplied(ctx, env.Client, provisioner)
		pods := test.UnschedulablePods(opts, 100)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)

		for _, pod := range pods {
			scheduledNode := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Name).To(Equal(scheduledNode.Name))
		}
	})
	It("should order initialized nodes for scheduling un-initialized nodes", func() {
		ExpectApplied(ctx, env.Client, provisioner)

		var machines []*v1alpha5.Machine
		var nodes []*v1.Node
		for i := 0; i < 100; i++ {
			m := test.Machine(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					},
				},
			})
			ExpectApplied(ctx, env.Client, m)
			m, n := ExpectMachineDeployed(ctx, env.Client, cluster, cloudProvider, m)
			machines = append(machines, m)
			nodes = append(nodes, n)
		}

		// Make one of the nodes and machines initialized
		elem := rand.Intn(100) //nolint:gosec
		ExpectMakeMachinesInitialized(ctx, env.Client, machines[elem])
		ExpectMakeNodesInitialized(ctx, env.Client, nodes[elem])
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machines[elem]))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[elem]))

		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		scheduledNode := ExpectScheduled(ctx, env.Client, pod)

		// Expect that the scheduled node is equal to the ready node since it's initialized
		Expect(scheduledNode.Name).To(Equal(nodes[elem].Name))
	})
	It("should consider a pod incompatible with an existing node but compatible with Provisioner", func() {
		machine, node := test.MachineAndNode(v1alpha5.Machine{
			Status: v1alpha5.MachineStatus{
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("10"),
					v1.ResourceMemory: resource.MustParse("10Gi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, machine, node)
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)
		ExpectMakeNodesInitialized(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machine))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

		pod := test.UnschedulablePod(test.PodOptions{
			NodeRequirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelTopologyZone,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"test-zone-1"},
				},
			},
		})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)

		ExpectApplied(ctx, env.Client, provisioner)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectScheduled(ctx, env.Client, pod)
	})
	Context("Daemonsets", func() {
		It("should not subtract daemonset overhead that is not strictly compatible with an existing node", func() {
			machine, node := test.MachineAndNode(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
						v1.ResourcePods:   resource.MustParse("110"),
					},
				},
			})
			// This DaemonSet is not compatible with the existing Machine/Node
			ds := test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100"),
						v1.ResourceMemory: resource.MustParse("100Gi")},
					},
					NodeRequirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
				}},
			)
			ExpectApplied(ctx, env.Client, provisioner, machine, node, ds)
			ExpectMakeMachinesInitialized(ctx, env.Client, machine)
			ExpectMakeNodesInitialized(ctx, env.Client, node)

			ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machine))
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("1Gi")},
				},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			scheduledNode := ExpectScheduled(ctx, env.Client, pod)
			Expect(scheduledNode.Name).To(Equal(node.Name))

			// Add another pod and expect that pod not to schedule against a Provisioner since we will model the DS against the Provisioner
			// In this case, the DS overhead will take over the entire capacity for every "theoretical node" so we can't schedule a new pod to any new Node
			pod2 := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("1Gi")},
				},
			})
			ExpectApplied(ctx, env.Client, provisioner)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod2)
			ExpectNotScheduled(ctx, env.Client, pod2)
		})
	})
})

var _ = Describe("No Pre-Binding", func() {
	It("should not bind pods to newNodes", func() {
		opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("10m"),
			},
		}}

		var nodeList v1.NodeList
		// shouldn't have any newNodes
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		Expect(nodeList.Items).To(HaveLen(0))

		ExpectApplied(ctx, env.Client, provisioner)
		initialPod := test.UnschedulablePod(opts)
		ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		ExpectNotScheduled(ctx, env.Client, initialPod)

		// should launch a single node
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		Expect(nodeList.Items).To(HaveLen(1))
		node1 := &nodeList.Items[0]

		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		secondPod := test.UnschedulablePod(opts)
		ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
		ExpectNotScheduled(ctx, env.Client, secondPod)
		// shouldn't create a second node as it can bind to the existingNodes node
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		Expect(nodeList.Items).To(HaveLen(1))
	})
	It("should handle resource zeroing of extended resources by kubelet", func() {
		// Issue #1459
		opts := test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:          resource.MustParse("10m"),
				fake.ResourceGPUVendorA: resource.MustParse("1"),
			},
		}}

		var nodeList v1.NodeList
		// shouldn't have any newNodes
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		Expect(nodeList.Items).To(HaveLen(0))

		ExpectApplied(ctx, env.Client, provisioner)
		initialPod := test.UnschedulablePod(opts)
		ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		ExpectNotScheduled(ctx, env.Client, initialPod)

		// should launch a single node
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		Expect(nodeList.Items).To(HaveLen(1))
		node1 := &nodeList.Items[0]

		// simulate kubelet zeroing out the extended resources on the node at startup
		node1.Status.Capacity = map[v1.ResourceName]resource.Quantity{
			fake.ResourceGPUVendorA: resource.MustParse("0"),
		}
		node1.Status.Allocatable = map[v1.ResourceName]resource.Quantity{
			fake.ResourceGPUVendorB: resource.MustParse("0"),
		}

		ExpectApplied(ctx, env.Client, node1)

		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		secondPod := test.UnschedulablePod(opts)
		ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, secondPod)
		ExpectNotScheduled(ctx, env.Client, secondPod)
		// shouldn't create a second node as it can bind to the existingNodes node
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		Expect(nodeList.Items).To(HaveLen(1))
	})
	It("should respect self pod affinity without pod binding (zone)", func() {
		// Issue #1975
		affLabels := map[string]string{"security": "s2"}

		pods := test.UnschedulablePods(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: affLabels,
			},
			PodRequirements: []v1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: affLabels,
				},
				TopologyKey: v1.LabelTopologyZone,
			}},
		}, 2)
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, pods[0])
		var nodeList v1.NodeList
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		for i := range nodeList.Items {
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(&nodeList.Items[i]))
		}
		// the second pod can schedule against the in-flight node, but for that to work we need to be careful
		// in how we fulfill the self-affinity by taking the existing node's domain as a preference over any
		// random viable domain
		ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, pods[1])
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		Expect(nodeList.Items).To(HaveLen(1))
	})
})

var _ = Describe("VolumeUsage", func() {
	BeforeEach(func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			fake.NewInstanceType(
				fake.InstanceTypeOptions{
					Name: "instance-type",
					Resources: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("1024"),
						v1.ResourcePods: resource.MustParse("1024"),
					},
				}),
		}
		provisioner.Spec.Limits = nil
	})
	It("should launch multiple newNodes if required due to volume limits", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		initialPod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node := ExpectScheduled(ctx, env.Client, initialPod)
		csiNode := &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name: node.Name,
			},
			Spec: storagev1.CSINodeSpec{
				Drivers: []storagev1.CSINodeDriver{
					{
						Name:   csiProvider,
						NodeID: "fake-node-id",
						Allocatable: &storagev1.VolumeNodeResources{
							Count: ptr.Int32(10),
						},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, csiNode)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

		sc := test.StorageClass(test.StorageClassOptions{
			ObjectMeta:  metav1.ObjectMeta{Name: "my-storage-class"},
			Provisioner: ptr.String(csiProvider),
			Zones:       []string{"test-zone-1"}})
		ExpectApplied(ctx, env.Client, sc)

		var pods []*v1.Pod
		for i := 0; i < 6; i++ {
			pvcA := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
				StorageClassName: ptr.String("my-storage-class"),
				ObjectMeta:       metav1.ObjectMeta{Name: fmt.Sprintf("my-claim-a-%d", i)},
			})
			pvcB := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
				StorageClassName: ptr.String("my-storage-class"),
				ObjectMeta:       metav1.ObjectMeta{Name: fmt.Sprintf("my-claim-b-%d", i)},
			})
			ExpectApplied(ctx, env.Client, pvcA, pvcB)
			pods = append(pods, test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{pvcA.Name, pvcB.Name},
			}))
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		var nodeList v1.NodeList
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		// we need to create a new node as the in-flight one can only contain 5 pods due to the CSINode volume limit
		Expect(nodeList.Items).To(HaveLen(2))
	})
	It("should launch a single node if all pods use the same PVC", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		initialPod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node := ExpectScheduled(ctx, env.Client, initialPod)
		csiNode := &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name: node.Name,
			},
			Spec: storagev1.CSINodeSpec{
				Drivers: []storagev1.CSINodeDriver{
					{
						Name:   csiProvider,
						NodeID: "fake-node-id",
						Allocatable: &storagev1.VolumeNodeResources{
							Count: ptr.Int32(10),
						},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, csiNode)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

		sc := test.StorageClass(test.StorageClassOptions{
			ObjectMeta:  metav1.ObjectMeta{Name: "my-storage-class"},
			Provisioner: ptr.String(csiProvider),
			Zones:       []string{"test-zone-1"}})
		ExpectApplied(ctx, env.Client, sc)

		pv := test.PersistentVolume(test.PersistentVolumeOptions{
			ObjectMeta: metav1.ObjectMeta{Name: "my-volume"},
			Zones:      []string{"test-zone-1"}})

		pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
			ObjectMeta:       metav1.ObjectMeta{Name: "my-claim"},
			StorageClassName: ptr.String("my-storage-class"),
			VolumeName:       pv.Name,
		})
		ExpectApplied(ctx, env.Client, pv, pvc)

		var pods []*v1.Pod
		for i := 0; i < 100; i++ {
			pods = append(pods, test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{pvc.Name},
			}))
		}
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		var nodeList v1.NodeList
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		// 100 of the same PVC should all be schedulable on the same node
		Expect(nodeList.Items).To(HaveLen(1))
	})
	It("should not fail for NFS volumes", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		initialPod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node := ExpectScheduled(ctx, env.Client, initialPod)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

		pv := test.PersistentVolume(test.PersistentVolumeOptions{
			ObjectMeta:       metav1.ObjectMeta{Name: "my-volume"},
			StorageClassName: "nfs",
			Zones:            []string{"test-zone-1"}})
		pv.Spec.NFS = &v1.NFSVolumeSource{
			Server: "fake.server",
			Path:   "/some/path",
		}
		pv.Spec.CSI = nil

		pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
			ObjectMeta:       metav1.ObjectMeta{Name: "my-claim"},
			VolumeName:       pv.Name,
			StorageClassName: ptr.String(""),
		})
		ExpectApplied(ctx, env.Client, pv, pvc)

		var pods []*v1.Pod
		for i := 0; i < 5; i++ {
			pods = append(pods, test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{pvc.Name, pvc.Name},
			}))
		}
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)

		var nodeList v1.NodeList
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		// 5 of the same PVC should all be schedulable on the same node
		Expect(nodeList.Items).To(HaveLen(1))
	})
	It("should launch nodes for pods with ephemeral volume using the specified storage class name", func() {
		// Launch an initial pod onto a node and register the CSI NodeClaim with a volume count limit of 1
		sc := test.StorageClass(test.StorageClassOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: "my-storage-class",
			},
			Provisioner: ptr.String(csiProvider),
			Zones:       []string{"test-zone-1"}})
		// Create another default storage class that shouldn't be used and has no associated limits
		sc2 := test.StorageClass(test.StorageClassOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: "default-storage-class",
				Annotations: map[string]string{
					pscheduling.IsDefaultStorageClassAnnotation: "true",
				},
			},
			Provisioner: ptr.String("other-provider"),
			Zones:       []string{"test-zone-1"}})

		initialPod := test.UnschedulablePod(test.PodOptions{})
		// Pod has an ephemeral volume claim that has a specified storage class, so it should use the one specified
		initialPod.Spec.Volumes = append(initialPod.Spec.Volumes, v1.Volume{
			Name: "tmp-ephemeral",
			VolumeSource: v1.VolumeSource{
				Ephemeral: &v1.EphemeralVolumeSource{
					VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{
						Spec: v1.PersistentVolumeClaimSpec{
							StorageClassName: lo.ToPtr(sc.Name),
							AccessModes: []v1.PersistentVolumeAccessMode{
								v1.ReadWriteOnce,
							},
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, sc, sc2, initialPod)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node := ExpectScheduled(ctx, env.Client, initialPod)
		csiNode := &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name: node.Name,
			},
			Spec: storagev1.CSINodeSpec{
				Drivers: []storagev1.CSINodeDriver{
					{
						Name:   csiProvider,
						NodeID: "fake-node-id",
						Allocatable: &storagev1.VolumeNodeResources{
							Count: ptr.Int32(1),
						},
					},
					{
						Name:   "other-provider",
						NodeID: "fake-node-id",
						Allocatable: &storagev1.VolumeNodeResources{
							Count: ptr.Int32(10),
						},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, csiNode)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

		pod := test.UnschedulablePod(test.PodOptions{})
		// Pod has an ephemeral volume claim that has a specified storage class, so it should use the one specified
		pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
			Name: "tmp-ephemeral",
			VolumeSource: v1.VolumeSource{
				Ephemeral: &v1.EphemeralVolumeSource{
					VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{
						Spec: v1.PersistentVolumeClaimSpec{
							StorageClassName: lo.ToPtr(sc.Name),
							AccessModes: []v1.PersistentVolumeAccessMode{
								v1.ReadWriteOnce,
							},
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, pod)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node2 := ExpectScheduled(ctx, env.Client, pod)
		Expect(node.Name).ToNot(Equal(node2.Name))
	})
	It("should launch nodes for pods with ephemeral volume using a default storage class", func() {
		// Launch an initial pod onto a node and register the CSI NodeClaim with a volume count limit of 1
		sc := test.StorageClass(test.StorageClassOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: "default-storage-class",
				Annotations: map[string]string{
					pscheduling.IsDefaultStorageClassAnnotation: "true",
				},
			},
			Provisioner: ptr.String(csiProvider),
			Zones:       []string{"test-zone-1"}})

		initialPod := test.UnschedulablePod(test.PodOptions{})
		// Pod has an ephemeral volume claim that has NO storage class, so it should use the default one
		initialPod.Spec.Volumes = append(initialPod.Spec.Volumes, v1.Volume{
			Name: "tmp-ephemeral",
			VolumeSource: v1.VolumeSource{
				Ephemeral: &v1.EphemeralVolumeSource{
					VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{
						Spec: v1.PersistentVolumeClaimSpec{
							AccessModes: []v1.PersistentVolumeAccessMode{
								v1.ReadWriteOnce,
							},
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, sc, initialPod)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node := ExpectScheduled(ctx, env.Client, initialPod)
		csiNode := &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name: node.Name,
			},
			Spec: storagev1.CSINodeSpec{
				Drivers: []storagev1.CSINodeDriver{
					{
						Name:   csiProvider,
						NodeID: "fake-node-id",
						Allocatable: &storagev1.VolumeNodeResources{
							Count: ptr.Int32(1),
						},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, csiNode)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

		pod := test.UnschedulablePod(test.PodOptions{})
		// Pod has an ephemeral volume claim that has NO storage class, so it should use the default one
		pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
			Name: "tmp-ephemeral",
			VolumeSource: v1.VolumeSource{
				Ephemeral: &v1.EphemeralVolumeSource{
					VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{
						Spec: v1.PersistentVolumeClaimSpec{
							AccessModes: []v1.PersistentVolumeAccessMode{
								v1.ReadWriteOnce,
							},
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		})

		ExpectApplied(ctx, env.Client, sc, provisioner, pod)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node2 := ExpectScheduled(ctx, env.Client, pod)
		Expect(node.Name).ToNot(Equal(node2.Name))
	})
	It("should launch nodes for pods with ephemeral volume using the newest storage class", func() {
		if env.Version.Minor() < 26 {
			Skip("Multiple default storage classes is only available in K8s >= 1.26.x")
		}
		// Launch an initial pod onto a node and register the CSI Node with a volume count limit of 1
		sc := test.StorageClass(test.StorageClassOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: "default-storage-class",
				Annotations: map[string]string{
					pscheduling.IsDefaultStorageClassAnnotation: "true",
				},
			},
			Provisioner: ptr.String("other-provider"),
			Zones:       []string{"test-zone-1"}})
		sc2 := test.StorageClass(test.StorageClassOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: "newer-default-storage-class",
				Annotations: map[string]string{
					pscheduling.IsDefaultStorageClassAnnotation: "true",
				},
			},
			Provisioner: ptr.String(csiProvider),
			Zones:       []string{"test-zone-1"}})

		ExpectApplied(ctx, env.Client, sc)
		// Wait a few seconds to apply the second storage class to get a newer creationTimestamp
		time.Sleep(time.Second * 2)
		ExpectApplied(ctx, env.Client, sc2)

		initialPod := test.UnschedulablePod(test.PodOptions{})
		// Pod has an ephemeral volume claim that has NO storage class, so it should use the default one
		initialPod.Spec.Volumes = append(initialPod.Spec.Volumes, v1.Volume{
			Name: "tmp-ephemeral",
			VolumeSource: v1.VolumeSource{
				Ephemeral: &v1.EphemeralVolumeSource{
					VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{
						Spec: v1.PersistentVolumeClaimSpec{
							AccessModes: []v1.PersistentVolumeAccessMode{
								v1.ReadWriteOnce,
							},
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, sc, initialPod)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node := ExpectScheduled(ctx, env.Client, initialPod)
		csiNode := &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name: node.Name,
			},
			Spec: storagev1.CSINodeSpec{
				Drivers: []storagev1.CSINodeDriver{
					{
						Name:   csiProvider,
						NodeID: "fake-node-id",
						Allocatable: &storagev1.VolumeNodeResources{
							Count: ptr.Int32(1),
						},
					},
					{
						Name:   "other-provider",
						NodeID: "fake-node-id",
						Allocatable: &storagev1.VolumeNodeResources{
							Count: ptr.Int32(10),
						},
					},
				},
			},
		}
		ExpectApplied(ctx, env.Client, csiNode)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

		pod := test.UnschedulablePod(test.PodOptions{})
		// Pod has an ephemeral volume claim that has NO storage class, so it should use the default one
		pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
			Name: "tmp-ephemeral",
			VolumeSource: v1.VolumeSource{
				Ephemeral: &v1.EphemeralVolumeSource{
					VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{
						Spec: v1.PersistentVolumeClaimSpec{
							AccessModes: []v1.PersistentVolumeAccessMode{
								v1.ReadWriteOnce,
							},
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, sc, provisioner, pod)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node2 := ExpectScheduled(ctx, env.Client, pod)
		Expect(node.Name).ToNot(Equal(node2.Name))
	})
	It("should not launch nodes for pods with ephemeral volume using a non-existent storage classes", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod(test.PodOptions{})
		pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
			Name: "tmp-ephemeral",
			VolumeSource: v1.VolumeSource{
				Ephemeral: &v1.EphemeralVolumeSource{
					VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{
						Spec: v1.PersistentVolumeClaimSpec{
							StorageClassName: ptr.String("non-existent"),
							AccessModes: []v1.PersistentVolumeAccessMode{
								v1.ReadWriteOnce,
							},
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

		var nodeList v1.NodeList
		Expect(env.Client.List(ctx, &nodeList)).To(Succeed())
		// no nodes should be created as the storage class doesn't eixst
		Expect(nodeList.Items).To(HaveLen(0))
	})
	Context("CSIMigration", func() {
		It("should launch nodes for pods with non-dynamic PVC using a migrated PVC/PV", func() {
			// We should assume that this PVC/PV is using CSI driver implicitly to limit pod scheduling
			// Launch an initial pod onto a node and register the CSI Node with a volume count limit of 1
			sc := test.StorageClass(test.StorageClassOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name: "in-tree-storage-class",
					Annotations: map[string]string{
						pscheduling.IsDefaultStorageClassAnnotation: "true",
					},
				},
				Provisioner: ptr.String(plugins.AWSEBSInTreePluginName),
				Zones:       []string{"test-zone-1"}})
			pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
				StorageClassName: ptr.String(sc.Name),
			})
			ExpectApplied(ctx, env.Client, provisioner, sc, pvc)
			initialPod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{pvc.Name},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
			node := ExpectScheduled(ctx, env.Client, initialPod)
			csiNode := &storagev1.CSINode{
				ObjectMeta: metav1.ObjectMeta{
					Name: node.Name,
				},
				Spec: storagev1.CSINodeSpec{
					Drivers: []storagev1.CSINodeDriver{
						{
							Name:   plugins.AWSEBSDriverName,
							NodeID: "fake-node-id",
							Allocatable: &storagev1.VolumeNodeResources{
								Count: ptr.Int32(1),
							},
						},
					},
				},
			}
			pv := test.PersistentVolume(test.PersistentVolumeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-volume",
				},
				Zones:              []string{"test-zone-1"},
				UseAWSInTreeDriver: true,
			})
			ExpectApplied(ctx, env.Client, csiNode, pvc, pv)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			pvc2 := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
				StorageClassName: ptr.String(sc.Name),
			})
			pod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{pvc2.Name},
			})
			ExpectApplied(ctx, env.Client, pvc2, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node2 := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Name).ToNot(Equal(node2.Name))
		})
		It("should launch nodes for pods with ephemeral volume using a migrated PVC/PV", func() {
			// We should assume that this PVC/PV is using CSI driver implicitly to limit pod scheduling
			// Launch an initial pod onto a node and register the CSI Node with a volume count limit of 1
			sc := test.StorageClass(test.StorageClassOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name: "in-tree-storage-class",
					Annotations: map[string]string{
						pscheduling.IsDefaultStorageClassAnnotation: "true",
					},
				},
				Provisioner: ptr.String(plugins.AWSEBSInTreePluginName),
				Zones:       []string{"test-zone-1"}})

			initialPod := test.UnschedulablePod(test.PodOptions{})
			// Pod has an ephemeral volume claim that references the in-tree storage provider
			initialPod.Spec.Volumes = append(initialPod.Spec.Volumes, v1.Volume{
				Name: "tmp-ephemeral",
				VolumeSource: v1.VolumeSource{
					Ephemeral: &v1.EphemeralVolumeSource{
						VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{
							Spec: v1.PersistentVolumeClaimSpec{
								AccessModes: []v1.PersistentVolumeAccessMode{
									v1.ReadWriteOnce,
								},
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceStorage: resource.MustParse("1Gi"),
									},
								},
								StorageClassName: ptr.String(sc.Name),
							},
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, provisioner, sc, initialPod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
			node := ExpectScheduled(ctx, env.Client, initialPod)
			csiNode := &storagev1.CSINode{
				ObjectMeta: metav1.ObjectMeta{
					Name: node.Name,
				},
				Spec: storagev1.CSINodeSpec{
					Drivers: []storagev1.CSINodeDriver{
						{
							Name:   plugins.AWSEBSDriverName,
							NodeID: "fake-node-id",
							Allocatable: &storagev1.VolumeNodeResources{
								Count: ptr.Int32(1),
							},
						},
					},
				},
			}
			ExpectApplied(ctx, env.Client, csiNode)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

			pod := test.UnschedulablePod(test.PodOptions{})
			// Pod has an ephemeral volume claim that reference the in-tree storage provider
			pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
				Name: "tmp-ephemeral",
				VolumeSource: v1.VolumeSource{
					Ephemeral: &v1.EphemeralVolumeSource{
						VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{
							Spec: v1.PersistentVolumeClaimSpec{
								AccessModes: []v1.PersistentVolumeAccessMode{
									v1.ReadWriteOnce,
								},
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceStorage: resource.MustParse("1Gi"),
									},
								},
								StorageClassName: ptr.String(sc.Name),
							},
						},
					},
				},
			})
			// Pod should not schedule to the first node since we should realize that we have hit our volume limits
			ExpectApplied(ctx, env.Client, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node2 := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Name).ToNot(Equal(node2.Name))
		})
	})
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
