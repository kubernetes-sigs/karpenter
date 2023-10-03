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
	"sort"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coreapis "github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning/orchestration"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/options"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var deprovisioningController *deprovisioning.Controller
var prov *provisioning.Provisioner
var cloudProvider *fake.CloudProvider
var nodeStateController controller.Controller
var machineStateController controller.Controller
var nodeClaimStateController controller.Controller
var fakeClock *clock.FakeClock
var recorder *test.EventRecorder
var queue *orchestration.Queue

var onDemandInstances []*cloudprovider.InstanceType
var leastExpensiveInstance, mostExpensiveInstance *cloudprovider.InstanceType
var leastExpensiveOffering, mostExpensiveOffering cloudprovider.Offering

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(coreapis.CRDs...))
	ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{Drift: lo.ToPtr(true)}}))
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeStateController = informer.NewNodeController(env.Client, cluster)
	machineStateController = informer.NewMachineController(env.Client, cluster)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cluster)
	recorder = test.NewEventRecorder()
	prov = provisioning.NewProvisioner(env.Client, env.KubernetesInterface.CoreV1(), recorder, cloudProvider, cluster)
	queue = orchestration.NewQueue(ctx, env.Client, recorder, cluster, fakeClock, true)
	deprovisioningController = deprovisioning.NewController(fakeClock, env.Client, prov, cloudProvider, recorder, cluster, queue)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	cloudProvider.Reset()
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()

	recorder.Reset() // Reset the events that we captured during the run

	// ensure any waiters on our clock are allowed to proceed before resetting our clock time
	for fakeClock.HasWaiters() {
		fakeClock.Step(1 * time.Minute)
	}
	fakeClock.SetTime(time.Now())
	cluster.Reset()
	cluster.MarkUnconsolidated()
	// Clean-up queue.
	queue.Reset()
	//queue = orchestration.NewQueue(ctx, env.Client, recorder, cluster, fakeClock, true)

	// Reset Feature Flags to test defaults
	ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{Drift: lo.ToPtr(true)}}))

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
		return onDemandInstances[i].Offerings.Cheapest().Price < onDemandInstances[j].Offerings.Cheapest().Price
	})
	leastExpensiveInstance, mostExpensiveInstance = onDemandInstances[0], onDemandInstances[len(onDemandInstances)-1]
	leastExpensiveOffering, mostExpensiveOffering = leastExpensiveInstance.Offerings[0], mostExpensiveInstance.Offerings[0]
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("Disruption Taints", func() {
	var provisioner *v1alpha5.Provisioner
	var machine *v1alpha5.Machine
	var nodePool *v1beta1.NodePool
	var nodeClaim *v1beta1.NodeClaim
	var machineNode, nodeClaimNode *v1.Node
	BeforeEach(func() {
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        1.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "spot-replacement",
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
		provisioner = test.Provisioner()
		machine, machineNode = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.LabelInstanceTypeStable:       currentInstance.Name,
					v1alpha5.LabelCapacityType:       currentInstance.Offerings[0].CapacityType,
					v1.LabelTopologyZone:             currentInstance.Offerings[0].Zone,
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
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
		nodePool = test.NodePool()
		nodeClaim, nodeClaimNode = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.LabelInstanceTypeStable: currentInstance.Name,
					v1alpha5.LabelCapacityType: currentInstance.Offerings[0].CapacityType,
					v1.LabelTopologyZone:       currentInstance.Offerings[0].Zone,
					v1beta1.NodePoolLabelKey:   nodePool.Name,
				},
			},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			currentInstance,
			replacementInstance,
		}
	})
	It("should remove taints from NodeClaims that were left tainted from a previous deprovisioning action", func() {
		pod := test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("100m"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		})
		nodePool.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{Duration: nil}
		nodeClaimNode.Spec.Taints = append(nodeClaimNode.Spec.Taints, v1beta1.DisruptionNoScheduleTaint)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, nodeClaimNode, pod)
		ExpectManualBinding(ctx, env.Client, pod, nodeClaimNode)

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodeClaimNode}, []*v1beta1.NodeClaim{nodeClaim})

		// Trigger the reconcile loop to start but don't trigger the verify action
		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
		}()
		wg.Wait()
		nodeClaimNode = ExpectNodeExists(ctx, env.Client, nodeClaimNode.Name)
		Expect(nodeClaimNode.Spec.Taints).ToNot(ContainElement(v1beta1.DisruptionNoScheduleTaint))
	})
	It("should add and remove taints from NodeClaims that fail to deprovision", func() {
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
		pod := test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("100m"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, nodeClaimNode, pod)
		ExpectManualBinding(ctx, env.Client, pod, nodeClaimNode)

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodeClaimNode}, []*v1beta1.NodeClaim{nodeClaim})

		// Trigger the reconcile loop to start but don't trigger the verify action
		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileFailed(ctx, deprovisioningController, client.ObjectKey{})
		}()

		// Iterate in a loop until we get to the validation action
		// Then, apply the pods to the cluster and bind them to the nodes
		for {
			time.Sleep(100 * time.Millisecond)
			if len(ExpectNodeClaims(ctx, env.Client)) == 2 {
				break
			}
		}
		nodeClaimNode = ExpectNodeExists(ctx, env.Client, nodeClaimNode.Name)
		Expect(nodeClaimNode.Spec.Taints).To(ContainElement(v1beta1.DisruptionNoScheduleTaint))

		createdNodeClaim := lo.Reject(ExpectNodeClaims(ctx, env.Client), func(nc *v1beta1.NodeClaim, _ int) bool {
			return nc.Name == nodeClaim.Name
		})
		ExpectDeleted(ctx, env.Client, createdNodeClaim[0])
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, createdNodeClaim[0])
		ExpectNotFound(ctx, env.Client, createdNodeClaim[0])

		wg.Wait()
		nodeClaimNode = ExpectNodeExists(ctx, env.Client, nodeClaimNode.Name)
		Expect(nodeClaimNode.Spec.Taints).ToNot(ContainElement(v1beta1.DisruptionNoScheduleTaint))
	})
	It("should add and remove taints from Machines that fail to deprovision", func() {
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{Enabled: lo.ToPtr(true)}
		pod := test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("100m"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine, machineNode, pod)
		ExpectManualBinding(ctx, env.Client, pod, machineNode)

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{machineNode}, []*v1alpha5.Machine{machine})

		fakeClock.Step(10 * time.Minute)

		// Trigger the reconcile loop to start but don't trigger the verify action
		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileFailed(ctx, deprovisioningController, client.ObjectKey{})
		}()

		// Iterate in a loop until we get to the validation action
		// Then, apply the pods to the cluster and bind them to the nodes
		for {
			time.Sleep(100 * time.Millisecond)
			if len(ExpectMachines(ctx, env.Client)) == 2 {
				break
			}
		}
		machineNode = ExpectNodeExists(ctx, env.Client, machineNode.Name)
		Expect(machineNode.Spec.Unschedulable).To(BeTrue())

		createdMachine := lo.Reject(ExpectMachines(ctx, env.Client), func(m *v1alpha5.Machine, _ int) bool {
			return m.Name == machine.Name
		})
		ExpectDeleted(ctx, env.Client, createdMachine[0])
		ExpectMachinesCascadeDeletion(ctx, env.Client, createdMachine[0])
		ExpectNotFound(ctx, env.Client, createdMachine[0])
		wg.Wait()
		machineNode = ExpectNodeExists(ctx, env.Client, machineNode.Name)
		Expect(machineNode.Spec.Unschedulable).To(BeFalse())
	})
})

