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

// This spec exists specifically because of the SimulateScheduling deep-copy reduction: ExistingNode used to mutate
// its embedded *state.StateNode directly, which meant a StateNode snapshot could never safely be shared between
// concurrent simulations. Now that ExistingNode clones its own usage trackers, one shared snapshot should be safe
// to hand to any number of concurrent SimulateScheduling calls. Run with `-race` to verify.
var _ = Describe("Concurrent SimulateScheduling", func() {
	It("should not race when multiple goroutines simulate scheduling against one shared snapshot", func() {
		nodePool := test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)

		const numCandidates = 8
		nodeClaims, nodes := test.NodeClaimsAndNodes(numCandidates, v1.NodeClaim{
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
		for i := range numCandidates {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		pdbs, err := pdb.NewLimits(ctx, env.Client)
		Expect(err).To(Succeed())
		nodePoolMap, nodePoolToInstanceTypesMap, err := disruption.BuildNodePoolMap(ctx, env.Client, cloudProvider)
		Expect(err).To(Succeed())

		candidates := make([]*disruption.Candidate, numCandidates)
		for i := range numCandidates {
			stateNode := ExpectStateNodeExists(cluster, nodes[i])
			c, err := disruption.NewCandidate(ctx, env.Client, recorder, env.Clock, stateNode, pdbs, nodePoolMap, nodePoolToInstanceTypesMap, queue, disruption.GracefulDisruptionClass)
			Expect(err).To(Succeed())
			candidates[i] = c
		}

		// One shared snapshot, handed to every goroutine -- this is exactly the pattern the reconcile-cycle
		// snapshot enables: many concurrent simulations reading the same StateNodes without any of them mutating
		// the nodes the others are relying on.
		sharedNodes := cluster.DeepCopyNodes()

		var wg sync.WaitGroup
		errs := make([]error, numCandidates)
		for i := range numCandidates {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, err := disruption.SimulateScheduling(ctx, env.Client, cluster, prov, env.Clock, recorder, sharedNodes, nil, candidates[idx])
				errs[idx] = err
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			Expect(err).To(Succeed(), "candidate %d", i)
		}
	})
})
