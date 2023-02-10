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
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	It("should ignore nodes without TTLSecondsUntilExpired", func() {
		prov := test.Provisioner()
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
		})

		ExpectApplied(ctx, env.Client, node, prov)
		ExpectMakeNodesReady(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		fakeClock.Step(10 * time.Minute)
		_, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
		Expect(err).ToNot(HaveOccurred())

		// we don't need a new node
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// and can't delete the node since expiry is not enabled
		ExpectNodeExists(ctx, env.Client, node.Name)
	})
	It("can delete expired nodes", func() {
		prov := test.Provisioner(test.ProvisionerOptions{
			TTLSecondsUntilExpired: ptr.Int64(60),
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}},
		)

		ExpectApplied(ctx, env.Client, node, prov)
		ExpectMakeNodesReady(ctx, env.Client, node)

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		_, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
		Expect(err).ToNot(HaveOccurred())
		wg.Wait()

		// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// and delete the old one
		ExpectNotFound(ctx, env.Client, node)
	})
	It("should expire one node at a time, starting with most expired", func() {
		expireProv := test.Provisioner(test.ProvisionerOptions{
			TTLSecondsUntilExpired: ptr.Int64(100),
		})
		nodeToExpire := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: expireProv.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
		})
		prov := test.Provisioner(test.ProvisionerOptions{
			TTLSecondsUntilExpired: ptr.Int64(500),
		})
		nodeNotExpire := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
		})

		ExpectApplied(ctx, env.Client, nodeToExpire, expireProv, nodeNotExpire, prov)
		ExpectMakeNodesReady(ctx, env.Client, nodeToExpire, nodeNotExpire)

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodeToExpire))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodeNotExpire))

		fakeClock.Step(10 * time.Minute)
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		_, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
		Expect(err).ToNot(HaveOccurred())
		wg.Wait()

		// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// and delete the old one
		ExpectNotFound(ctx, env.Client, nodeToExpire)
	})
	It("can replace node for expiration", func() {
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
			TTLSecondsUntilExpired: ptr.Int64(30),
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name,
					v1alpha5.LabelCapacityType:       mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:             mostExpensiveOffering.Zone,
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
		})
		ExpectApplied(ctx, env.Client, rs, pod, node, prov)
		ExpectMakeNodesReady(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectScheduled(ctx, env.Client, pod)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())

		// deprovisioning won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodesReady(ctx, env.Client, &wg, 1, node)

		fakeClock.Step(10 * time.Minute)
		_, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
		Expect(err).ToNot(HaveOccurred())
		wg.Wait()

		Expect(cloudProvider.CreateCalls).To(HaveLen(1))

		ExpectNotFound(ctx, env.Client, node)
	})
	It("should uncordon nodes when expiration replacement partially fails", func() {
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
		cloudProvider.AllowedCreateCalls = 2

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

		prov := test.Provisioner(test.ProvisionerOptions{
			TTLSecondsUntilExpired: ptr.Int64(30),
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       currentInstance.Name,
					v1alpha5.LabelCapacityType:       currentInstance.Offerings[0].CapacityType,
					v1.LabelTopologyZone:             currentInstance.Offerings[0].Zone,
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("7")},
		})
		ExpectApplied(ctx, env.Client, rs, node, prov, pods[0], pods[1], pods[2])
		ExpectMakeNodesReady(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectManualBinding(ctx, env.Client, pods[0], node)
		ExpectManualBinding(ctx, env.Client, pods[1], node)
		ExpectManualBinding(ctx, env.Client, pods[2], node)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())

		fakeClock.Step(10 * time.Minute)
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		_, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
		Expect(err).To(HaveOccurred())
		wg.Wait()

		// Expiration should try to make 3 calls but fail for the third.
		Expect(cloudProvider.CreateCalls).To(HaveLen(3))

		node = ExpectNodeExists(ctx, env.Client, node.Name)
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

		prov := test.Provisioner(test.ProvisionerOptions{
			TTLSecondsUntilExpired: ptr.Int64(200),
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       currentInstance.Name,
					v1alpha5.LabelCapacityType:       currentInstance.Offerings[0].CapacityType,
					v1.LabelTopologyZone:             currentInstance.Offerings[0].Zone,
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("8")},
		})
		ExpectApplied(ctx, env.Client, rs, node, prov, pods[0], pods[1], pods[2])
		ExpectMakeNodesReady(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectManualBinding(ctx, env.Client, pods[0], node)
		ExpectManualBinding(ctx, env.Client, pods[1], node)
		ExpectManualBinding(ctx, env.Client, pods[2], node)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())

		// deprovisioning won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodesReady(ctx, env.Client, &wg, 3, node)

		fakeClock.Step(10 * time.Minute)
		_, err := deprovisioningController.Reconcile(ctx, reconcile.Request{})
		Expect(err).ToNot(HaveOccurred())
		wg.Wait()

		Expect(cloudProvider.CreateCalls).To(HaveLen(3))

		ExpectNotFound(ctx, env.Client, node)
	})
})
