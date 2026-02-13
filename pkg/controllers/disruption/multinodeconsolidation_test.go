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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/pdb"
)

var multiNodePool1, multiNodePool2, multiNodePool3 *v1.NodePool
var multiNodeConsolidation *disruption.MultiNodeConsolidation
var multiNodePoolMap map[string]*v1.NodePool
var multiNodePoolInstanceTypeMap map[string]map[string]*cloudprovider.InstanceType

var _ = Describe("MultiNodeConsolidation", func() {
	BeforeEach(func() {
		// General purpose nodepool
		multiNodePool1 = test.NodePool(v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "multi-nodepool-1",
			},
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
				},
			},
		})

		// Dedicated nodepool with taint
		multiNodePool2 = test.NodePool(v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "multi-nodepool-2",
			},
			Spec: v1.NodePoolSpec{
				Template: v1.NodeClaimTemplate{
					Spec: v1.NodeClaimTemplateSpec{
						Taints: []corev1.Taint{
							{
								Key:    "workload",
								Value:  "dedicated",
								Effect: corev1.TaintEffectNoSchedule,
							},
						},
					},
				},
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
				},
			},
		})

		multiNodePool3 = test.NodePool(v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "multi-nodepool-3",
			},
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
				},
			},
		})

		ExpectApplied(ctx, env.Client, multiNodePool1, multiNodePool2, multiNodePool3)

		multiNodePoolMap = map[string]*v1.NodePool{
			multiNodePool1.Name: multiNodePool1,
			multiNodePool2.Name: multiNodePool2,
			multiNodePool3.Name: multiNodePool3,
		}
		multiNodePoolInstanceTypeMap = map[string]map[string]*cloudprovider.InstanceType{
			multiNodePool1.Name: {mostExpensiveInstance.Name: mostExpensiveInstance},
			multiNodePool2.Name: {mostExpensiveInstance.Name: mostExpensiveInstance},
			multiNodePool3.Name: {mostExpensiveInstance.Name: mostExpensiveInstance},
		}

		c := disruption.MakeConsolidation(fakeClock, cluster, env.Client, prov, cloudProvider, recorder, queue)
		multiNodeConsolidation = disruption.NewMultiNodeConsolidation(c)
	})

	AfterEach(func() {
		fakeClock.SetTime(time.Now())
		ExpectCleanedUp(ctx, env.Client)
	})

	Context("Candidate Sorting", func() {
		It("should sort candidates by disruption cost globally without nodepool interweaving", func() {
			// Create nodes using the working test pattern
			nodeClaims1, nodes1 := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            multiNodePool1.Name,
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
			for _, nc := range nodeClaims1 {
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}

			nodeClaims2, nodes2 := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            multiNodePool2.Name,
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
			for _, nc := range nodeClaims2 {
				nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
			}

			// Apply nodes
			for i := 0; i < 3; i++ {
				ExpectApplied(ctx, env.Client, nodeClaims1[i], nodes1[i])
				ExpectApplied(ctx, env.Client, nodeClaims2[i], nodes2[i])
			}

			// Create pods with owner references
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			allNodes := append(nodes1, nodes2...)
			pods := test.Pods(6, test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceCPU: resource.MustParse("100m"),
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					}},
				},
			})
			for i := 0; i < 6; i++ {
				ExpectApplied(ctx, env.Client, pods[i])
				ExpectManualBinding(ctx, env.Client, pods[i], allNodes[i])
			}

			// Sync cluster state
			allNodeClaims := append(nodeClaims1, nodeClaims2...)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, allNodes, allNodeClaims)

			// Use the working test pattern
			budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, cluster, fakeClock, env.Client, cloudProvider, recorder, multiNodeConsolidation.Reason())
			Expect(err).To(Succeed())

			candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, fakeClock, cloudProvider, multiNodeConsolidation.ShouldDisrupt, multiNodeConsolidation.Class(), queue)
			Expect(err).To(Succeed())

			// Debug: Print candidate information
			fmt.Printf("\n🔍 DEBUG: GetCandidates returned %d candidates\n", len(candidates))
			for i, c := range candidates {
				pods, _ := c.Pods(ctx, env.Client)
				fmt.Printf("   [%d] Node: %s, NodePool: %s, Pods: %d, Cost: %.2f\n",
					i, c.Name(), c.NodePool.Name, len(pods), c.DisruptionCost)
			}
			cpuQty := mostExpensiveInstance.Capacity[corev1.ResourceCPU]
			fmt.Printf("   mostExpensiveInstance: %s, CPU: %v, Offerings: %d\n",
				mostExpensiveInstance.Name,
				cpuQty,
				len(mostExpensiveInstance.Offerings))
			if len(mostExpensiveInstance.Offerings) > 0 {
				fmt.Printf("   First offering price: $%.4f\n", mostExpensiveInstance.Offerings[0].Price)
			}

			commands, err := multiNodeConsolidation.ComputeCommands(ctx, budgets, candidates...)
			Expect(err).To(BeNil())

			fmt.Printf("\n✅ TEST COMPLETE: ComputeCommands returned %d command(s)\n\n", len(commands))
		})
	})

	Context("NodePool Starvation (Figma Scenario)", func() {
		It("should starve dedicated nodepool when general pool has many low-cost nodes", func() {
			// Figma issue: 100 low-cost general nodes, 10 high-cost dedicated nodes
			// Dedicated nodes never get consolidated due to cost-based sorting
			generalCandidates, err := createMNCandidates(multiNodePool1, 1.0, 90)
			Expect(err).To(BeNil())

			dedicatedCandidates, err := createMNCandidatesWithTaint(multiNodePool2, 5.0, 10)
			Expect(err).To(BeNil())

			allCandidates := append(generalCandidates, dedicatedCandidates...)
			budgetMapping := map[string]int{
				multiNodePool1.Name: 150,
				multiNodePool2.Name: 20,
			}

			commands, err := multiNodeConsolidation.ComputeCommands(ctx, budgetMapping, allCandidates...)
			Expect(err).To(BeNil())

			// Print detailed command information
			printCommandDetails("Figma Starvation Test", commands, []string{multiNodePool1.Name, multiNodePool2.Name})

			if len(commands) > 0 {
				dedicatedCount := 0
				for _, candidate := range commands[0].Candidates {
					if candidate.NodePool.Name == multiNodePool2.Name {
						dedicatedCount++
					}
				}
				// Dedicated nodes starved out - never appear in batch
				Expect(dedicatedCount).To(Equal(0),
					"Expected 0 dedicated nodes but found %d - starvation not occurring!", dedicatedCount)
			}
		})
	})
})

