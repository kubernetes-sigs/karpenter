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
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/pdb"
)

// Cluster.Snapshot() (and cow_regression_test.go's TestSnapshotIsolation_* tests) already prove, at the Cluster
// API level, that a snapshot held by a caller can't be mutated out from under it by later cluster writes.
// concurrent_simulation_test.go proves SimulateScheduling itself is race-safe under concurrent *reads*. Neither
// closes the actual integration gap: does a snapshot held across the *entire duration* of a real
// SimulateScheduling call -- which itself reads cluster state, and which runs for multiple milliseconds of real
// scheduling simulation -- stay stable even while unrelated cluster mutations and another concurrent
// SimulateScheduling call are actively running? This spec exercises exactly that, with a real SimulateScheduling
// call as one of the concurrent participants (not just synthetic Cluster API calls on both sides).
var _ = Describe("SimulateScheduling snapshot isolation", func() {
	It("should keep a snapshot held across a SimulateScheduling call stable despite concurrent mutations", func() {
		nodePool := test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)

		// One candidate node (what SimulateScheduling will be asked to consolidate) plus several "other" nodes
		// that aren't part of the simulation but are mutated concurrently while it runs.
		const numOtherNodes = 8
		nodeClaims, nodes := test.NodeClaimsAndNodes(1+numOtherNodes, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
					v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				},
			},
			Status: v1.NodeClaimStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:  resource.MustParse("32"),
					corev1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		for i := range nodeClaims {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		candidateNode, otherNodes := nodes[0], nodes[1:]

		pdbs, err := pdb.NewLimits(ctx, env.Client)
		Expect(err).To(Succeed())
		nodePoolMap, nodePoolToInstanceTypesMap, err := disruption.BuildNodePoolMap(ctx, env.Client, cloudProvider)
		Expect(err).To(Succeed())
		candidateStateNode := ExpectStateNodeExists(cluster, candidateNode)
		candidate, err := disruption.NewCandidate(ctx, env.Client, recorder, env.Clock, candidateStateNode, pdbs, nodePoolMap, nodePoolToInstanceTypesMap, queue, disruption.GracefulDisruptionClass)
		Expect(err).To(Succeed())

		// Held across the entire concurrent window below -- this is the invariant under test: a snapshot in the
		// caller's hand must not change no matter what else runs concurrently, including a real SimulateScheduling
		// call and a burst of unrelated mutations.
		preSnapshot := cluster.Snapshot()
		Expect(preSnapshot).To(HaveLen(1 + numOtherNodes))
		preRequests := make([]*resource.Quantity, len(preSnapshot))
		for i, n := range preSnapshot {
			requests := n.PodRequests()
			preRequests[i] = requests.Cpu()
		}

		var wg sync.WaitGroup
		var simErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, simErr = disruption.SimulateScheduling(ctx, env.Client, cluster, prov, env.Clock, recorder, nil, candidate)
		}()

		// Concurrently mutate cluster state on the "other" nodes -- none of them are the candidate, but every one
		// of these bumps Cluster.generation, which is exactly the condition that would corrupt preSnapshot if
		// Snapshot()'s copy-on-write isolation didn't hold.
		for i, n := range otherNodes {
			wg.Add(1)
			go func(idx int, node *corev1.Node) {
				defer wg.Done()
				pod := test.Pod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("isolation-churn-%d", idx), Namespace: "default"},
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				})
				pod.Spec.NodeName = node.Name
				_ = cluster.UpdatePod(ctx, pod)
				cluster.MarkForDeletion(node.Spec.ProviderID)
				cluster.UnmarkForDeletion(node.Spec.ProviderID)
			}(i, n)
		}
		wg.Wait()

		Expect(simErr).To(Succeed())

		// The snapshot captured before any of this concurrent activity started must be exactly as it was --
		// unaffected by the mutations, and unaffected by SimulateScheduling's own internal (separately-taken)
		// Snapshot() call.
		Expect(preSnapshot).To(HaveLen(1 + numOtherNodes))
		for i, n := range preSnapshot {
			requests := n.PodRequests()
			Expect(requests.Cpu().Cmp(*preRequests[i])).To(Equal(0), "node %d: snapshot held across SimulateScheduling must not observe concurrent mutations", i)
		}

		// A *fresh* snapshot taken now, by contrast, must observe the concurrent mutations -- confirming they
		// really happened (this isn't isolation working by accident because nothing actually changed).
		postSnapshot := cluster.Snapshot()
		sawMutation := false
		for _, n := range postSnapshot {
			requests := n.PodRequests()
			if n.Name() != candidateNode.Name && !requests.Cpu().IsZero() {
				sawMutation = true
				break
			}
		}
		Expect(sawMutation).To(BeTrue(), "expected the concurrent pod bindings to be visible in a snapshot taken after they completed")
	})
})
