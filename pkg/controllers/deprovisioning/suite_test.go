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

package deprovisioning_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var deprovisioningController *deprovisioning.Controller
var provisioner *provisioning.Provisioner
var cloudProvider *fake.CloudProvider
var nodeStateController controller.Controller
var machineStateController controller.Controller
var fakeClock *clock.FakeClock
var fakeRecorder *record.FakeRecorder
var onDemandInstances []*cloudprovider.InstanceType
var mostExpensiveInstance *cloudprovider.InstanceType
var mostExpensiveOffering cloudprovider.Offering
var leastExpensiveInstance *cloudprovider.InstanceType
var leastExpensiveOffering cloudprovider.Offering

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Deprovisioning")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: true}))
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	fakeRecorder = record.NewFakeRecorder(500)
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeStateController = informer.NewNodeController(env.Client, cluster)
	machineStateController = informer.NewMachineController(env.Client, cluster)
	provisioner = provisioning.NewProvisioner(env.Client, env.KubernetesInterface.CoreV1(), events.NewRecorder(fakeRecorder), cloudProvider, cluster)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	cloudProvider.CreateCalls = nil
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
	cloudProvider.AllowedCreateCalls = math.MaxInt
	fakeRecorder.Events = make(chan string, 500) // clear the events
	onDemandInstances = lo.Filter(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
		for _, o := range i.Offerings.Available() {
			if o.CapacityType == v1alpha5.CapacityTypeOnDemand {
				return true
			}
		}
		return false
	})
	// Sort the instances by pricing from low to high
	sort.Slice(onDemandInstances, func(i, j int) bool {
		return cheapestOffering(onDemandInstances[i].Offerings).Price < cheapestOffering(onDemandInstances[j].Offerings).Price
	})
	leastExpensiveInstance = onDemandInstances[0]
	leastExpensiveOffering = leastExpensiveInstance.Offerings[0]
	mostExpensiveInstance = onDemandInstances[len(onDemandInstances)-1]
	mostExpensiveOffering = mostExpensiveInstance.Offerings[0]

	// ensure any waiters on our clock are allowed to proceed before resetting our clock time
	for fakeClock.HasWaiters() {
		fakeClock.Step(1 * time.Minute)
	}
	fakeClock.SetTime(time.Now())
	cluster.SetConsolidated(false)
	deprovisioningController = deprovisioning.NewController(fakeClock, env.Client, provisioner, cloudProvider, events.NewRecorder(fakeRecorder), cluster)

	// Reset Feature Flags to test defaults
	ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: true}))
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cluster.Reset()
})

var _ = Describe("Consolidation State", func() {
	It("should not reset consolidation state if consolidation hasn't run", func() {
		// this assumes that the consolidation reset period is 5 minutes, which it is currently
		_, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
		Expect(err).ToNot(HaveOccurred())
		Expect(cluster.Consolidated()).To(BeTrue())
		fakeClock.Step(1 * time.Minute)
		Expect(cluster.Consolidated()).To(BeTrue())

		// reconciling now shouldn't set the last consolidated time to current time, as consolidation isn't actually
		// running since it last ran 1 minute ago
		_, err = deprovisioningController.Reconcile(ctx, reconcile.Request{})
		Expect(err).ToNot(HaveOccurred())
		// but advancing the clock 4:30, so we are at 5:30 past the last run time should cause consolidated to return
		// false
		fakeClock.Step(4*time.Minute + 30*time.Second)
		Expect(cluster.Consolidated()).To(BeFalse())
	})
})

var _ = Describe("Pod Eviction Cost", func() {
	const standardPodCost = 1.0
	It("should have a standard disruptionCost for a pod with no priority or disruptionCost specified", func() {
		cost := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{})
		Expect(cost).To(BeNumerically("==", standardPodCost))
	})
	It("should have a higher disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "-100",
			}},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
	It("should have higher costs for higher deletion costs", func() {
		cost1 := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "101",
			}},
		})
		cost2 := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		cost3 := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "99",
			}},
		})
		Expect(cost1).To(BeNumerically(">", cost2))
		Expect(cost2).To(BeNumerically(">", cost3))
	})
	It("should have a higher disruptionCost for a pod with a higher priority", func() {
		cost := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: ptr.Int32(1)},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a lower priority", func() {
		cost := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: ptr.Int32(-1)},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
})

