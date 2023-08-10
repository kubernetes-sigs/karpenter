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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Expiration", func() {
	var prov *v1alpha5.Provisioner
	var machine *v1alpha5.Machine
	var node *v1.Node

	BeforeEach(func() {
		prov = test.Provisioner(test.ProvisionerOptions{
			TTLSecondsUntilExpired: ptr.Int64(30),
			Limits: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("100"),
			},
		})
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
		machine.StatusConditions().MarkTrue(v1alpha5.MachineExpired)
	})
	It("should ignore nodes without the expired status condition", func() {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineExpired)
		ExpectApplied(ctx, env.Client, machine, node, prov)

		// inform cluster state about nodes and machines
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		// Expect to not create or delete more machines
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, machine)
	})
	It("should continue to the next expired node if the first cannot reschedule all pods", func() {
		pod := test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU: resource.MustParse("150"),
				},
			},
		})
		podToExpire := test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU: resource.MustParse("1"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, machine, node, prov, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and machines
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		machine2, node2 := test.MachineAndNode(v1alpha5.Machine{
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
					v1.ResourceCPU:  resource.MustParse("1"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		machine2.StatusConditions().MarkTrue(v1alpha5.MachineExpired)
		ExpectApplied(ctx, env.Client, machine2, node2, podToExpire)
		ExpectManualBinding(ctx, env.Client, podToExpire, node2)

		// inform cluster state about nodes and machines
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node2}, []*v1alpha5.Machine{machine2})

		// deprovisioning won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		ExpectMachinesCascadeDeletion(ctx, env.Client, machine, machine2)

		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(2))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		ExpectExists(ctx, env.Client, machine)
		ExpectNotFound(ctx, env.Client, machine2)
	})
	It("should ignore nodes with the expired status condition set to false", func() {
		machine.StatusConditions().MarkFalse(v1alpha5.MachineExpired, "", "")
		ExpectApplied(ctx, env.Client, machine, node, prov)

		// inform cluster state about nodes and machines
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		fakeClock.Step(10 * time.Minute)

		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		// Expect to not create or delete more machines
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, machine)
	})
	It("can delete expired nodes", func() {
		ExpectApplied(ctx, env.Client, machine, node, prov)

		// inform cluster state about nodes and machines
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)

		// Expect that the expired machine is gone
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, machine, node)
	})
	It("should deprovision all empty expired nodes in parallel", func() {
		machines, nodes := test.MachinesAndNodes(100, v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				},
			},
			Status: v1alpha5.MachineStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		for _, m := range machines {
			m.StatusConditions().MarkTrue(v1alpha5.MachineExpired)
			ExpectApplied(ctx, env.Client, m)
		}
		for _, n := range nodes {
			ExpectApplied(ctx, env.Client, n)
		}
		ExpectApplied(ctx, env.Client, prov)

		// inform cluster state about nodes and machines
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, nodes, machines)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machines...)

		// Expect that the expired machines are gone
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(0))
	})
	It("should expire one non-empty node at a time, starting with most expired", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

		pods := test.Pods(2, test.PodOptions{
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
			// Make each pod request only fit on a single node
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("30")},
			},
		})
		machine2, node2 := test.MachineAndNode(v1alpha5.Machine{
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
		machine2.Status.Conditions = append(machine2.Status.Conditions, apis.Condition{
			Type:               v1alpha5.MachineExpired,
			Status:             v1.ConditionTrue,
			LastTransitionTime: apis.VolatileTime{Inner: metav1.Time{Time: time.Now().Add(-time.Hour)}},
		})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], machine, machine2, node, node2, prov)

		// bind pods to node so that they're not empty and don't deprovision in parallel.
		ExpectManualBinding(ctx, env.Client, pods[0], node)
		ExpectManualBinding(ctx, env.Client, pods[1], node2)

		// inform cluster state about nodes and machines
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node, node2}, []*v1alpha5.Machine{machine, machine2})

		// deprovisioning won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine2)

		// Expect that one of the expired machines is gone
		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(2))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		ExpectNotFound(ctx, env.Client, machine2, node2)
		ExpectExists(ctx, env.Client, machine)
		ExpectExists(ctx, env.Client, node)
	})
	It("can replace node for expiration", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

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
		})
		prov.Spec.TTLSecondsUntilExpired = ptr.Int64(30)
		ExpectApplied(ctx, env.Client, rs, pod, machine, node, prov)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and machines
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		// deprovisioning won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)

		// Expect that the new machine was created, and it's different than the original
		ExpectNotFound(ctx, env.Client, machine, node)
		machines := ExpectMachines(ctx, env.Client)
		nodes := ExpectNodes(ctx, env.Client)
		Expect(machines).To(HaveLen(1))
		Expect(nodes).To(HaveLen(1))
		Expect(machines[0].Name).ToNot(Equal(machine.Name))
		Expect(nodes[0].Name).ToNot(Equal(node.Name))
	})
	It("should uncordon nodes when expiration replacement fails", func() {
		cloudProvider.AllowedCreateCalls = 0 // fail the replacement and expect it to uncordon

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
		})
		ExpectApplied(ctx, env.Client, rs, machine, node, prov, pod)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and machines
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectNewMachinesDeleted(ctx, env.Client, &wg, 1)
		_, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
		Expect(err).To(HaveOccurred())
		wg.Wait()

		// We should have tried to create a new machine but failed to do so; therefore, we uncordoned the existing node
		node = ExpectExists(ctx, env.Client, node)
		Expect(node.Spec.Unschedulable).To(BeFalse())
	})
	It("can replace node for expiration with multiple nodes", func() {
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
			Name: "replacement-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        0.3,
					Available:    true,
				},
			},
			Resources: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("3")},
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
				}},
			// Make each pod request about a third of the allocatable on the node
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("2")},
			},
		})
		machine.Labels = lo.Assign(machine.Labels, map[string]string{
			v1.LabelInstanceTypeStable: currentInstance.Name,
			v1alpha5.LabelCapacityType: currentInstance.Offerings[0].CapacityType,
			v1.LabelTopologyZone:       currentInstance.Offerings[0].Zone,
		})
		machine.Status.Allocatable = map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("8")}
		node.Labels = lo.Assign(node.Labels, map[string]string{
			v1.LabelInstanceTypeStable: currentInstance.Name,
			v1alpha5.LabelCapacityType: currentInstance.Offerings[0].CapacityType,
			v1.LabelTopologyZone:       currentInstance.Offerings[0].Zone,
		})
		node.Status.Allocatable = map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("8")}

		ExpectApplied(ctx, env.Client, rs, machine, node, prov, pods[0], pods[1], pods[2])

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pods[0], node)
		ExpectManualBinding(ctx, env.Client, pods[1], node)
		ExpectManualBinding(ctx, env.Client, pods[2], node)

		// inform cluster state about nodes and machines
		ExpectMakeInitializedAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		// deprovisioning won't delete the old machine until the new machine is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, cluster, cloudProvider, 3)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Cascade any deletion of the machine to the node
		ExpectMachinesCascadeDeletion(ctx, env.Client, machine)

		Expect(ExpectMachines(ctx, env.Client)).To(HaveLen(3))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(3))
		ExpectNotFound(ctx, env.Client, machine, node)
	})
})
