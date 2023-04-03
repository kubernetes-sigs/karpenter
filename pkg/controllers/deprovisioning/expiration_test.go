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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
		})
		machine, node = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionExpiredAnnotationValue,
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
	})
	It("should ignore nodes with the disruption annotation but different value", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.VoluntaryDisruptionAnnotationKey: "wrong-value",
		})
		ExpectApplied(ctx, env.Client, machine, node, prov)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		// Expect to not create or delete more machines
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, node)
	})
	It("should ignore nodes without the disruption annotation", func() {
		delete(node.Annotations, v1alpha5.VoluntaryDisruptionAnnotationKey)
		ExpectApplied(ctx, env.Client, machine, node, prov)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		// Expect to not create or delete more machines
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, node)
	})
	It("can delete expired nodes", func() {
		ExpectApplied(ctx, env.Client, machine, node, prov)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Expect that the expired machine is gone
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, node)
	})
	It("should expire one node at a time, starting with most expired", func() {
		expireProv := test.Provisioner(test.ProvisionerOptions{
			TTLSecondsUntilExpired: ptr.Int64(100),
		})
		machineToExpire, nodeToExpire := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionExpiredAnnotationValue,
				},
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

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{nodeToExpire, nodeNotExpire}, []*v1alpha5.Machine{machineToExpire, machineNotExpire})

		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

		// Expect that one of the expired machines is gone
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectNotFound(ctx, env.Client, nodeToExpire)
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
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		// deprovisioning won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodesReady(ctx, env.Client, &wg, 1)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		// Expect that the new machine was created, and it's different than the original
		ExpectNotFound(ctx, env.Client, node)
		nodes := ExpectNodes(ctx, env.Client)
		Expect(nodes).To(HaveLen(1))
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
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
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
		machine, node := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionExpiredAnnotationValue,
				},
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
		ExpectApplied(ctx, env.Client, rs, machine, node, prov, pods[0], pods[1], pods[2])

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pods[0], node)
		ExpectManualBinding(ctx, env.Client, pods[1], node)
		ExpectManualBinding(ctx, env.Client, pods[2], node)

		// inform cluster state about nodes and machines
		ExpectMakeReadyAndStateUpdated(ctx, env.Client, nodeStateController, machineStateController, []*v1.Node{node}, []*v1alpha5.Machine{machine})

		// deprovisioning won't delete the old machine until the new machine is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodesReady(ctx, env.Client, &wg, 3)
		ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})
		wg.Wait()

		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(3))
		ExpectNotFound(ctx, env.Client, node)
	})
})