var _ = Describe("Replace Nodes", func() {
	It("can replace node", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)},
		})
		machine, node := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
			},
		})
		ExpectApplied(ctx, env.Client, rs, pod, node, machine, prov)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		fakeClock.Step(10 * time.Minute)

		// consolidation won't delete the old machine until the new machine is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)

		// should create a new machine as there is a cheaper one that can hold the pod
		machines := ExpectMachines(ctx, env.Client)
		nodes := ExpectNodes(ctx, env.Client)
		Expect(machines).To(HaveLen(1))
		Expect(nodes).To(HaveLen(1))

		// Expect that the new machine does not request the most expensive instance type
		Expect(machines[0].Name).ToNot(Equal(machine.Name))
		Expect(scheduling.NewNodeSelectorRequirements(machines[0].Spec.Requirements...).Has(v1.LabelInstanceTypeStable)).To(BeTrue())
		Expect(scheduling.NewNodeSelectorRequirements(machines[0].Spec.Requirements...).Get(v1.LabelInstanceTypeStable).Has(mostExpensiveInstance.Name)).To(BeFalse())

		// and delete the old one
		ExpectNotFound(ctx, env.Client, machine, node)
	})
	It("can replace nodes, considers PDB", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		pdb := test.PodDisruptionBudget(test.PDBOptions{
			Labels:         labels,
			MaxUnavailable: fromInt(0),
			Status: &policyv1.PodDisruptionBudgetStatus{
				ObservedGeneration: 1,
				DisruptionsAllowed: 0,
				CurrentHealthy:     1,
				DesiredHealthy:     1,
				ExpectedPods:       1,
			},
		})

		prov := test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)},
		})
		machine, node := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], machine, node, prov, pdb)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pods[0], node)
		ExpectManualBinding(ctx, env.Client, pods[1], node)
		ExpectManualBinding(ctx, env.Client, pods[2], node)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		fakeClock.Step(10 * time.Minute)

		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		Expect(cluster.Consolidated()).To(BeTrue())

		// we didn't create a new machine or delete the old one
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, machine)
	})
	It("can replace nodes, PDB namespace must match", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)},
		})
		machine, node := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
			},
		})
		namespace := test.Namespace()
		pdb := test.PodDisruptionBudget(test.PDBOptions{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace.ObjectMeta.Name,
			},
			Labels:         labels,
			MaxUnavailable: fromInt(0),
			Status: &policyv1.PodDisruptionBudgetStatus{
				ObservedGeneration: 1,
				DisruptionsAllowed: 0,
				CurrentHealthy:     1,
				DesiredHealthy:     1,
				ExpectedPods:       1,
			},
		})

		// bind pods to node
		ExpectApplied(ctx, env.Client, rs, pod, machine, node, prov, namespace, pdb)
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		fakeClock.Step(10 * time.Minute)

		// consolidation won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)

		// should create a new machine as there is a cheaper one that can hold the pod
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectNotFound(ctx, env.Client, machine, node)
	})
	It("can replace nodes, considers do-not-consolidate annotation", func() {
		labels := map[string]string{
			"app": "test",
		}

		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)},
		})
		regularMachine, regularNode := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		annotatedMachine, annotatedNode := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1alpha5.DoNotConsolidateNodeAnnotationKey: "true",
				},
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], prov)
		ExpectApplied(ctx, env.Client, regularMachine, regularNode, annotatedMachine, annotatedNode)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pods[0], regularNode)
		ExpectManualBinding(ctx, env.Client, pods[1], regularNode)
		ExpectManualBinding(ctx, env.Client, pods[2], annotatedNode)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{regularNode, annotatedNode}, []*v1alpha5.Machine{regularMachine, annotatedMachine})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, regularMachine)

		// we should delete the non-annotated node
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectNotFound(ctx, env.Client, regularMachine, regularNode)
	})
	It("won't replace node if any spot replacement is more expensive", func() {
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        0.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "potential-spot-replacement",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeSpot,
					Zone:         "test-zone-1a",
					Price:        1.0,
					Available:    true,
				},
				{
					CapacityType: v1alpha5.CapacityTypeSpot,
					Zone:         "test-zone-1b",
					Price:        0.2,
					Available:    true,
				},
				{
					CapacityType: v1alpha5.CapacityTypeSpot,
					Zone:         "test-zone-1c",
					Price:        0.4,
					Available:    true,
				},
			},
		})
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			currentInstance,
			replacementInstance,
		}

		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)},
		})
		machine, node := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       currentInstance.Name,
					v1alpha5.LabelCapacityType:       currentInstance.Offerings[0].CapacityType,
					v1.LabelTopologyZone:             currentInstance.Offerings[0].Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
			},
		})

		ExpectApplied(ctx, env.Client, rs, pod, machine, node, prov)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		fakeClock.Step(10 * time.Minute)
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		Expect(cluster.Consolidated()).To(BeTrue())

		// Expect to not create or delete more machines
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, machine)
	})
	It("won't replace on-demand node if on-demand replacement is more expensive", func() {
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        0.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "on-demand-replacement",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        0.6,
					Available:    true,
				},
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1b",
					Price:        0.6,
					Available:    true,
				},
				{
					CapacityType: v1alpha5.CapacityTypeSpot,
					Zone:         "test-zone-1b",
					Price:        0.2,
					Available:    true,
				},
				{
					CapacityType: v1alpha5.CapacityTypeSpot,
					Zone:         "test-zone-1c",
					Price:        0.3,
					Available:    true,
				},
			},
		})

		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			currentInstance,
			replacementInstance,
		}

		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		// provisioner should require on-demand instance for this test case
		prov := test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)},
			Requirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1alpha5.LabelCapacityType,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{v1alpha5.CapacityTypeOnDemand},
				},
			},
		})
		machine, node := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       currentInstance.Name,
					v1alpha5.LabelCapacityType:       currentInstance.Offerings[0].CapacityType,
					v1.LabelTopologyZone:             currentInstance.Offerings[0].Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
			},
		})

		ExpectApplied(ctx, env.Client, rs, pod, machine, node, prov)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		fakeClock.Step(10 * time.Minute)
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		Expect(cluster.Consolidated()).To(BeTrue())

		// Expect to not create or delete more machines
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, machine)
	})
	It("waits for node deletion to finish", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)},
		})
		machine, node := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{"unit-test.com/block-deletion"},
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
			},
		})

		ExpectApplied(ctx, env.Client, rs, pod, machine, node, prov)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		fakeClock.Step(10 * time.Minute)

		// consolidation won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)

		var consolidationFinished atomic.Bool
		go func() {
			defer GinkgoRecover()
			ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
			Expect(cluster.Consolidated()).To(BeFalse())
			consolidationFinished.Store(true)
		}()
		wg.Wait()

		// machine should still exist
		ExpectExists(ctx, env.Client, machine)
		// and consolidation should still be running waiting on the machine's deletion
		Expect(consolidationFinished.Load()).To(BeFalse())

		// fetch the latest machine object and remove the finalizer
		machine = ExpectExists(ctx, env.Client, machine)
		ExpectFinalizersRemoved(ctx, env.Client, machine)

		// consolidation should complete now that the finalizer on the machine is gone and it can
		// was actually deleted
		Eventually(consolidationFinished.Load, 10*time.Second).Should(BeTrue())
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)

		ExpectNotFound(ctx, env.Client, machine, node)

		// Expect that the new machine was created and its different than the original
		machines := ExpectMachines(ctx, env.Client)
		nodes := ExpectNodes(ctx, env.Client)
		Expect(machines).To(HaveLen(1))
		Expect(nodes).To(HaveLen(1))
		Expect(machines[0].Name).ToNot(Equal(machine.Name))
		Expect(nodes[0].Name).ToNot(Equal(node.Name))
	})
})

