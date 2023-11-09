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

package disruption_test

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

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Expiration", func() {
	var nodePool *v1beta1.NodePool
	var nodeClaim *v1beta1.NodeClaim
	var node *v1.Node

	BeforeEach(func() {
		nodePool = test.NodePool(v1beta1.NodePool{
			Spec: v1beta1.NodePoolSpec{
				Disruption: v1beta1.Disruption{
					ConsolidateAfter: &v1beta1.NillableDuration{Duration: nil},
					ExpireAfter:      v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 30)},
				},
			},
		})
		nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
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
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Expired)
	})
	It("should ignore nodes without the expired status condition", func() {
		_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Expired)
		ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})

		// Expect to not create or delete more nodeclaims
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, nodeClaim)
	})
	It("should ignore nodes with the karpenter.sh/do-not-disrupt annotation", func() {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1beta1.DoNotDisruptAnnotationKey: "true"})
		ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})

		// Expect to not create or delete more nodeclaims
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, nodeClaim)
	})
	It("should ignore nodes that have pods with the karpenter.sh/do-not-evict annotation", func() {
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1alpha5.DoNotEvictPodAnnotationKey: "true",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})

		// Expect to not create or delete more nodeclaims
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, nodeClaim)
	})
	It("should ignore nodes that have pods with the karpenter.sh/do-not-disrupt annotation", func() {
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1beta1.DoNotDisruptAnnotationKey: "true",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})

		// Expect to not create or delete more nodeclaims
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, nodeClaim)
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
		ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		nodeClaim2, node2 := test.NodeClaimAndNode(v1beta1.NodeClaim{
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
					v1.ResourceCPU:  resource.MustParse("1"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		nodeClaim2.StatusConditions().MarkTrue(v1beta1.Expired)
		ExpectApplied(ctx, env.Client, nodeClaim2, node2, podToExpire)
		ExpectManualBinding(ctx, env.Client, podToExpire, node2)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node2}, []*v1beta1.NodeClaim{nodeClaim2})

		// disruption won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})
		wg.Wait()

		// Process the item so that the nodes can be deleted.
		ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim, nodeClaim2)

		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		ExpectExists(ctx, env.Client, nodeClaim)
		ExpectNotFound(ctx, env.Client, nodeClaim2)
	})
	It("should ignore nodes with the expired status condition set to false", func() {
		nodeClaim.StatusConditions().MarkFalse(v1beta1.Expired, "", "")
		ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		fakeClock.Step(10 * time.Minute)

		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})

		// Expect to not create or delete more nodeclaims
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, nodeClaim)
	})
	It("can delete expired nodes", func() {
		ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})
		wg.Wait()

		// Process the item so that the nodes can be deleted.
		ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		// Cascade any deletion of the nodeClaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

		// Expect that the expired nodeClaim is gone
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, nodeClaim, node)
	})
	It("should disrupt all empty expired nodes in parallel", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(100, v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
				},
			},
			Status: v1beta1.NodeClaimStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		for _, m := range nodeClaims {
			m.StatusConditions().MarkTrue(v1beta1.Expired)
			ExpectApplied(ctx, env.Client, m)
		}
		for _, n := range nodes {
			ExpectApplied(ctx, env.Client, n)
		}
		ExpectApplied(ctx, env.Client, nodePool)

		// inform cluster state about nodes and nodeClaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})
		wg.Wait()

		// Process the item so that the nodes can be deleted.
		ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		// Cascade any deletion of the nodeClaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims...)

		// Expect that the expired nodeClaims are gone
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
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
		nodeClaim2, node2 := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
				},
			},
			Status: v1beta1.NodeClaimStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
			},
		})
		nodeClaim2.Status.Conditions = append(nodeClaim2.Status.Conditions, apis.Condition{
			Type:               v1beta1.Expired,
			Status:             v1.ConditionTrue,
			LastTransitionTime: apis.VolatileTime{Inner: metav1.Time{Time: time.Now().Add(-time.Hour)}},
		})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], nodeClaim, nodeClaim2, node, node2, nodePool)

		// bind pods to node so that they're not empty and don't disrupt in parallel.
		ExpectManualBinding(ctx, env.Client, pods[0], node)
		ExpectManualBinding(ctx, env.Client, pods[1], node2)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

		// disruption won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})
		wg.Wait()

		// Process the item so that the nodes can be deleted.
		ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		// Cascade any deletion of the nodeClaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim2)

		// Expect that one of the expired nodeclaims is gone
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		ExpectNotFound(ctx, env.Client, nodeClaim2, node2)
		ExpectExists(ctx, env.Client, nodeClaim)
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
		ExpectApplied(ctx, env.Client, rs, pod, nodeClaim, node, nodePool)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		// disruption won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})
		wg.Wait()

		// Process the item so that the nodes can be deleted.
		ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		// Cascade any deletion of the nodeClaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

		// Expect that the new nodeClaim was created, and it's different than the original
		ExpectNotFound(ctx, env.Client, nodeClaim, node)
		nodeclaims := ExpectNodeClaims(ctx, env.Client)
		nodes := ExpectNodes(ctx, env.Client)
		Expect(nodeclaims).To(HaveLen(1))
		Expect(nodes).To(HaveLen(1))
		Expect(nodeclaims[0].Name).ToNot(Equal(nodeClaim.Name))
		Expect(nodes[0].Name).ToNot(Equal(node.Name))
	})
	It("should untaint nodes when expiration replacement fails", func() {
<<<<<<< HEAD
		cloudProvider.AllowedCreateCalls = 0 // fail the replacement and expect it to untaint
=======
		cloudProvider.AllowedCreateCalls = 0 // fail the replacement and expect it to uncordon
>>>>>>> 4127666b (revert queue)

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
		ExpectApplied(ctx, env.Client, rs, nodeClaim, node, nodePool, pod)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectNewNodeClaimsDeleted(ctx, env.Client, &wg, 1)
		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})
		wg.Wait()

		ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		// We should have tried to create a new nodeClaim but failed to do so; therefore, we uncordoned the existing node
		node = ExpectExists(ctx, env.Client, node)
		Expect(node.Spec.Taints).ToNot(ContainElement(v1beta1.DisruptionNoScheduleTaint))
	})
	It("can replace node for expiration with multiple nodes", func() {
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1beta1.CapacityTypeOnDemand,
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
					CapacityType: v1beta1.CapacityTypeOnDemand,
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
		nodeClaim.Labels = lo.Assign(nodeClaim.Labels, map[string]string{
			v1.LabelInstanceTypeStable:   currentInstance.Name,
			v1beta1.CapacityTypeLabelKey: currentInstance.Offerings[0].CapacityType,
			v1.LabelTopologyZone:         currentInstance.Offerings[0].Zone,
		})
		nodeClaim.Status.Allocatable = map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("8")}
		node.Labels = lo.Assign(node.Labels, map[string]string{
			v1.LabelInstanceTypeStable:   currentInstance.Name,
			v1beta1.CapacityTypeLabelKey: currentInstance.Offerings[0].CapacityType,
			v1.LabelTopologyZone:         currentInstance.Offerings[0].Zone,
		})
		node.Status.Allocatable = map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("8")}

		ExpectApplied(ctx, env.Client, rs, nodeClaim, node, nodePool, pods[0], pods[1], pods[2])

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pods[0], node)
		ExpectManualBinding(ctx, env.Client, pods[1], node)
		ExpectManualBinding(ctx, env.Client, pods[2], node)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		// disruption won't delete the old nodeClaim until the new nodeClaim is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 3)
		ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})
		wg.Wait()

		// Process the item so that the nodes can be deleted.
		ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
		// Cascade any deletion of the nodeClaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(3))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(3))
		ExpectNotFound(ctx, env.Client, nodeClaim, node)
	})
})