var _ = Describe("Combined/Deprovisioning", func() {
	var provisioner *v1alpha5.Provisioner
	var machine *v1alpha5.Machine
	var nodePool *v1beta1.NodePool
	var nodeClaim *v1beta1.NodeClaim
	var machineNode, nodeClaimNode *v1.Node
	BeforeEach(func() {
		provisioner = test.Provisioner()
		machine, machineNode = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
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
		nodePool = test.NodePool()
		nodeClaim, nodeClaimNode = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
				},
			},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
	})
	It("should deprovision all empty Machine and NodeClaims in parallel (Emptiness)", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = lo.ToPtr[int64](30)
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenEmpty
		nodePool.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 30)}
		machine.StatusConditions().MarkTrue(v1alpha5.MachineEmpty)
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Empty)
		ExpectApplied(ctx, env.Client, provisioner, nodePool, machine, nodeClaim, machineNode, nodeClaimNode)

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{machineNode}, []*v1alpha5.Machine{machine})
		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodeClaimNode}, []*v1beta1.NodeClaim{nodeClaim})

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		ExpectQueueItemProcessed(ctx, queue)
		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)
		// Cascade any deletion of the nodeclaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

		// Expect that the empty machine is gone
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		// Expect that the empty nodeclaim is gone
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))

		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, machine, nodeClaim, machineNode, nodeClaimNode)
	})
	It("should deprovision all empty Machine and NodeClaims in parallel (Expiration)", func() {
		provisioner.Spec.TTLSecondsUntilExpired = lo.ToPtr[int64](30)
		nodePool.Spec.Disruption.ExpireAfter = v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 30)}
		machine.StatusConditions().MarkTrue(v1alpha5.MachineExpired)
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Expired)
		ExpectApplied(ctx, env.Client, provisioner, nodePool, machine, nodeClaim, machineNode, nodeClaimNode)

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{machineNode}, []*v1alpha5.Machine{machine})
		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodeClaimNode}, []*v1beta1.NodeClaim{nodeClaim})

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		ExpectQueueItemProcessed(ctx, queue)
		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)
		// Cascade any deletion of the nodeclaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

		// Expect that the expired machine is gone
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		// Expect that the expired nodeclaim is gone
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))

		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, machine, nodeClaim, machineNode, nodeClaimNode)
	})
	It("should deprovision all empty Machine and NodeClaims in parallel (Drift)", func() {
		machine.StatusConditions().MarkTrue(v1alpha5.MachineDrifted)
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Drifted)

		ExpectApplied(ctx, env.Client, provisioner, nodePool, machine, nodeClaim, machineNode, nodeClaimNode)

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{machineNode}, []*v1alpha5.Machine{machine})
		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodeClaimNode}, []*v1beta1.NodeClaim{nodeClaim})

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		ExpectQueueItemProcessed(ctx, queue)
		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)
		// Cascade any deletion of the nodeclaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

		// Expect that the drifted machine is gone
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		// Expect that the drifted nodeclaim is gone
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))

		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, machine, nodeClaim, machineNode, nodeClaimNode)
	})
	It("should deprovision all empty Machine and NodeClaims in parallel (Consolidation)", func() {
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{Enabled: lo.ToPtr(true)}
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
		ExpectApplied(ctx, env.Client, provisioner, nodePool, machine, nodeClaim, machineNode, nodeClaimNode)

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{machineNode}, []*v1alpha5.Machine{machine})
		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodeClaimNode}, []*v1beta1.NodeClaim{nodeClaim})

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		ExpectQueueItemProcessed(ctx, queue)
		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)
		// Cascade any deletion of the nodeclaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

		// Expect that the empty machine is gone due to consolidation
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		// Expect that the empty nodeclaim is gone due to consolidation
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))

		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, machine, nodeClaim, machineNode, nodeClaimNode)
	})
	It("should deprovision a Machine and replace with a cheaper NodeClaim", func() {
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{Enabled: lo.ToPtr(true)}
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        1.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "spot-replacement",
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
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{replacementInstance.Name},
			},
			{
				Key:      v1alpha5.LabelCapacityType,
				Operator: v1.NodeSelectorOpExists,
			},
		}
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{currentInstance.Name},
			},
		}
		machine.Labels = lo.Assign(machine.Labels, map[string]string{
			v1.LabelInstanceTypeStable: currentInstance.Name,
			v1alpha5.LabelCapacityType: currentInstance.Offerings[0].CapacityType,
			v1.LabelTopologyZone:       currentInstance.Offerings[0].Zone,
		})
		machineNode.Labels = lo.Assign(machineNode.Labels, map[string]string{
			v1.LabelInstanceTypeStable: currentInstance.Name,
			v1alpha5.LabelCapacityType: currentInstance.Offerings[0].CapacityType,
			v1.LabelTopologyZone:       currentInstance.Offerings[0].Zone,
		})
		pod := test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("100m"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, nodePool, machine, machineNode, pod)
		ExpectManualBinding(ctx, env.Client, pod, machineNode)

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{machineNode}, []*v1alpha5.Machine{machine})

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		ExpectQueueItemProcessed(ctx, queue)
		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)

		// a machine should be replaced with a single nodeclaim
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectNotFound(ctx, env.Client, machine, machineNode)
	})
	It("should deprovision multiple Machines and replace with a single cheaper NodeClaim", func() {
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{Enabled: lo.ToPtr(true)}
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        1.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "spot-replacement",
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
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{replacementInstance.Name},
			},
		}
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{currentInstance.Name},
			},
			{
				Key:      v1alpha5.LabelCapacityType,
				Operator: v1.NodeSelectorOpExists,
			},
		}

		ExpectApplied(ctx, env.Client, provisioner, nodePool)

		var machines []*v1alpha5.Machine
		var nodes []*v1.Node
		for i := 0; i < 5; i++ {
			m, n := test.MachineAndNode(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       currentInstance.Name,
						v1alpha5.LabelCapacityType:       currentInstance.Offerings[0].CapacityType,
						v1.LabelTopologyZone:             currentInstance.Offerings[0].Zone,
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
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100m"),
						v1.ResourceMemory: resource.MustParse("100Mi"),
					},
				},
			})
			machines = append(machines, m)
			nodes = append(nodes, n)
			ExpectApplied(ctx, env.Client, m, n, pod)
			ExpectManualBinding(ctx, env.Client, pod, n)
		}

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, nodes, machines)

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		ExpectQueueItemProcessed(ctx, queue)
		// Cascade any deletion of the machines to the nodes
		ExpectMachinesCascadeDeletion(ctx, env.Client, machines...)

		// all machines should be replaced with a single nodeclaim
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))

		ExpectNotFound(ctx, env.Client, lo.Map(machines, func(m *v1alpha5.Machine, _ int) client.Object { return m })...)
		ExpectNotFound(ctx, env.Client, lo.Map(nodes, func(n *v1.Node, _ int) client.Object { return n })...)
	})
	It("should deprovision a NodeClaim and replace with a cheaper Machine", func() {
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{Enabled: lo.ToPtr(true)}
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        1.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "spot-replacement",
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
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{currentInstance.Name},
			},
		}
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{replacementInstance.Name},
			},
			{
				Key:      v1alpha5.LabelCapacityType,
				Operator: v1.NodeSelectorOpExists,
			},
		}
		nodeClaim.Labels = lo.Assign(nodeClaim.Labels, map[string]string{
			v1.LabelInstanceTypeStable:   currentInstance.Name,
			v1beta1.CapacityTypeLabelKey: currentInstance.Offerings[0].CapacityType,
			v1.LabelTopologyZone:         currentInstance.Offerings[0].Zone,
		})
		nodeClaimNode.Labels = lo.Assign(nodeClaimNode.Labels, map[string]string{
			v1.LabelInstanceTypeStable:   currentInstance.Name,
			v1beta1.CapacityTypeLabelKey: currentInstance.Offerings[0].CapacityType,
			v1.LabelTopologyZone:         currentInstance.Offerings[0].Zone,
		})
		pod := test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("100m"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, nodePool, nodeClaim, nodeClaimNode, pod)
		ExpectManualBinding(ctx, env.Client, pod, nodeClaimNode)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodeClaimNode}, []*v1beta1.NodeClaim{nodeClaim})

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		ExpectQueueItemProcessed(ctx, queue)
		// Cascade any deletion of the nodeclaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

		// a nodeclaim should be replaced with a single machine
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectNotFound(ctx, env.Client, nodeClaim, nodeClaimNode)
	})
	It("should deprovision multiple NodeClaims and replace with a single cheaper Machine", func() {
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{Enabled: lo.ToPtr(true)}
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        1.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "spot-replacement",
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
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{currentInstance.Name},
			},
		}
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{replacementInstance.Name},
			},
			{
				Key:      v1alpha5.LabelCapacityType,
				Operator: v1.NodeSelectorOpExists,
			},
		}

		ExpectApplied(ctx, env.Client, provisioner, nodePool)

		var nodeClaims []*v1beta1.NodeClaim
		var nodes []*v1.Node
		for i := 0; i < 5; i++ {
			nc, n := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   currentInstance.Name,
						v1beta1.CapacityTypeLabelKey: currentInstance.Offerings[0].CapacityType,
						v1.LabelTopologyZone:         currentInstance.Offerings[0].Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100m"),
						v1.ResourceMemory: resource.MustParse("100Mi"),
					},
				},
			})
			nodeClaims = append(nodeClaims, nc)
			nodes = append(nodes, n)
			ExpectApplied(ctx, env.Client, nc, n, pod)
			ExpectManualBinding(ctx, env.Client, pod, n)
		}

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Cascade any deletion of the nodeclaims to the nodes
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims...)

		// all nodeClaims should be replaced with a single machine
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))

		ExpectNotFound(ctx, env.Client, lo.Map(nodeClaims, func(nc *v1beta1.NodeClaim, _ int) client.Object { return nc })...)
		ExpectNotFound(ctx, env.Client, lo.Map(nodes, func(n *v1.Node, _ int) client.Object { return n })...)
	})
	It("should deprovision Machines and NodeClaims to consolidate pods onto a single NodeClaim", func() {
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{Enabled: lo.ToPtr(true)}
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        1.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "spot-replacement",
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
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{replacementInstance.Name},
			},
		}
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{currentInstance.Name},
			},
			{
				Key:      v1alpha5.LabelCapacityType,
				Operator: v1.NodeSelectorOpExists,
			},
		}

		ExpectApplied(ctx, env.Client, provisioner, nodePool)

		var machines []*v1alpha5.Machine
		var nodeClaims []*v1beta1.NodeClaim
		var nodes []*v1.Node
		for i := 0; i < 2; i++ {
			m, n := test.MachineAndNode(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       currentInstance.Name,
						v1alpha5.LabelCapacityType:       currentInstance.Offerings[0].CapacityType,
						v1.LabelTopologyZone:             currentInstance.Offerings[0].Zone,
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
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100m"),
						v1.ResourceMemory: resource.MustParse("100Mi"),
					},
				},
			})
			machines = append(machines, m)
			nodes = append(nodes, n)
			ExpectApplied(ctx, env.Client, m, n, pod)
			ExpectManualBinding(ctx, env.Client, pod, n)
		}
		for i := 0; i < 2; i++ {
			nc, n := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   currentInstance.Name,
						v1beta1.CapacityTypeLabelKey: currentInstance.Offerings[0].CapacityType,
						v1.LabelTopologyZone:         currentInstance.Offerings[0].Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100m"),
						v1.ResourceMemory: resource.MustParse("100Mi"),
					},
				},
			})
			nodeClaims = append(nodeClaims, nc)
			nodes = append(nodes, n)
			ExpectApplied(ctx, env.Client, nc, n, pod)
			ExpectManualBinding(ctx, env.Client, pod, n)
		}

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, nodes, machines)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		ExpectQueueItemProcessed(ctx, queue)
		// Cascade any deletion of the machines to the nodes
		ExpectMachinesCascadeDeletion(ctx, env.Client, machines...)
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims...)

		// all machines and nodeclaims should be replaced with a single nodeclaim
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))

		ExpectNotFound(ctx, env.Client, lo.Map(machines, func(m *v1alpha5.Machine, _ int) client.Object { return m })...)
		ExpectNotFound(ctx, env.Client, lo.Map(nodeClaims, func(nc *v1beta1.NodeClaim, _ int) client.Object { return nc })...)
		ExpectNotFound(ctx, env.Client, lo.Map(nodes, func(n *v1.Node, _ int) client.Object { return n })...)
	})
	It("should deprovision Machines and NodeClaims to consolidate pods onto a single Machine", func() {
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{Enabled: lo.ToPtr(true)}
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        1.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "spot-replacement",
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
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{currentInstance.Name},
			},
		}
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{replacementInstance.Name},
			},
			{
				Key:      v1alpha5.LabelCapacityType,
				Operator: v1.NodeSelectorOpExists,
			},
		}

		ExpectApplied(ctx, env.Client, provisioner, nodePool)

		var machines []*v1alpha5.Machine
		var nodeClaims []*v1beta1.NodeClaim
		var nodes []*v1.Node
		for i := 0; i < 2; i++ {
			m, n := test.MachineAndNode(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       currentInstance.Name,
						v1alpha5.LabelCapacityType:       currentInstance.Offerings[0].CapacityType,
						v1.LabelTopologyZone:             currentInstance.Offerings[0].Zone,
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
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100m"),
						v1.ResourceMemory: resource.MustParse("100Mi"),
					},
				},
			})
			machines = append(machines, m)
			nodes = append(nodes, n)
			ExpectApplied(ctx, env.Client, m, n, pod)
			ExpectManualBinding(ctx, env.Client, pod, n)
		}
		for i := 0; i < 2; i++ {
			nc, n := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   currentInstance.Name,
						v1beta1.CapacityTypeLabelKey: currentInstance.Offerings[0].CapacityType,
						v1.LabelTopologyZone:         currentInstance.Offerings[0].Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100m"),
						v1.ResourceMemory: resource.MustParse("100Mi"),
					},
				},
			})
			nodeClaims = append(nodeClaims, nc)
			nodes = append(nodes, n)
			ExpectApplied(ctx, env.Client, nc, n, pod)
			ExpectManualBinding(ctx, env.Client, pod, n)
		}

		// inform cluster state about nodes and machines
		ExpectMakeNodesAndMachinesInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, nodes, machines)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		fakeClock.Step(10 * time.Minute)
		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		ExpectQueueItemProcessed(ctx, queue)
		// Cascade any deletion of the machines to the nodes
		ExpectMachinesCascadeDeletion(ctx, env.Client, machines...)
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims...)

		// all machines and nodeclaims should be replaced with a single nodeclaim
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))

		ExpectNotFound(ctx, env.Client, lo.Map(machines, func(m *v1alpha5.Machine, _ int) client.Object { return m })...)
		ExpectNotFound(ctx, env.Client, lo.Map(nodeClaims, func(nc *v1beta1.NodeClaim, _ int) client.Object { return nc })...)
		ExpectNotFound(ctx, env.Client, lo.Map(nodes, func(n *v1.Node, _ int) client.Object { return n })...)
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