var _ = Describe("Delete Node", func() {
	var prov *v1alpha5.Provisioner
	var machine1, machine2 *v1alpha5.Machine
	var node1, node2 *v1.Node

	BeforeEach(func() {
		prov = test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)},
		})
		machine1, node1 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       leastExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       leastExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             leastExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		machine2, node2 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       leastExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       leastExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             leastExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
	})
	It("can delete nodes", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})
		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], machine1, node1, machine2, node2, prov)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		ExpectManualBinding(ctx, env.Client, pods[2], node2)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine2)

		// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		// and delete the old one
		ExpectNotFound(ctx, env.Client, machine2, node2)
	})
	It("can delete nodes, considers PDB", func() {
		var nl v1.NodeList
		Expect(env.Client.List(ctx, &nl)).To(Succeed())
		Expect(nl.Items).To(HaveLen(0))
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		// only pod[2] is covered by the PDB
		pods[2].Labels = labels
		pdb := test.PodDisruptionBudget(test.PDBOptions{
			Labels:         labels,
			MaxUnavailable: fromInt(0),
			Status: &policyv1.PodDisruptionBudgetStatus{
				ObservedGeneration: 1,
				DisruptionsAllowed: 0,
				CurrentHealthy:     1,
				DesiredHealthy:     1,
				ExpectedPods:       1,
			},
		})
		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], machine1, node1, machine2, node2, prov, pdb)

		// two pods on node 1
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		// one on node 2, but it has a PDB with zero disruptions allowed
		ExpectManualBinding(ctx, env.Client, pods[2], node2)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)

		// we don't need a new node
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		// but we expect to delete the machine with more pods (node1) as the pod on machine2 has a PDB preventing
		// eviction
		ExpectNotFound(ctx, env.Client, machine1, node1)
	})
	It("can delete nodes, considers do-not-evict", func() {
		// create our RS, so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		// only pod[2] has a do not evict annotation
		pods[2].Annotations = map[string]string{
			v1alpha5.DoNotEvictPodAnnotationKey: "true",
		}
		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], machine1, node1, machine2, node2, prov)

		// two pods on node 1
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		// one on node 2, but it has a do-not-evict annotation
		ExpectManualBinding(ctx, env.Client, pods[2], node2)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)

		// we don't need a new node
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		// but we expect to delete the machine with more pods (machine1) as the pod on machine2 has a do-not-evict annotation
		ExpectNotFound(ctx, env.Client, machine1, node1)
	})
	It("can delete nodes, evicts pods without an ownerRef", func() {
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		// pod[2] is a stand-alone (non ReplicaSet) pod
		pods[2].OwnerReferences = nil
		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], machine1, node1, machine2, node2, prov)

		// two pods on node 1
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		// one on node 2, but it's a standalone pod
		ExpectManualBinding(ctx, env.Client, pods[2], node2)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine2)

		// we don't need a new node
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		// but we expect to delete the machine with the fewest pods (machine 2) even though the pod has no ownerRefs
		// and will not be recreated
		ExpectNotFound(ctx, env.Client, machine2, node2)
	})
	It("won't delete node if it would require pods to schedule on an un-initialized node", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})
		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], machine1, node1, machine2, node2, prov)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		ExpectManualBinding(ctx, env.Client, pods[2], node2)

		// inform cluster state about nodes and machines, intentionally leaving node1 as not ready
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node2}, []*v1alpha5.Machine{machine1, machine2})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// shouldn't delete the node
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		Expect(fakeRecorder.Events).To(HaveLen(2))
		event := <-fakeRecorder.Events
		Expect(strings.Contains(event, "not all pods would schedule")).To(BeTrue())
		Expect(strings.Contains(event, "would schedule against a non-initialized node")).To(BeTrue())
	})
})