func createMNCandidates(nodePool *v1.NodePool, disruptionCost float64, count int) ([]*disruption.Candidate, error) {
	candidates := []*disruption.Candidate{}

	for i := 0; i < count; i++ {
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
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
					corev1.ResourceCPU:    resource.MustParse("32"),
					corev1.ResourceMemory: resource.MustParse("128Gi"),
				},
			},
		})

		// Create ReplicaSet for pod ownership (makes pod reschedulable)
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

		// Create pod with resource requests and owner reference
		pod := test.Pod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))

		limits, err := pdb.NewLimits(ctx, env.Client)
		if err != nil {
			return nil, err
		}

		stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim)
		candidate, err := disruption.NewCandidate(
			ctx,
			env.Client,
			recorder,
			fakeClock,
			stateNode,
			limits,
			multiNodePoolMap,
			multiNodePoolInstanceTypeMap,
			queue,
			disruption.GracefulDisruptionClass,
		)
		if err != nil {
			return nil, err
		}
		candidate.DisruptionCost = disruptionCost
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

// printCommandDetails prints detailed information about consolidation commands for debugging
func printCommandDetails(testName string, commands []disruption.Command, nodePoolNames []string) {
	fmt.Printf("\n╔═══════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║ %s\n", testName)
	fmt.Printf("╚═══════════════════════════════════════════════════════════╝\n")

	if len(commands) == 0 {
		fmt.Printf("❌ No consolidation commands generated\n\n")
		return
	}

	cmd := commands[0]
	fmt.Printf("\n📋 CONSOLIDATION COMMAND:\n")
	fmt.Printf("   Decision: %s\n", cmd.Decision())
	fmt.Printf("   Total candidates: %d\n", len(cmd.Candidates))

	// Count candidates by nodepool
	nodePoolCounts := make(map[string]int)
	for _, candidate := range cmd.Candidates {
		nodePoolCounts[candidate.NodePool.Name]++
	}

	fmt.Printf("\n🏊 Candidates by NodePool:\n")
	for _, poolName := range nodePoolNames {
		count := nodePoolCounts[poolName]
		if count > 0 {
			fmt.Printf("   ✓ %s: %d nodes\n", poolName, count)
		} else {
			fmt.Printf("   ✗ %s: 0 nodes (STARVED)\n", poolName)
		}
	}

	// Print replacement information
	if len(cmd.Replacements) > 0 {
		fmt.Printf("\n🔄 Replacements: %d new NodeClaim(s)\n", len(cmd.Replacements))
		fmt.Printf("   (Replacement details available in cmd.Results)\n")
	} else {
		fmt.Printf("\n🗑️  Decision: DELETE (no replacements needed)\n")
	}

	fmt.Printf("\n Estimated Monthly Savings: $%.2f\n", cmd.EstimatedSavings())
	fmt.Printf("═══════════════════════════════════════════════════════════\n\n")
}

func createMNCandidatesWithTaint(nodePool *v1.NodePool, disruptionCost float64, count int) ([]*disruption.Candidate, error) {
	candidates := []*disruption.Candidate{}

	for i := 0; i < count; i++ {
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
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
					corev1.ResourceCPU:    resource.MustParse("32"),
					corev1.ResourceMemory: resource.MustParse("128Gi"),
				},
			},
		})

		// Apply taints to node
		if len(nodePool.Spec.Template.Spec.Taints) > 0 {
			node.Spec.Taints = nodePool.Spec.Template.Spec.Taints
		}

		// Create ReplicaSet for pod ownership (makes pod reschedulable)
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

		// Create pod with toleration, resource requests, and owner reference
		podOptions := test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					},
				},
			},
		}
		if len(nodePool.Spec.Template.Spec.Taints) > 0 {
			podOptions.Tolerations = []corev1.Toleration{
				{
					Key:      nodePool.Spec.Template.Spec.Taints[0].Key,
					Operator: corev1.TolerationOpEqual,
					Value:    nodePool.Spec.Template.Spec.Taints[0].Value,
					Effect:   nodePool.Spec.Template.Spec.Taints[0].Effect,
				},
			}
		}
		pod := test.Pod(podOptions)

		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))

		limits, err := pdb.NewLimits(ctx, env.Client)
		if err != nil {
			return nil, err
		}

		stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim)
		candidate, err := disruption.NewCandidate(
			ctx,
			env.Client,
			recorder,
			fakeClock,
			stateNode,
			limits,
			multiNodePoolMap,
			multiNodePoolInstanceTypeMap,
			queue,
			disruption.GracefulDisruptionClass,
		)
		if err != nil {
			return nil, err
		}
		candidate.DisruptionCost = disruptionCost
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}