func leastExpensiveInstanceWithZone(zone string) *cloudprovider.InstanceType {
	for _, elem := range onDemandInstances {
		if len(elem.Offerings.Requirements(scheduling.NewRequirements(scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, zone)))) > 0 {
			return elem
		}
	}
	return onDemandInstances[len(onDemandInstances)-1]
}

func mostExpensiveInstanceWithZone(zone string) *cloudprovider.InstanceType {
	for i := len(onDemandInstances) - 1; i >= 0; i-- {
		elem := onDemandInstances[i]
		if len(elem.Offerings.Requirements(scheduling.NewRequirements(scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, zone)))) > 0 {
			return elem
		}
	}
	return onDemandInstances[0]
}

//nolint:unparam
func fromInt(i int) *intstr.IntOrString {
	v := intstr.FromInt(i)
	return &v
}

func ExpectQueueItemProcessed(ctx context.Context, queue *orchestration.Queue) {
	cmd, shutdown := queue.Pop()
	Expect(shutdown).To(BeFalse())
	queue.ProcessItem(ctx, cmd)
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
	GinkgoHelper()
	existingMachines := ExpectMachines(ctx, c)
	existingMachineNames := sets.NewString(lo.Map(existingMachines, func(m *v1alpha5.Machine, _ int) string {
		return m.Name
	})...)

	wg.Add(1)
	go func() {
		GinkgoHelper()
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
					Expect(client.IgnoreNotFound(c.Delete(ctx, m))).To(Succeed())
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
	GinkgoHelper()

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
					m, n := ExpectMachineDeployed(ctx, c, cluster, cloudProvider, m)
					ExpectMakeMachinesInitialized(ctx, c, m)
					ExpectMakeNodesInitialized(ctx, c, n)

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

// ExpectNewMachinesDeleted simulates the machines being created and then removed, similar to what would happen
// during an ICE error on the created machine
func ExpectNewNodeClaimsDeleted(ctx context.Context, c client.Client, wg *sync.WaitGroup, numNewNodeClaims int) {
	GinkgoHelper()
	existingNodeClaims := ExpectNodeClaims(ctx, c)
	existingNodeClaimNames := sets.NewString(lo.Map(existingNodeClaims, func(nc *v1beta1.NodeClaim, _ int) string {
		return nc.Name
	})...)

	wg.Add(1)
	go func() {
		GinkgoHelper()
		nodeClaimsDeleted := 0
		ctx, cancel := context.WithTimeout(ctx, time.Second*30) // give up after 30s
		defer GinkgoRecover()
		defer wg.Done()
		defer cancel()
		for {
			select {
			case <-time.After(50 * time.Millisecond):
				nodeClaimList := &v1beta1.NodeClaimList{}
				if err := c.List(ctx, nodeClaimList); err != nil {
					continue
				}
				for i := range nodeClaimList.Items {
					m := &nodeClaimList.Items[i]
					if existingNodeClaimNames.Has(m.Name) {
						continue
					}
					Expect(client.IgnoreNotFound(c.Delete(ctx, m))).To(Succeed())
					nodeClaimsDeleted++
					if nodeClaimsDeleted == numNewNodeClaims {
						return
					}
				}
			case <-ctx.Done():
				Fail(fmt.Sprintf("waiting for nodeclaims to be deleted, %s", ctx.Err()))
			}
		}
	}()
}

func ExpectMakeNewNodeClaimsReady(ctx context.Context, c client.Client, wg *sync.WaitGroup, cluster *state.Cluster,
	cloudProvider cloudprovider.CloudProvider, numNewNodeClaims int) {
	GinkgoHelper()

	existingNodeClaims := ExpectNodeClaims(ctx, c)
	existingNodeClaimNames := sets.NewString(lo.Map(existingNodeClaims, func(nc *v1beta1.NodeClaim, _ int) string {
		return nc.Name
	})...)

	wg.Add(1)
	go func() {
		nodeClaimsMadeReady := 0
		ctx, cancel := context.WithTimeout(ctx, time.Second*10) // give up after 10s
		defer GinkgoRecover()
		defer wg.Done()
		defer cancel()
		for {
			select {
			case <-time.After(50 * time.Millisecond):
				nodeClaimList := &v1beta1.NodeClaimList{}
				if err := c.List(ctx, nodeClaimList); err != nil {
					continue
				}
				for i := range nodeClaimList.Items {
					nc := &nodeClaimList.Items[i]
					if existingNodeClaimNames.Has(nc.Name) {
						continue
					}
					nc, n := ExpectNodeClaimDeployed(ctx, c, cluster, cloudProvider, nc)
					ExpectMakeNodeClaimsInitialized(ctx, c, nc)
					ExpectMakeNodesInitialized(ctx, c, n)

					nodeClaimsMadeReady++
					existingNodeClaimNames.Insert(nc.Name)
					// did we make all the nodes ready that we expected?
					if nodeClaimsMadeReady == numNewNodeClaims {
						return
					}
				}
			case <-ctx.Done():
				Fail(fmt.Sprintf("waiting for nodeclaims to be ready, %s", ctx.Err()))
			}
		}
	}()
}