var _ = Describe("Node Lifetime Consideration", func() {
	var prov *v1alpha5.Provisioner
	var machine1, machine2 *v1alpha5.Machine
	var node1, node2 *v1.Node

	BeforeEach(func() {
		prov = test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{
				Enabled: ptr.Bool(true),
			},
			TTLSecondsUntilExpired: ptr.Int64(3),
		})
		machine1, node1 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       leastExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       leastExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             leastExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		machine2, node2 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       leastExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       leastExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             leastExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
	})
	It("should consider node lifetime remaining when calculating disruption cost", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], prov)
		ExpectApplied(ctx, env.Client, machine1, node1) // ensure node1 is the oldest node
		time.Sleep(2 * time.Second)                     // this sleep is unfortunate, but necessary.  The creation time is from etcd, and we can't mock it, so we
		// need to sleep to force the second node to be created a bit after the first node.
		ExpectApplied(ctx, env.Client, machine2, node2)

		// two pods on node 1, one on node 2
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		ExpectManualBinding(ctx, env.Client, pods[2], node2)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})

		fakeClock.SetTime(time.Now())

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)

		// the second node has more pods, so it would normally not be picked for consolidation, except it very little
		// lifetime remaining, so it should be deleted
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectNotFound(ctx, env.Client, machine1, node1)
	})
})

var _ = Describe("Topology Consideration", func() {
	var prov *v1alpha5.Provisioner
	var zone1Machine, zone2Machine, zone3Machine *v1alpha5.Machine
	var zone1Node, zone2Node, zone3Node *v1.Node
	var oldMachineNames sets.String

	BeforeEach(func() {
		testZone1Instance := leastExpensiveInstanceWithZone("test-zone-1")
		testZone2Instance := mostExpensiveInstanceWithZone("test-zone-2")
		testZone3Instance := leastExpensiveInstanceWithZone("test-zone-3")

		prov = test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)},
		})
		zone1Machine, zone1Node = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelTopologyZone:             "test-zone-1",
					v1.LabelInstanceTypeStable:       testZone1Instance.Name,
					v1alpha5.LabelCapacityType:       testZone1Instance.Offerings[0].CapacityType,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")},
			},
		})
		zone2Machine, zone2Node = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelTopologyZone:             "test-zone-2",
					v1.LabelInstanceTypeStable:       testZone2Instance.Name,
					v1alpha5.LabelCapacityType:       testZone2Instance.Offerings[0].CapacityType,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")},
			},
		})
		zone3Machine, zone3Node = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelTopologyZone:             "test-zone-3",
					v1.LabelInstanceTypeStable:       testZone3Instance.Name,
					v1alpha5.LabelCapacityType:       testZone1Instance.Offerings[0].CapacityType,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")},
			},
		})
		oldMachineNames = sets.NewString(zone1Machine.Name, zone2Machine.Name, zone3Machine.Name)
	})
	It("can replace node maintaining zonal topology spread", func() {
		labels := map[string]string{
			"app": "test-zonal-spread",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

		tsc := v1.TopologySpreadConstraint{
			MaxSkew:           1,
			TopologyKey:       v1.LabelTopologyZone,
			WhenUnsatisfiable: v1.DoNotSchedule,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
		}
		pods := test.Pods(4, test.PodOptions{
			ResourceRequirements:      v1.ResourceRequirements{Requests: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}},
			TopologySpreadConstraints: []v1.TopologySpreadConstraint{tsc},
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], zone1Machine, zone1Node, zone2Machine, zone2Node, zone3Machine, zone3Node, prov)

		// bind pods to nodes
		ExpectManualBinding(ctx, env.Client, pods[0], zone1Node)
		ExpectManualBinding(ctx, env.Client, pods[1], zone2Node)
		ExpectManualBinding(ctx, env.Client, pods[2], zone3Node)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{zone1Node, zone2Node, zone3Node}, []*v1alpha5.Machine{zone1Machine, zone2Machine, zone3Machine})

		ExpectSkew(ctx, env.Client, "default", &tsc).To(ConsistOf(1, 1, 1))

		fakeClock.Step(10 * time.Minute)

		// consolidation won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, zone2Machine)

		// should create a new node as there is a cheaper one that can hold the pod
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(3))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(3))
		ExpectNotFound(ctx, env.Client, zone2Machine, zone2Node)

		// Find the new node associated with the machine
		newMachine, ok := lo.Find(ExpectMachines(ctx, env.Client), func(m *v1alpha5.Machine) bool {
			return !oldMachineNames.Has(m.Name)
		})
		Expect(ok).To(BeTrue())
		newNode, ok := lo.Find(ExpectNodes(ctx, env.Client), func(n *v1.Node) bool {
			return newMachine.Status.ProviderID == n.Spec.ProviderID
		})
		Expect(ok).To(BeTrue())

		// we need to emulate the replicaset controller and bind a new pod to the newly created node
		ExpectApplied(ctx, env.Client, pods[3])
		ExpectManualBinding(ctx, env.Client, pods[3], newNode)

		// we should maintain our skew, the new node must be in the same zone as the old node it replaced
		ExpectSkew(ctx, env.Client, "default", &tsc).To(ConsistOf(1, 1, 1))
	})
	It("won't delete node if it would violate pod anti-affinity", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		pods := test.Pods(3, test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{Requests: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}},
			PodAntiRequirements: []v1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
					TopologyKey:   v1.LabelHostname,
				},
			},
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				},
			},
		})

		// Make the Zone 2 instance also the least expensive instance
		zone2Instance := leastExpensiveInstanceWithZone("test-zone-2")
		zone2Node.Labels = lo.Assign(zone2Node.Labels, map[string]string{
			v1alpha5.ProvisionerNameLabelKey: prov.Name,
			v1.LabelTopologyZone:             "test-zone-2",
			v1.LabelInstanceTypeStable:       zone2Instance.Name,
			v1alpha5.LabelCapacityType:       zone2Instance.Offerings[0].CapacityType,
		})
		zone2Machine.Labels = lo.Assign(zone2Machine.Labels, map[string]string{
			v1alpha5.ProvisionerNameLabelKey: prov.Name,
			v1.LabelTopologyZone:             "test-zone-2",
			v1.LabelInstanceTypeStable:       zone2Instance.Name,
			v1alpha5.LabelCapacityType:       zone2Instance.Offerings[0].CapacityType,
		})
		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], zone1Machine, zone1Node, zone2Machine, zone2Node, zone3Machine, zone3Node, prov)

		// bind pods to nodes
		ExpectManualBinding(ctx, env.Client, pods[0], zone1Node)
		ExpectManualBinding(ctx, env.Client, pods[1], zone2Node)
		ExpectManualBinding(ctx, env.Client, pods[2], zone3Node)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{zone1Node, zone2Node, zone3Node}, []*v1alpha5.Machine{zone1Machine, zone2Machine, zone3Machine})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// our nodes are already the cheapest available, so we can't replace them.  If we delete, it would
		// violate the anti-affinity rule, so we can't do anything.
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(3))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(3))
		ExpectExists(ctx, env.Client, zone1Machine)
		ExpectExists(ctx, env.Client, zone2Machine)
		ExpectExists(ctx, env.Client, zone3Machine)
	})
})

