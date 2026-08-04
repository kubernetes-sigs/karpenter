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
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var (
	nodeClaim1, nodeClaim2 *v1.NodeClaim
	nodePool               *v1.NodePool
	node1, node2           *corev1.Node
)

var _ = Describe("Queue", func() {
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim1, node1 = test.NodeClaimAndNode(
			v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[0].Name,
						v1.CapacityTypeLabelKey:        cloudProvider.InstanceTypes[0].Offerings.Cheapest().Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       cloudProvider.InstanceTypes[0].Offerings.Cheapest().Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32")},
				},
			},
		)
		nodeClaim2, node2 = test.NodeClaimAndNode(
			v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[0].Name,
						v1.CapacityTypeLabelKey:        cloudProvider.InstanceTypes[0].Offerings.Cheapest().Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       cloudProvider.InstanceTypes[0].Offerings.Cheapest().Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32")},
				},
			},
		)
	})
	// multiReplacementCommand builds the alpha bounded multi-node multi-replacement command shape: a multi-node
	// consolidation command with exactly two replacements.
	multiReplacementCommand := func(q *disruption.Queue, stateNode *state.StateNode) *disruption.Command {
		nct := scheduling.NewNodeClaimTemplate(nodePool)
		nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
		nct2 := scheduling.NewNodeClaimTemplate(nodePool)
		nct2.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
		return &disruption.Command{
			Method: disruption.NewMultiNodeConsolidation(
				disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, q),
			),
			CreationTimestamp: env.Clock.Now(),
			ID:                uuid.New(),
			Results:           scheduling.Results{},
			Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
			Replacements: []*disruption.Replacement{
				{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct}},
				{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct2}},
			},
		}
	}
	enableMultiReplacement := func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
			FeatureGates: test.FeatureGates{MultiNodeMultiReplacement: lo.ToPtr(true)},
		}))
	}
	Context("Reconcile", func() {
		It("should keep nodes tainted when replacements haven't finished initialization", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			replacements := []*disruption.Replacement{
				{
					NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct},
				},
			}

			stateNode := ExpectStateNodeExists(cluster, node1)
			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements:      replacements,
			}
			Expect(queue.StartCommand(ctx, cmd)).To(BeNil())

			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).To(ContainElement(v1.DisruptedNoScheduleTaint))

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)

			// Update state
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).To(ContainElement(v1.DisruptedNoScheduleTaint))
		})
		It("should not return an error when handling commands before the timeout", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			replacements := []*disruption.Replacement{
				{
					NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct},
				},
			}

			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements:      replacements,
			}
			Expect(queue.StartCommand(ctx, cmd)).To(BeNil())
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(queue.HasAny(stateNode.ProviderID())).To(BeTrue()) // Expect the command to still be in the queue
		})
		It("should not return an error when the NodeClaim doesn't exist but the NodeCliam is in cluster state", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			replacements := []*disruption.Replacement{
				{
					NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct},
				},
			}

			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements:      replacements,
			}
			Expect(queue.StartCommand(ctx, cmd)).To(BeNil())

			replacementNodeClaim := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[0].Name}, replacementNodeClaim))
			replacementNodeClaim, _ = ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, replacementNodeClaim)

			cluster.UpdateNodeClaim(replacementNodeClaim)
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(queue.HasAny(stateNode.ProviderID())).To(BeTrue()) // Expect the command to still be in the queue
		})
		It("should untaint nodes when a command times out", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			replacements := []*disruption.Replacement{
				{
					NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct},
				},
			}

			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements:      replacements,
			}
			Expect(queue.StartCommand(ctx, cmd)).To(BeNil())

			// Step the clock to trigger the timeout.
			env.Clock.Step(11 * time.Minute)

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1.DisruptedNoScheduleTaint))
		})
		It("should fully handle a command when replacements are initialized", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			replacements := []*disruption.Replacement{
				{
					NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct},
				},
			}

			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements:      replacements,
			}
			Expect(queue.StartCommand(ctx, cmd)).To(BeNil())

			replacementNodeClaim := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[0].Name}, replacementNodeClaim))
			replacementNodeClaim, replacementNode := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, replacementNodeClaim)

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			// Get the command
			Expect(cmd.Replacements[0].Initialized).To(BeFalse())

			Expect(recorder.DetectedEvent(disruptionevents.Launching(replacementNodeClaim, string(cmd.Reason())).Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(replacementNodeClaim).Message)).To(BeTrue())

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{replacementNode}, []*v1.NodeClaim{replacementNodeClaim})

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())

			terminatingEvents := disruptionevents.Terminating(node1, nodeClaim1, string(cmd.Reason()))
			Expect(recorder.DetectedEvent(terminatingEvents[0].Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(terminatingEvents[1].Message)).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should only finish a command when all replacements are initialized", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			nct2 := scheduling.NewNodeClaimTemplate(nodePool)
			nct2.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			replacements := []*disruption.Replacement{
				{
					NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct},
				},
				{
					NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct2},
				},
			}

			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements:      replacements,
			}
			Expect(queue.StartCommand(ctx, cmd)).To(BeNil())

			replacementNodeClaim1 := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[0].Name}, replacementNodeClaim1))
			replacementNodeClaim1, replacementNode1 := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, replacementNodeClaim1)
			replacementNodeClaim2 := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[1].Name}, replacementNodeClaim2))
			replacementNodeClaim2, replacementNode2 := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, replacementNodeClaim2)

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim1).Message)).To(BeTrue())
			Expect(cmd.Replacements[1].Initialized).To(BeFalse())

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{replacementNode1}, []*v1.NodeClaim{replacementNodeClaim1})

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd.Replacements[1].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim1).Message)).To(BeTrue())

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{replacementNode2}, []*v1.NodeClaim{replacementNodeClaim2})

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd.Replacements[1].Initialized).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should retain an initialized replacement and clean up an unused replacement when the command times out", func() {
			enableMultiReplacement()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := multiReplacementCommand(queue, stateNode)
			Expect(queue.StartCommand(ctx, cmd)).To(Succeed())

			initializedNodeClaim := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[0].Name}, initializedNodeClaim)).To(Succeed())
			initializedNodeClaim, initializedNode := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, initializedNodeClaim)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{initializedNode}, []*v1.NodeClaim{initializedNodeClaim})

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd.Replacements[1].Initialized).To(BeFalse())
			ExpectExists(ctx, env.Client, nodeClaim1)

			env.Clock.Step(11 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)

			Expect(queue.IsEmpty()).To(BeTrue())
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, node1)
			ExpectExists(ctx, env.Client, initializedNodeClaim)
			ExpectExists(ctx, env.Client, initializedNode)
			ExpectNotFound(ctx, env.Client, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: cmd.Replacements[1].Name}})
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1.DisruptedNoScheduleTaint))
		})
		It("should not delete sources when an initialized replacement disappears", func() {
			enableMultiReplacement()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := multiReplacementCommand(queue, stateNode)
			Expect(queue.StartCommand(ctx, cmd)).To(Succeed())

			firstNodeClaim := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[0].Name}, firstNodeClaim)).To(Succeed())
			firstNodeClaim, firstNode := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, firstNodeClaim)
			secondNodeClaim := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[1].Name}, secondNodeClaim)).To(Succeed())
			secondNodeClaim, secondNode := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, secondNodeClaim)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{firstNode}, []*v1.NodeClaim{firstNodeClaim})
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd.Replacements[1].Initialized).To(BeFalse())

			ExpectDeleted(ctx, env.Client, firstNodeClaim)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{secondNode}, []*v1.NodeClaim{secondNodeClaim})
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)

			Expect(queue.IsEmpty()).To(BeFalse())
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, node1)
			ExpectExists(ctx, env.Client, secondNodeClaim)
			ExpectExists(ctx, env.Client, secondNode)

			env.Clock.Step(11 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(queue.IsEmpty()).To(BeTrue())
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1.DisruptedNoScheduleTaint))
		})
		It("should not delete sources when an initialized replacement node becomes not ready", func() {
			enableMultiReplacement()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := multiReplacementCommand(queue, stateNode)
			Expect(queue.StartCommand(ctx, cmd)).To(Succeed())

			firstNodeClaim := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[0].Name}, firstNodeClaim)).To(Succeed())
			firstNodeClaim, firstNode := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, firstNodeClaim)
			secondNodeClaim := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[1].Name}, secondNodeClaim)).To(Succeed())
			secondNodeClaim, secondNode := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, secondNodeClaim)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{firstNode}, []*v1.NodeClaim{firstNodeClaim})
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd.Replacements[1].Initialized).To(BeFalse())

			ExpectMakeNodesNotReady(ctx, env.Client, env.Clock, firstNode)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{secondNode}, []*v1.NodeClaim{secondNodeClaim})
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)

			Expect(queue.IsEmpty()).To(BeFalse())
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, node1)

			env.Clock.Step(11 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(queue.IsEmpty()).To(BeTrue())
			ExpectExists(ctx, env.Client, nodeClaim1)
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1.DisruptedNoScheduleTaint))
		})
		It("should retain an uninitialized replacement that is already registered and workload-bearing", func() {
			enableMultiReplacement()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := multiReplacementCommand(queue, stateNode)
			Expect(queue.StartCommand(ctx, cmd)).To(Succeed())

			replacementNodeClaim := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[0].Name}, replacementNodeClaim)).To(Succeed())
			replacementNodeClaim, replacementNode := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, replacementNodeClaim)
			workload := test.Pod()
			ExpectApplied(ctx, env.Client, workload)
			ExpectManualBinding(ctx, env.Client, workload, replacementNode)

			env.Clock.Step(11 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)

			Expect(queue.IsEmpty()).To(BeTrue())
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, node1)
			// The registered replacement is retained, while the replacement that never launched is cleaned up.
			ExpectExists(ctx, env.Client, replacementNodeClaim)
			ExpectExists(ctx, env.Client, replacementNode)
			ExpectExists(ctx, env.Client, workload)
			ExpectNotFound(ctx, env.Client, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: cmd.Replacements[1].Name}})
			replacementNode = ExpectNodeExists(ctx, env.Client, replacementNode.Name)
			Expect(replacementNode.Spec.Taints).ToNot(ContainElement(v1.DisruptedNoScheduleTaint))
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1.DisruptedNoScheduleTaint))
		})
		It("should keep upstream two-replacement behavior when the feature gate is disabled", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := multiReplacementCommand(queue, stateNode)
			Expect(queue.StartCommand(ctx, cmd)).To(Succeed())

			// Only the NodeClaims are initialized. Neither replacement has a backing Node in cluster state, so the
			// gated Ready barrier would block here if it applied to gate-disabled commands.
			for _, replacement := range cmd.Replacements {
				replacementNodeClaim := &v1.NodeClaim{}
				Expect(env.Client.Get(ctx, types.NamespacedName{Name: replacement.Name}, replacementNodeClaim)).To(Succeed())
				ExpectMakeNodeClaimsInitialized(ctx, env.Client, env.Clock, replacementNodeClaim)
			}

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(queue.IsEmpty()).To(BeTrue())
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should retry rather than fail when a replacement has no backing Node in cluster state", func() {
			enableMultiReplacement()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := multiReplacementCommand(queue, stateNode)
			Expect(queue.StartCommand(ctx, cmd)).To(Succeed())

			// Both NodeClaims report Initialized, but the node informer has not observed a backing Node yet, so
			// the StateNode either doesn't exist or has a nil Node.
			for _, replacement := range cmd.Replacements {
				replacementNodeClaim := &v1.NodeClaim{}
				Expect(env.Client.Get(ctx, types.NamespacedName{Name: replacement.Name}, replacementNodeClaim)).To(Succeed())
				ExpectMakeNodeClaimsInitialized(ctx, env.Client, env.Clock, replacementNodeClaim)
				replacementNodeClaim = ExpectExists(ctx, env.Client, replacementNodeClaim)
				replacementNodeClaim.Status.ProviderID = test.RandomProviderID()
				cluster.UpdateNodeClaim(replacementNodeClaim)
				Expect(ExpectStateNodeExistsForNodeClaim(cluster, replacementNodeClaim).Node).To(BeNil())
			}

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			// The command is retried instead of being failed, and no source is deleted.
			Expect(queue.IsEmpty()).To(BeFalse())
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, node1)
		})
		It("should use the uncached reader for the final replacement barrier", func() {
			enableMultiReplacement()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			uncachedFailureQueue := disruption.NewQueue(env.Client, recorder, cluster, env.Clock, prov, &failNodeClaimReader{Reader: env.Client})
			cmd := multiReplacementCommand(uncachedFailureQueue, stateNode)
			Expect(uncachedFailureQueue.StartCommand(ctx, cmd)).To(Succeed())

			replacementNodeClaims := make([]*v1.NodeClaim, len(cmd.Replacements))
			replacementNodes := make([]*corev1.Node, len(cmd.Replacements))
			for i, replacement := range cmd.Replacements {
				replacementNodeClaims[i] = &v1.NodeClaim{}
				Expect(env.Client.Get(ctx, types.NamespacedName{Name: replacement.Name}, replacementNodeClaims[i])).To(Succeed())
				replacementNodeClaims[i], replacementNodes[i] = ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, replacementNodeClaims[i])
			}
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				replacementNodes, replacementNodeClaims)

			ExpectObjectReconciled(ctx, env.Client, uncachedFailureQueue, stateNode.NodeClaim)
			Expect(uncachedFailureQueue.IsEmpty()).To(BeFalse())
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, node1)
		})
		It("should clean up a replacement that was updated after creation", func() {
			enableMultiReplacement()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := multiReplacementCommand(queue, stateNode)
			Expect(queue.StartCommand(ctx, cmd)).To(Succeed())

			// Simulate the lifecycle controller writing to the replacement after creation. A creation-time
			// resourceVersion precondition would turn the cleanup delete into a swallowed conflict.
			staleReplacements := lo.Map(cmd.Replacements, func(replacement *disruption.Replacement, _ int) *v1.NodeClaim {
				replacementNodeClaim := &v1.NodeClaim{}
				Expect(env.Client.Get(ctx, types.NamespacedName{Name: replacement.Name}, replacementNodeClaim)).To(Succeed())
				stored := replacementNodeClaim.DeepCopy()
				replacementNodeClaim.Annotations = lo.Assign(replacementNodeClaim.Annotations, map[string]string{"test.karpenter.sh/mutated": "true"})
				Expect(env.Client.Patch(ctx, replacementNodeClaim, client.MergeFrom(stored))).To(Succeed())
				updated := ExpectExists(ctx, env.Client, replacementNodeClaim)
				Expect(updated.ResourceVersion).ToNot(Equal(stored.ResourceVersion))
				return updated
			})

			env.Clock.Step(11 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)

			Expect(queue.IsEmpty()).To(BeTrue())
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, node1)
			for _, staleReplacement := range staleReplacements {
				ExpectNotFound(ctx, env.Client, staleReplacement)
			}
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1.DisruptedNoScheduleTaint))
		})
		It("should not wait for replacements when none are needed", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements:      nil,
			}
			Expect(queue.StartCommand(ctx, cmd)).To(BeNil())

			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)

			terminatingEvents := disruptionevents.Terminating(node1, nodeClaim1, string(cmd.Reason()))
			Expect(recorder.DetectedEvent(terminatingEvents[0].Message)).To(BeTrue())
			Expect(recorder.DetectedEvent(terminatingEvents[1].Message)).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should finish two commands in order as replacements are initialized", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1, nodeClaim2, node2)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1, node2}, []*v1.NodeClaim{nodeClaim1, nodeClaim2})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			stateNode2 := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim2)

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			replacements := []*disruption.Replacement{{
				NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct},
			}}
			nct2 := scheduling.NewNodeClaimTemplate(nodePool)
			nct2.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			replacements2 := []*disruption.Replacement{{
				NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct2},
			}}

			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements:      replacements,
			}
			Expect(queue.StartCommand(ctx, cmd)).To(BeNil())
			cmd2 := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode2, NodePool: nodePool}},
				Replacements:      replacements2,
			}
			Expect(queue.StartCommand(ctx, cmd2)).To(BeNil())

			replacementNodeClaim1 := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd.Replacements[0].Name}, replacementNodeClaim1))
			replacementNodeClaim2 := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: cmd2.Replacements[0].Name}, replacementNodeClaim2))

			replacementNodeClaim1, replacementNode1 := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, replacementNodeClaim1)
			replacementNodeClaim2, replacementNode2 := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, replacementNodeClaim2)

			// Reconcile the first command and expect nothing to be initialized
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim1).Message)).To(BeTrue())
			Expect(cmd2.Replacements[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim2).Message)).To(BeTrue())

			// Make the first command's node initialized
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{replacementNode1}, []*v1.NodeClaim{replacementNodeClaim1})
			// Reconcile the second command and expect nothing to be initialized
			ExpectObjectReconciled(ctx, env.Client, queue, cmd2.Candidates[0].NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim1).Message)).To(BeTrue())
			Expect(cmd2.Replacements[0].Initialized).To(BeFalse())
			Expect(recorder.DetectedEvent(disruptionevents.WaitingOnReadiness(nodeClaim2).Message)).To(BeTrue())

			// Reconcile the first command and expect the replacement to be initialized
			ExpectObjectReconciled(ctx, env.Client, queue, cmd.Candidates[0].NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd2.Replacements[0].Initialized).To(BeFalse())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)

			// Make the second command's node initialized
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{replacementNode2}, []*v1.NodeClaim{replacementNodeClaim2})

			// Reconcile the second command and expect the replacement to be initialized
			ExpectObjectReconciled(ctx, env.Client, queue, cmd2.Candidates[0].NodeClaim)
			Expect(cmd.Replacements[0].Initialized).To(BeTrue())
			Expect(cmd2.Replacements[0].Initialized).To(BeTrue())

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim2)
			// And expect the nodeClaim and node to be deleted
			ExpectNotFound(ctx, env.Client, nodeClaim2, node2)
		})
		Context("CalculateRetryDuration", func() {
			DescribeTable("should calculate correct timeout based on queue length",
				func(numCommands int, expectedDuration time.Duration) {
					q := disruption.NewQueue(env.Client, recorder, cluster, env.Clock, prov, env.Client)
					q.Lock()
					for i := range numCommands {
						q.ProviderIDToCommand[strconv.Itoa(i)] = &disruption.Command{}
					}
					q.Unlock()
					actualDuration := q.GetMaxRetryDuration()
					Expect(actualDuration).To(Equal(expectedDuration))
				},
				Entry("very small queue - 100 commands", 100, 10*time.Minute),                  // max(100*80ms, 10min) = 10min
				Entry("small queue - 4000 commands", 4000, 10*time.Minute),                     // max(4000*80ms, 10min) = 10min
				Entry("medium queue - 10000 commands", 10000, 13*time.Minute+20*time.Second),   // 10000*80ms = 13min 20sec
				Entry("large queue - 40000 commands", 40000, 53*time.Minute+20*time.Second),    // 40000*80ms = 53min 20sec
				Entry("very large queue - 80000 commands (capped)", 80000, 1*time.Hour),        // min(80000*80ms, 1hr) = 1hr
				Entry("extremely large queue - 100000 commands (capped)", 100000, 1*time.Hour), // min(100000*80ms, 1hr) = 1hr
			)
		})
	})
	Context("StartCommand", func() {
		It("should clean up a partial replacement launch and unmark source nodes", func() {
			enableMultiReplacement()
			node1.Spec.Taints = lo.Reject(node1.Spec.Taints, func(taint corev1.Taint, _ int) bool {
				return taint.MatchTaint(&v1.DisruptedNoScheduleTaint)
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			failingClient := &failNthNodeClaimCreateClient{Client: env.Client, failAt: 2}
			failingProvisioner := provisioning.NewProvisioner(failingClient, recorder, cloudProvider, cluster, env.Clock, deviceallocation.NewController(failingClient))
			failingQueue := disruption.NewQueue(failingClient, recorder, cluster, env.Clock, failingProvisioner, env.Client)

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			nct2 := scheduling.NewNodeClaimTemplate(nodePool)
			nct2.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			cmd := &disruption.Command{
				Method: disruption.NewMultiNodeConsolidation(
					disruption.MakeConsolidation(env.Clock, cluster, failingClient, failingProvisioner, cloudProvider, recorder, failingQueue),
				),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements: []*disruption.Replacement{
					{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct}},
					{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct2}},
				},
			}

			Expect(failingQueue.StartCommand(ctx, cmd)).ToNot(Succeed())
			Expect(failingQueue.IsEmpty()).To(BeTrue())
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, node1)
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1.DisruptedNoScheduleTaint))

			successfulNames := lo.Compact(lo.Map(cmd.Replacements, func(replacement *disruption.Replacement, _ int) string {
				return replacement.Name
			}))
			Expect(successfulNames).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: successfulNames[0]}})
		})
		It("should surface a replacement cleanup failure instead of swallowing it", func() {
			enableMultiReplacement()
			node1.Spec.Taints = lo.Reject(node1.Spec.Taints, func(taint corev1.Taint, _ int) bool {
				return taint.MatchTaint(&v1.DisruptedNoScheduleTaint)
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			failingClient := &conflictNodeClaimDeleteClient{Client: &failNthNodeClaimCreateClient{Client: env.Client, failAt: 2}}
			failingProvisioner := provisioning.NewProvisioner(failingClient, recorder, cloudProvider, cluster, env.Clock, deviceallocation.NewController(failingClient))
			failingQueue := disruption.NewQueue(failingClient, recorder, cluster, env.Clock, failingProvisioner, env.Client)

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			nct2 := scheduling.NewNodeClaimTemplate(nodePool)
			nct2.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			cmd := &disruption.Command{
				Method: disruption.NewMultiNodeConsolidation(
					disruption.MakeConsolidation(env.Clock, cluster, failingClient, failingProvisioner, cloudProvider, recorder, failingQueue),
				),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements: []*disruption.Replacement{
					{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct}},
					{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct2}},
				},
			}

			err := failingQueue.StartCommand(ctx, cmd)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("deleting unused replacement"))
			// The replacement that could not be deleted is still reported, and the source is still unmarked.
			successfulNames := lo.Compact(lo.Map(cmd.Replacements, func(replacement *disruption.Replacement, _ int) string {
				return replacement.Name
			}))
			Expect(successfulNames).To(HaveLen(1))
			ExpectExists(ctx, env.Client, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: successfulNames[0]}})
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1.DisruptedNoScheduleTaint))
		})
		It("should use the uncached reader when cleaning up a partial replacement launch", func() {
			enableMultiReplacement()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			failingClient := &failNthNodeClaimCreateClient{Client: env.Client, failAt: 2}
			failingProvisioner := provisioning.NewProvisioner(failingClient, recorder, cloudProvider, cluster, env.Clock, deviceallocation.NewController(failingClient))
			failingQueue := disruption.NewQueue(failingClient, recorder, cluster, env.Clock, failingProvisioner, &failNodeClaimReader{Reader: env.Client})

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			nct2 := scheduling.NewNodeClaimTemplate(nodePool)
			nct2.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			cmd := &disruption.Command{
				Method: disruption.NewMultiNodeConsolidation(
					disruption.MakeConsolidation(env.Clock, cluster, failingClient, failingProvisioner, cloudProvider, recorder, failingQueue),
				),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements: []*disruption.Replacement{
					{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct}},
					{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct2}},
				},
			}

			err := failingQueue.StartCommand(ctx, cmd)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("getting unused replacement"))
			successfulNames := lo.Compact(lo.Map(cmd.Replacements, func(replacement *disruption.Replacement, _ int) string {
				return replacement.Name
			}))
			Expect(successfulNames).To(HaveLen(1))
			ExpectExists(ctx, env.Client, &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: successfulNames[0]}})
		})
		It("should only roll back candidates that it tainted when marking partially fails", func() {
			node2.Spec.Taints = append(node2.Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1, nodeClaim2, node2)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{node1, node2}, []*v1.NodeClaim{nodeClaim1, nodeClaim2})
			stateNode1 := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)
			stateNode2 := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim2)

			// node2 is already tainted and carries a disruption condition owned by something else. Tainting it
			// fails, so this command must not clear that state while rolling back.
			failingClient := &failNodeTaintClient{Client: env.Client, nodeName: node2.Name}
			failingProvisioner := provisioning.NewProvisioner(failingClient, recorder, cloudProvider, cluster, env.Clock, deviceallocation.NewController(failingClient))
			failingQueue := disruption.NewQueue(failingClient, recorder, cluster, env.Clock, failingProvisioner, env.Client)

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, failingProvisioner, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates: []*disruption.Candidate{
					{StateNode: stateNode1, NodePool: nodePool},
					{StateNode: stateNode2, NodePool: nodePool},
				},
				Replacements: []*disruption.Replacement{
					{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct}},
				},
			}

			Expect(failingQueue.StartCommand(ctx, cmd)).ToNot(Succeed())
			Expect(failingQueue.IsEmpty()).To(BeTrue())
			// The candidate this command tainted is rolled back.
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).ToNot(ContainElement(v1.DisruptedNoScheduleTaint))
			Expect(ExpectExists(ctx, env.Client, nodeClaim1).StatusConditions().Get(v1.ConditionTypeDisruptionReason)).To(BeNil())
			// The candidate it never tainted keeps the state it already had.
			node2 = ExpectNodeExists(ctx, env.Client, node2.Name)
			Expect(node2.Spec.Taints).To(ContainElement(v1.DisruptedNoScheduleTaint))
		})
		It("should preserve a pre-existing disruption taint when condition marking fails", func() {
			node1.Spec.Taints = append(node1.Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			failingClient := &failNodeClaimGetClient{Client: env.Client, nodeClaimName: nodeClaim1.Name}
			failingProvisioner := provisioning.NewProvisioner(failingClient, recorder, cloudProvider, cluster, env.Clock, deviceallocation.NewController(failingClient))
			failingQueue := disruption.NewQueue(failingClient, recorder, cluster, env.Clock, failingProvisioner, env.Client)
			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, failingProvisioner, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements:      []*disruption.Replacement{{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct}}},
			}

			Expect(failingQueue.StartCommand(ctx, cmd)).ToNot(Succeed())
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).To(ContainElement(v1.DisruptedNoScheduleTaint))
		})
		It("should preserve a pre-existing disruption taint when a queued command times out", func() {
			node1.Spec.Taints = append(node1.Spec.Taints, v1.DisruptedNoScheduleTaint)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim1, node1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{node1}, []*v1.NodeClaim{nodeClaim1})
			stateNode := ExpectStateNodeExistsForNodeClaim(cluster, nodeClaim1)

			nct := scheduling.NewNodeClaimTemplate(nodePool)
			nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
			cmd := &disruption.Command{
				Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
				CreationTimestamp: env.Clock.Now(),
				ID:                uuid.New(),
				Results:           scheduling.Results{},
				Candidates:        []*disruption.Candidate{{StateNode: stateNode, NodePool: nodePool}},
				Replacements:      []*disruption.Replacement{{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct}}},
			}
			Expect(queue.StartCommand(ctx, cmd)).To(Succeed())

			env.Clock.Step(11 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, queue, stateNode.NodeClaim)
			Expect(queue.IsEmpty()).To(BeTrue())
			node1 = ExpectNodeExists(ctx, env.Client, node1.Name)
			Expect(node1.Spec.Taints).To(ContainElement(v1.DisruptedNoScheduleTaint))
			Expect(ExpectExists(ctx, env.Client, nodeClaim1).StatusConditions().Get(v1.ConditionTypeDisruptionReason)).To(BeNil())
		})
	})
})
