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
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
		prov = test.Provisioner()
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
	It("should ignore nodes without TTLSecondsUntilExpired", func() {
		ExpectApplied(ctx, env.Client, machine, node, prov)

		// inform cluster state about the nodes and machines
		ExpectMakeNodesReady(ctx, env.Client, node)
		ExpectMakeMachinesReady(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machine))

		fakeClock.Step(10 * time.Minute)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		// Expect to not create or delete more machines
		ExpectMachineCount(ctx, env.Client, "==", 1)
		ExpectExists(ctx, env.Client, machine)
	})
	It("can delete expired nodes", func() {
		prov.Spec.TTLSecondsUntilExpired = lo.ToPtr[int64](60)
		ExpectApplied(ctx, env.Client, machine, node, prov)

		// inform cluster state about the nodes and machines
		ExpectMakeNodesReady(ctx, env.Client, node)
		ExpectMakeMachinesReady(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machine))

		// step forward past the expiration time
		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Expect that the expired machine is gone
		ExpectMachineCount(ctx, env.Client, "==", 0)
		ExpectNotFound(ctx, env.Client, machine)
	})
	It("should expire one node at a time, starting with most expired", func() {
		expireProv := test.Provisioner(test.ProvisionerOptions{
			TTLSecondsUntilExpired: ptr.Int64(100),
		})
		machineToExpire, nodeToExpire := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: expireProv.Name,
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
		prov = test.Provisioner(test.ProvisionerOptions{
			TTLSecondsUntilExpired: ptr.Int64(500),
		})
		machineNotExpire, nodeNotExpire := test.MachineAndNode(v1alpha5.Machine{
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

		ExpectApplied(ctx, env.Client, machineToExpire, nodeToExpire, machineNotExpire, nodeNotExpire, expireProv, prov)

		// inform cluster state about the nodes and machines
		ExpectMakeNodesReady(ctx, env.Client, nodeToExpire, nodeNotExpire)
		ExpectMakeMachinesReady(ctx, env.Client, machineToExpire, machineNotExpire)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodeToExpire))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodeNotExpire))
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machineToExpire))
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machineNotExpire))

		// step forward past the expiration time
		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Expect that one of the expired machines is gone
		ExpectMachineCount(ctx, env.Client, "==", 1)
		ExpectNotFound(ctx, env.Client, machineToExpire)
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
		prov.Spec.TTLSecondsUntilExpired = lo.ToPtr[int64](30)
		ExpectApplied(ctx, env.Client, rs, pod, machine, node, prov)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectScheduled(ctx, env.Client, pod)

		// inform cluster state about the nodes and machines
		ExpectMakeNodesReady(ctx, env.Client, node)
		ExpectMakeMachinesReady(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machine))

		// deprovisioning won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, nodeStateController, machineStateController, cloudProvider, 1, machine)
		fakeClock.Step(10 * time.Minute)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Expect that the new machine was created, and it's different than the original
		ExpectMachineCount(ctx, env.Client, "==", 1)
		machineList := &v1alpha5.MachineList{}
		Expect(env.Client.List(ctx, machineList)).To(Succeed())
		Expect(machineList.Items[0].Name).ToNot(Equal(machine.Name))
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
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("8")},
			},
		})
		prov.Spec.TTLSecondsUntilExpired = lo.ToPtr[int64](200)
		ExpectApplied(ctx, env.Client, rs, machine, node, prov, pods[0], pods[1], pods[2])

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pods[0], node)
		ExpectManualBinding(ctx, env.Client, pods[1], node)
		ExpectManualBinding(ctx, env.Client, pods[2], node)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])

		// inform cluster state about the nodes and machines
		ExpectMakeNodesReady(ctx, env.Client, node)
		ExpectMakeMachinesReady(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, machineStateController, client.ObjectKeyFromObject(machine))

		// deprovisioning won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewMachinesReady(ctx, env.Client, &wg, nodeStateController, machineStateController, cloudProvider, 3, machine)
		fakeClock.Step(10 * time.Minute)
		go triggerVerifyAction()
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		ExpectMachineCount(ctx, env.Client, "==", 3)
		ExpectNotFound(ctx, env.Client, machine)
	})
})