var _ = Describe("Empty Nodes", func() {
	var prov *v1alpha5.Provisioner
	var machine1, machine2 *v1alpha5.Machine
	var node1, node2 *v1.Node

	BeforeEach(func() {
		prov = test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)}})
		machine1, node1 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		machine2, node2 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
	})
	It("can delete empty nodes with consolidation", func() {
		ExpectApplied(ctx, env.Client, machine1, node1, prov)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)

		// we should delete the empty node
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, machine1, node1)
	})
	It("can delete multiple empty nodes with consolidation", func() {
		ExpectApplied(ctx, env.Client, machine1, node1, machine2, node2, prov)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine1, machine2)

		// we should delete the empty nodes
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, machine1)
		ExpectNotFound(ctx, env.Client, machine2)
	})
	It("can delete empty nodes with TTLSecondsAfterEmpty with the emptiness timestamp", func() {
		prov = test.Provisioner(test.ProvisionerOptions{TTLSecondsAfterEmpty: ptr.Int64(10)})

		// Update the machine and node to be "owned" by the new provisioner and the node
		// to be marked as empty
		machine1.Labels = lo.Assign(machine1.Labels, map[string]string{
			v1alpha5.ProvisionerNameLabelKey: prov.Name,
		})
		node1.Labels = lo.Assign(node1.Labels, map[string]string{
			v1alpha5.ProvisionerNameLabelKey: prov.Name,
		})
		node1.Annotations = lo.Assign(node1.Annotations, map[string]string{
			v1alpha5.EmptinessTimestampAnnotationKey: fakeClock.Now().Format(time.RFC3339),
		})
		ExpectApplied(ctx, env.Client, prov, machine1, node1)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)

		// we should delete the empty node
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, machine1, node1)
	})
	It("considers pending pods when consolidating", func() {
		machine1, node1 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("128"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})

		// there is a pending pod that should land on the node
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1"),
				},
			},
		})
		unsched := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("125"),
				},
			},
		})

		ExpectApplied(ctx, env.Client, machine1, node1, pod, unsched, prov)

		// bind one of the pods to the node
		ExpectManualBinding(ctx, env.Client, pod, node1)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})

		fakeClock.Step(10 * time.Minute)
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// we don't need any new nodes and consolidation should notice the huge pending pod that needs the large
		// node to schedule, which prevents the large expensive node from being replaced
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, machine1)
	})
})

