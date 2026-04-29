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

// End-to-end test for kubernetes-sigs/karpenter#1962: multi-node
// consolidation's binary search over sorted prefixes is prefix-blind.
// When a candidate that blocks joint deletion (e.g. a node hosting a
// pod whose requirements no other node can satisfy) sorts between
// good candidates, every prefix containing the bad candidate fails
// the simulator. The binary search exits empty and multi-node
// consolidation contributes nothing, even though a non-prefix subset
// excluding the bad candidate would consolidate cleanly.
//
// This test runs against the real disruption stack (envtest, fake
// CloudProvider, real provisioner). It constructs three candidates
// sorted [good_0, bad, good_2] by disruption cost, where bad's pod
// has a NodeSelector that no remaining node and no replacement can
// satisfy. The good pods are unrestricted and fit anywhere.
//
// Without the fix in firstNConsolidationOption, multi-node returns
// no command. With the fix (binary search first, pairwise non-prefix
// fallback when it returns empty), multi-node returns one command
// deleting good_0 and good_2 together.

package disruption_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("MultiNode Consolidation Non-Prefix Subset (#1962)", func() {
	It("finds a non-prefix consolidation when a bad candidate sorts between good ones", func() {
		nodePool := test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
					Budgets:             []v1.Budget{{Nodes: "100%"}},
				},
			},
		})

		nodeClaims := make([]*v1.NodeClaim, 3)
		nodes := make([]*corev1.Node, 3)
		for i := range nodeClaims {
			nodeClaims[i], nodes[i] = test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					Allocatable: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU:  resource.MustParse("32"),
						corev1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaims[i].StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		}

		// nodes[1] is the "bad" candidate. Tag it with a unique label so
		// only its own pod (which sets the matching NodeSelector) can
		// land there. The label is not in the NodePool template, so
		// replacement nodes won't carry it either.
		nodes[1].Labels["bad-only"] = "true"

		labels := map[string]string{"app": "test"}
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

		ownerRefs := []metav1.OwnerReference{{
			APIVersion:         "apps/v1",
			Kind:               "ReplicaSet",
			Name:               rs.Name,
			UID:                rs.UID,
			Controller:         lo.ToPtr(true),
			BlockOwnerDeletion: lo.ToPtr(true),
		}}

		// Sort order is by disruption cost ascending. We use the pod
		// deletion cost annotation to pin good_0 first, bad in the
		// middle, good_2 last. With this order the binary search probes
		// [good_0, bad] and fails on bad's unschedulable pod.
		podGood0 := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels:          labels,
				OwnerReferences: ownerRefs,
				Annotations:     map[string]string{corev1.PodDeletionCost: "-2147483647"},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})
		podBad := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels:          labels,
				OwnerReferences: ownerRefs,
			},
			NodeSelector: map[string]string{"bad-only": "true"},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})
		podGood2 := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels:          labels,
				OwnerReferences: ownerRefs,
				Annotations:     map[string]string{corev1.PodDeletionCost: "2147483647"},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})

		ExpectApplied(ctx, env.Client, nodePool,
			nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2],
			podGood0, podBad, podGood2)

		ExpectManualBinding(ctx, env.Client, podGood0, nodes[0])
		ExpectManualBinding(ctx, env.Client, podBad, nodes[1])
		ExpectManualBinding(ctx, env.Client, podGood2, nodes[2])

		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client,
			nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		c := disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)
		multiConsolidation := disruption.NewMultiNodeConsolidation(c,
			disruption.WithValidator(NewTestMultiConsolidationValidator(nodePool)))

		budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, fakeClock, env.Client, cloudProvider, recorder, multiConsolidation.Reason())
		Expect(err).To(Succeed())

		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, fakeClock, cloudProvider,
			multiConsolidation.ShouldDisrupt, multiConsolidation.Class(), queue)
		Expect(err).To(Succeed())
		Expect(candidates).To(HaveLen(3))

		cmds, err := multiConsolidation.ComputeCommands(ctx, budgets, candidates...)
		Expect(err).To(Succeed())

		// With the #1962 fix, multi-node consolidation finds the non-prefix
		// subset {good_0, good_2}. Without the fix, the binary search
		// probes only [good_0, bad] (which fails because bad's pod can't
		// reschedule) and exits empty.
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Candidates).To(HaveLen(2))

		providerIDs := sets.New[string]()
		for _, cand := range cmds[0].Candidates {
			providerIDs.Insert(cand.ProviderID())
		}
		Expect(providerIDs.Has(nodes[0].Spec.ProviderID)).To(BeTrue(), "good_0 should be in the consolidation set")
		Expect(providerIDs.Has(nodes[2].Spec.ProviderID)).To(BeTrue(), "good_2 should be in the consolidation set")
		Expect(providerIDs.Has(nodes[1].Spec.ProviderID)).To(BeFalse(), "bad candidate should not be in the consolidation set")
	})
})
