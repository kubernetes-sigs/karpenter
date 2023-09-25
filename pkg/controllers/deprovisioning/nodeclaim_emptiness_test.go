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

// nolint:gosec
package deprovisioning_test

import (
	"sort"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("NodeClaim/Emptiness", func() {
	Context("ConsolidationPolicy:WhenEmpty", func() {
		var nodePool *v1beta1.NodePool
		var nodeClaim *v1beta1.NodeClaim
		var node *v1.Node

		BeforeEach(func() {
			nodePool = test.NodePool(v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Disruption: v1beta1.Disruption{
						ConsolidateAfter:    &v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 30)},
						ConsolidationPolicy: v1beta1.ConsolidationPolicyWhenEmpty,
						ExpireAfter:         v1beta1.NillableDuration{Duration: nil},
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
			nodeClaim.StatusConditions().MarkTrue(v1beta1.Empty)
		})
		It("can delete empty nodes with TTLSecondsAfterEmpty", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)
			wg := sync.WaitGroup{}
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

			// Cascade any deletion of the nodeClaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the empty node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("should ignore TTLSecondsAfterEmpty nodes without the empty status condition", func() {
			_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Empty)
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
		It("should ignore TTLSecondsAfterEmpty nodes with the empty status condition set to false", func() {
			nodeClaim.StatusConditions().MarkFalse(v1beta1.Empty, "", "")
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
	})
	var _ = Describe("ConsolidatePolicy:WhenUnderutilized", func() {
		var nodePool *v1beta1.NodePool
		var nodeClaim1, nodeClaim2 *v1beta1.NodeClaim
		var node1, node2 *v1.Node

		BeforeEach(func() {
			nodePool = test.NodePool(v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Disruption: v1beta1.Disruption{
						ConsolidateAfter:    &v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 30)},
						ConsolidationPolicy: v1beta1.ConsolidationPolicyWhenUnderutilized,
						ExpireAfter:         v1beta1.NillableDuration{Duration: nil},
					},
				},
			})
			nodeClaim1, node1 = test.NodeClaimAndNode(v1beta1.NodeClaim{
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
			nodeClaim1.StatusConditions().MarkTrue(v1beta1.Empty)
			nodeClaim2, node2 = test.NodeClaimAndNode(v1beta1.NodeClaim{
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
			nodeClaim2.StatusConditions().MarkTrue(v1beta1.Empty)
		})
		It("can delete empty nodes with consolidation", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})
			wg.Wait()

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)

			// we should delete the empty node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("can delete multiple empty nodes with consolidation", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodeClaim2, node2, nodePool)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1, node2}, []*v1beta1.NodeClaim{nodeClaim1, nodeClaim2})

			fakeClock.Step(10 * time.Minute)
			wg := sync.WaitGroup{}
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, deprovisioningController, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1, nodeClaim2)

			// we should delete the empty nodes
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
			ExpectNotFound(ctx, env.Client, nodeClaim1)
			ExpectNotFound(ctx, env.Client, nodeClaim2)
		})
		It("considers pending pods when consolidating", func() {
			largeTypes := lo.Filter(cloudProvider.InstanceTypes, func(item *cloudprovider.InstanceType, index int) bool {
				return item.Capacity.Cpu().Cmp(resource.MustParse("64")) >= 0
			})
			sort.Slice(largeTypes, func(i, j int) bool {
				return largeTypes[i].Offerings[0].Price < largeTypes[j].Offerings[0].Price
			})

			largeCheapType := largeTypes[0]
			nodeClaim1, node1 = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   largeCheapType.Name,
						v1beta1.CapacityTypeLabelKey: largeCheapType.Offerings[0].CapacityType,
						v1.LabelTopologyZone:         largeCheapType.Offerings[0].Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  *largeCheapType.Capacity.Cpu(),
						v1.ResourcePods: *largeCheapType.Capacity.Pods(),
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
						v1.ResourceCPU: resource.MustParse("62"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, nodeClaim1, node1, pod, unsched, nodePool)

			// bind one of the pods to the node
			ExpectManualBinding(ctx, env.Client, pod, node1)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})

			ExpectReconcileSucceeded(ctx, deprovisioningController, client.ObjectKey{})

			// we don't need any new nodes and consolidation should notice the huge pending pod that needs the large
			// node to schedule, which prevents the large expensive node from being replaced
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim1)
		})
	})
})