var _ = Describe("Consolidation TTL", func() {
	var prov *v1alpha5.Provisioner
	var machine1, machine2 *v1alpha5.Machine
	var node1, node2 *v1.Node

	BeforeEach(func() {
		prov = test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)}})
		machine1, node1 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		machine2, node2 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
	})
	It("should not deprovision nodes that receive blocking pods during the TTL", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				},
			},
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU: resource.MustParse("1"),
				},
			}})
		noEvictPod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				Annotations: map[string]string{v1alpha5.DoNotEvictPodAnnotationKey: "true"},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				},
			},
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU: resource.MustParse("1"),
				},
			}})
		ExpectApplied(ctx, env.Client, machine1, node1, prov, pod, noEvictPod)
		ExpectManualBinding(ctx, env.Client, pod, node1)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})

		var wg sync.WaitGroup
		wg.Add(1)
		finished := atomic.Bool{}
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			defer finished.Store(true)
			_, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
			Expect(err).To(HaveOccurred())
		}()

		// wait for the deprovisioningController to block on the validation timeout
		Eventually(fakeClock.HasWaiters, time.Second*10).Should(BeTrue())
		// controller should be blocking during the timeout
		Expect(finished.Load()).To(BeFalse())
		// and the node should not be deleted yet
		ExpectExists(ctx, env.Client, node1)

		// make the node non-empty by binding it
		ExpectManualBinding(ctx, env.Client, noEvictPod, node1)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

		// advance the clock so that the timeout expires
		fakeClock.Step(31 * time.Second)
		// controller should finish
		Eventually(finished.Load, 10*time.Second).Should(BeTrue())
		wg.Wait()

		// nothing should be removed since the node is no longer empty
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, node1)
	})
	It("should wait for the node TTL for empty nodes before consolidating", func() {
		ExpectApplied(ctx, env.Client, machine1, node1, prov)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})

		var wg sync.WaitGroup
		wg.Add(1)
		finished := atomic.Bool{}
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			defer finished.Store(true)
			ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		}()

		// wait for the controller to block on the validation timeout
		Eventually(fakeClock.HasWaiters, time.Second*10).Should(BeTrue())
		// controller should be blocking during the timeout
		Expect(finished.Load()).To(BeFalse())
		// and the node should not be deleted yet
		ExpectExists(ctx, env.Client, machine1)

		// advance the clock so that the timeout expires
		fakeClock.Step(31 * time.Second)
		// controller should finish
		Eventually(finished.Load, 10*time.Second).Should(BeTrue())
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)

		// machine should be deleted after the TTL due to emptiness
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, machine1, node1)
	})
	It("should wait for the node TTL for non-empty nodes before consolidating", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		// assign the machines to the least expensive offering so only one of them gets deleted
		machine1.Labels = lo.Assign(machine1.Labels, map[string]string{
			v1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
			v1alpha5.LabelCapacityType: leastExpensiveOffering.CapacityType,
			v1.LabelTopologyZone:       leastExpensiveOffering.Zone,
		})
		node1.Labels = lo.Assign(node1.Labels, map[string]string{
			v1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
			v1alpha5.LabelCapacityType: leastExpensiveOffering.CapacityType,
			v1.LabelTopologyZone:       leastExpensiveOffering.Zone,
		})
		machine2.Labels = lo.Assign(machine2.Labels, map[string]string{
			v1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
			v1alpha5.LabelCapacityType: leastExpensiveOffering.CapacityType,
			v1.LabelTopologyZone:       leastExpensiveOffering.Zone,
		})
		node2.Labels = lo.Assign(node2.Labels, map[string]string{
			v1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
			v1alpha5.LabelCapacityType: leastExpensiveOffering.CapacityType,
			v1.LabelTopologyZone:       leastExpensiveOffering.Zone,
		})

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], machine1, node1, machine2, node2, prov)

		// bind pods to nodes
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		ExpectManualBinding(ctx, env.Client, pods[2], node2)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})

		var wg sync.WaitGroup
		wg.Add(1)
		finished := atomic.Bool{}
		go func() {
			defer wg.Done()
			defer finished.Store(true)
			ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		}()

		// wait for the controller to block on the validation timeout
		Eventually(fakeClock.HasWaiters, time.Second*10).Should(BeTrue())
		// controller should be blocking during the timeout
		Expect(finished.Load()).To(BeFalse())
		// and the node should not be deleted yet
		ExpectExists(ctx, env.Client, machine1)
		ExpectExists(ctx, env.Client, machine2)

		// advance the clock so that the timeout expires
		fakeClock.Step(31 * time.Second)
		// controller should finish
		Eventually(finished.Load, 10*time.Second).Should(BeTrue())
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine2)

		// machine should be deleted after the TTL due to emptiness
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectNotFound(ctx, env.Client, machine2, node2)
	})
	It("should not consolidate if the action becomes invalid during the node TTL wait", func() {
		pod := test.Pod()
		ExpectApplied(ctx, env.Client, machine1, node1, prov, pod)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1}, []*v1alpha5.Machine{machine1})

		var wg sync.WaitGroup
		wg.Add(1)
		finished := atomic.Bool{}
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			defer finished.Store(true)
			_, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
			Expect(err).To(HaveOccurred())
		}()

		// wait for the deprovisioningController to block on the validation timeout
		Eventually(fakeClock.HasWaiters, time.Second*10).Should(BeTrue())
		// controller should be blocking during the timeout
		Expect(finished.Load()).To(BeFalse())
		// and the node should not be deleted yet
		ExpectExists(ctx, env.Client, machine1)

		// make the node non-empty by binding it
		ExpectManualBinding(ctx, env.Client, pod, node1)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

		// advance the clock so that the timeout expires
		fakeClock.Step(31 * time.Second)
		// controller should finish
		Eventually(finished.Load, 10*time.Second).Should(BeTrue())
		wg.Wait()

		// nothing should be removed since the node is no longer empty
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, machine1)
	})
})

