/*
Copyright The Kubernetes Authors.

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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const hugePages2Mi corev1.ResourceName = "hugepages-2Mi"

func flexibleOnDemandInstanceType(name, memory string) *cloudprovider.InstanceType {
	resources := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse(memory),
		corev1.ResourcePods:   resource.MustParse("10"),
	}
	it := fake.NewInstanceType(name,
		fake.WithResources(resources),
		fake.WithOfferings(cloudprovider.Offering{
			Available: true,
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
				corev1.LabelTopologyZone: "test-zone-1",
			}),
			Price: fake.PriceFromResources(resources),
		}),
	)
	it.ApplyMaxCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("8Gi")})
	return it
}

var _ = Describe("Flexible Capacity Consolidation", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	BeforeEach(func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			flexibleOnDemandInstanceType("small", "4Gi"),
			flexibleOnDemandInstanceType("large", "16Gi"),
		}
		ExpectSingletonReconciled(ctx, pricingController)
		nodePool = test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				// The test instance types only have on-demand offerings
				Template: v1.NodeClaimTemplate{
					Spec: v1.NodeClaimTemplateSpec{
						Requirements: []v1.NodeSelectorRequirementWithMinValues{
							{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}},
						},
					},
				},
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
					Budgets:             []v1.Budget{{Nodes: "100%"}},
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
				},
			},
		})
		// A large node that was launched for pods needing 3Gi of hugepages
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: "large",
					v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
					corev1.LabelTopologyZone:       "test-zone-1",
				},
				Annotations: map[string]string{v1.FlexibleCapacityAnnotationKey: `{"hugepages-2Mi":"3Gi"}`},
			},
			Status: v1.NodeClaimStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("3900m"),
					corev1.ResourceMemory: resource.MustParse("13302Mi"),
					corev1.ResourcePods:   resource.MustParse("10"),
					hugePages2Mi:          resource.MustParse("3Gi"),
				},
			},
		})
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
	})
	It("should size the replacement node to the hugepages of the pods that remain", func() {
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         "apps/v1",
					Kind:               "ReplicaSet",
					Name:               rs.Name,
					UID:                rs.UID,
					Controller:         new(true),
					BlockOwnerDeletion: new(true),
				}},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("256Mi"),
					hugePages2Mi:          resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{hugePages2Mi: resource.MustParse("1Gi")},
			},
		})
		ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectSingletonReconciled(ctx, disruptionController)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, env.Clock, cluster, cloudProvider, cmds[0])
		ExpectObjectReconciled(ctx, env.Client, queue, nodeClaim)
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

		nodeClaims := ExpectNodeClaims(ctx, env.Client)
		Expect(nodeClaims).To(HaveLen(1))
		Expect(nodeClaims[0].Name).ToNot(Equal(nodeClaim.Name))
		Expect(nodeClaims[0].Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"1Gi"}`))
		Expect(scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaims[0].Spec.Requirements...).Get(corev1.LabelInstanceTypeStable).Values()).To(ConsistOf("small"))
		ExpectNotFound(ctx, env.Client, nodeClaim, node)
	})
})