var _ = Describe("Parallelization", func() {
	var prov *v1alpha5.Provisioner
	var machine *v1alpha5.Machine
	var node *v1.Node

	BeforeEach(func() {
		prov = test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)}})
		machine, node = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
	})
	It("should schedule an additional node when receiving pending pods while consolidating", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				},
			},
		})

		node.Finalizers = []string{"karpenter.sh/test-finalizer"}
		machine.Finalizers = []string{"karpenter.sh/test-finalizer"}

		ExpectApplied(ctx, env.Client, rs, pod, machine, node, prov)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		fakeClock.Step(10 * time.Minute)

		// Run the processing loop in parallel in the background with environment context
		var wg sync.WaitGroup
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectTriggerVerifyAction(&wg)
		go func() {
			defer GinkgoRecover()
			_, _ = deprovisioningController.Reconcile(ctx, reconcile.Request{})
		}()
		wg.Wait()

		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(2))

		// Add a new pending pod that should schedule while node is not yet deleted
		pod = test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, provisioner, pod)
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(2))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		ExpectScheduled(ctx, env.Client, pod)
	})
	It("should not consolidate a node that is launched for pods on a deleting node", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

		prov := test.Provisioner(test.ProvisionerOptions{
			Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)},
		})
		podOpts := test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				},
			},
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU: resource.MustParse("1"),
				},
			},
		}

		var pods []*v1.Pod
		for i := 0; i < 5; i++ {
			pod := test.UnschedulablePod(podOpts)
			pods = append(pods, pod)
		}
		ExpectApplied(ctx, env.Client, rs, prov)
		ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, provisioner, lo.Map(pods, func(p *v1.Pod, _ int) *v1.Pod { return p.DeepCopy() })...)

		machines := ExpectMachines(ctx, env.Client)
		Expect(machines).To(HaveLen(1))
		nodes := ExpectNodes(ctx, env.Client)
		Expect(nodes).To(HaveLen(1))

		// Update cluster state with new node
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[0]))

		// Mark the node for deletion and re-trigger reconciliation
		oldNodeName := nodes[0].Name
		cluster.MarkForDeletion(nodes[0].Name)
		ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, provisioner, lo.Map(pods, func(p *v1.Pod, _ int) *v1.Pod { return p.DeepCopy() })...)

		// Make sure that the cluster state is aware of the current node state
		nodes = ExpectNodes(ctx, env.Client)
		Expect(nodes).To(HaveLen(2))
		newNode, _ := lo.Find(nodes, func(n *v1.Node) bool { return n.Name != oldNodeName })

		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, nodes, nil)

		// Wait for the nomination cache to expire
		time.Sleep(time.Second * 11)

		// Re-create the pods to re-bind them
		for i := 0; i < 2; i++ {
			ExpectDeleted(ctx, env.Client, pods[i])
			pod := test.UnschedulablePod(podOpts)
			ExpectApplied(ctx, env.Client, pod)
			ExpectManualBinding(ctx, env.Client, pod, newNode)
		}

		// Trigger a reconciliation run which should take into account the deleting node
		// consolidation shouldn't trigger additional actions
		fakeClock.Step(10 * time.Minute)

		result, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
	})
})

var _ = Describe("Multi-Node Consolidation", func() {
	var prov *v1alpha5.Provisioner
	var machine1, machine2, machine3 *v1alpha5.Machine
	var node1, node2, node3 *v1.Node

	BeforeEach(func() {
		prov = test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: ptr.Bool(true)}})
		machine1, node1 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		machine2, node2 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		machine3, node3 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
	})
	It("can merge 3 nodes into 1", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], machine1, node1, machine2, node2, machine3, node3, prov)
		ExpectMakeNodesReady(ctx, env.Client, node1, node2, node3)

		// bind pods to nodes
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node2)
		ExpectManualBinding(ctx, env.Client, pods[2], node3)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2, node3}, []*v1alpha5.Machine{machine1, machine2, machine3})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine1, machine2, machine3)

		// three machines should be replaced with a single machine
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectNotFound(ctx, env.Client, machine1, node1, machine2, node2, machine3, node3)
	})
	It("won't merge 2 nodes into 1 of the same type", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		// Make the machines the least expensive instance type and make them of the same type
		machine1.Labels = lo.Assign(machine1.Labels, map[string]string{
			v1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
			v1alpha5.LabelCapacityType: leastExpensiveOffering.CapacityType,
			v1.LabelTopologyZone:       leastExpensiveOffering.Zone,
		})
		node1.Labels = lo.Assign(node1.Labels, map[string]string{
			v1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
			v1alpha5.LabelCapacityType: leastExpensiveOffering.CapacityType,
			v1.LabelTopologyZone:       leastExpensiveOffering.Zone,
		})
		machine2.Labels = lo.Assign(machine1.Labels, map[string]string{
			v1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
			v1alpha5.LabelCapacityType: leastExpensiveOffering.CapacityType,
			v1.LabelTopologyZone:       leastExpensiveOffering.Zone,
		})
		node2.Labels = lo.Assign(node2.Labels, map[string]string{
			v1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
			v1alpha5.LabelCapacityType: leastExpensiveOffering.CapacityType,
			v1.LabelTopologyZone:       leastExpensiveOffering.Zone,
		})
		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], machine1, node1, machine2, node2, prov)
		ExpectMakeNodesReady(ctx, env.Client, node1, node2)

		// bind pods to nodes
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node2)
		ExpectManualBinding(ctx, env.Client, pods[2], node2)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine1)

		// We have [cheap-node, cheap-node] which multi-node consolidation could consolidate via
		// [delete cheap-node, delete cheap-node, launch cheap-node]. This isn't the best method though
		// as we should instead just delete one of the nodes instead of deleting both and launching a single
		// identical replacement. This test verifies the filterOutSameType function from multi-node consolidation
		// works to ensure we perform the least-disruptive action.
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		// should have just deleted the node with the fewest pods
		ExpectNotFound(ctx, env.Client, machine1, node1)
		// and left the other node alone
		ExpectExists(ctx, env.Client, machine2)
		ExpectExists(ctx, env.Client, node2)
	})
	It("should wait for the node TTL for non-empty nodes before consolidating (multi-node)", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         ptr.Bool(true),
						BlockOwnerDeletion: ptr.Bool(true),
					},
				}}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], machine1, node1, machine2, node2, prov)

		// bind pods to nodes
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		ExpectManualBinding(ctx, env.Client, pods[2], node2)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node1, node2}, []*v1alpha5.Machine{machine1, machine2})

		var wg sync.WaitGroup
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)

		wg.Add(1)
		finished := atomic.Bool{}
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			defer finished.Store(true)
			ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		}()

		// wait for the controller to block on the validation timeout
		Eventually(fakeClock.HasWaiters, time.Second*5).Should(BeTrue())
		// controller should be blocking during the timeout
		Expect(finished.Load()).To(BeFalse())
		// and the node should not be deleted yet
		ExpectExists(ctx, env.Client, machine1)
		ExpectExists(ctx, env.Client, machine2)

		// advance the clock so that the timeout expires
		fakeClock.Step(31 * time.Second)
		// controller should finish
		Eventually(finished.Load, 10*time.Second).Should(BeTrue())
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine1, machine2)

		// should launch a single smaller replacement node
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		// and delete the two large ones
		ExpectNotFound(ctx, env.Client, machine1, node1, machine2, node2)
	})
})

func leastExpensiveInstanceWithZone(zone string) *cloudprovider.InstanceType {
	for _, elem := range onDemandInstances {
		if hasZone(elem.Offerings, zone) {
			return elem
		}
	}
	return onDemandInstances[len(onDemandInstances)-1]
}

func mostExpensiveInstanceWithZone(zone string) *cloudprovider.InstanceType {
	for i := len(onDemandInstances) - 1; i >= 0; i-- {
		elem := onDemandInstances[i]
		if hasZone(elem.Offerings, zone) {
			return elem
		}
	}
	return onDemandInstances[0]
}

// hasZone checks whether any of the passed offerings have a zone matching
// the passed zone
func hasZone(ofs []cloudprovider.Offering, zone string) bool {
	for _, elem := range ofs {
		if elem.Zone == zone {
			return true
		}
	}
	return false
}

func fromInt(i int) *intstr.IntOrString {
	v := intstr.FromInt(i)
	return &v
}

func ExpectTriggerVerifyAction(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			time.Sleep(250 * time.Millisecond)
			if fakeClock.HasWaiters() {
				break
			}
		}
		fakeClock.Step(45 * time.Second)
	}()
}

// ExpectNewMachinesDeleted simulates the machines being created and then removed, similar to what would happen
// during an ICE error on the created machine
func ExpectNewMachinesDeleted(ctx context.Context, c client.Client, wg *sync.WaitGroup, numNewMachines int) {
	existingMachines := ExpectMachines(ctx, c)
	existingMachineNames := sets.NewString(lo.Map(existingMachines, func(m *v1alpha5.Machine, _ int) string {
		return m.Name
	})...)

	wg.Add(1)
	go func() {
		machinesDeleted := 0
		ctx, cancel := context.WithTimeout(ctx, time.Second*30) // give up after 30s
		defer GinkgoRecover()
		defer wg.Done()
		defer cancel()
		for {
			select {
			case <-time.After(50 * time.Millisecond):
				machineList := &v1alpha5.MachineList{}
				if err := c.List(ctx, machineList); err != nil {
					continue
				}
				for i := range machineList.Items {
					m := &machineList.Items[i]
					if existingMachineNames.Has(m.Name) {
						continue
					}
					ExpectWithOffset(1, client.IgnoreNotFound(c.Delete(ctx, m))).To(Succeed())
					machinesDeleted++
					if machinesDeleted == numNewMachines {
						return
					}
				}
			case <-ctx.Done():
				Fail(fmt.Sprintf("waiting for machines to be deleted, %s", ctx.Err()))
			}
		}
	}()
}

func ExpectMakeNewMachinesReady(ctx context.Context, c client.Client, wg *sync.WaitGroup, cluster *state.Cluster,
	cloudProvider cloudprovider.CloudProvider, numNewMachines int) {

	existingMachines := ExpectMachines(ctx, c)
	existingMachineNames := sets.NewString(lo.Map(existingMachines, func(m *v1alpha5.Machine, _ int) string {
		return m.Name
	})...)

	wg.Add(1)
	go func() {
		machinesMadeReady := 0
		ctx, cancel := context.WithTimeout(ctx, time.Second*10) // give up after 10s
		defer GinkgoRecover()
		defer wg.Done()
		defer cancel()
		for {
			select {
			case <-time.After(50 * time.Millisecond):
				machineList := &v1alpha5.MachineList{}
				if err := c.List(ctx, machineList); err != nil {
					continue
				}
				for i := range machineList.Items {
					m := &machineList.Items[i]
					if existingMachineNames.Has(m.Name) {
						continue
					}
					m, n := ExpectMachineDeployedWithOffset(1, ctx, c, cluster, cloudProvider, m)
					ExpectMakeMachinesReadyWithOffset(1, ctx, c, m)
					ExpectMakeNodesReadyWithOffset(1, ctx, c, n)

					machinesMadeReady++
					existingMachineNames.Insert(m.Name)
					// did we make all the nodes ready that we expected?
					if machinesMadeReady == numNewMachines {
						return
					}
				}
			case <-ctx.Done():
				Fail(fmt.Sprintf("waiting for machines to be ready, %s", ctx.Err()))
			}
		}
	}()
}

func ExpectMakeReadyAndStateUpdated(ctx context.Context, c client.Client, nodeStateController, machineStateController controller.Controller, nodes []*v1.Node, machines []*v1alpha5.Machine) {
	ExpectMakeNodesReadyWithOffset(1, ctx, c, nodes...)
	ExpectMakeMachinesReadyWithOffset(1, ctx, c, machines...)

	// Inform cluster state about node and machine readiness
	for _, n := range nodes {
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
	}
	for _, m := range machines {
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(m))
	}
}

// cheapestOffering grabs the cheapest offering from the passed offerings
func cheapestOffering(ofs []cloudprovider.Offering) cloudprovider.Offering {
	offering := cloudprovider.Offering{Price: math.MaxFloat64}
	for _, of := range ofs {
		if of.Price < offering.Price {
			offering = of
		}
	}
	return offering
}
