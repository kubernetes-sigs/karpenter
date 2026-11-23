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
	"context"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// countingCloudProvider counts GetInstanceTypes calls, which BuildNodePoolMap makes once per NodePool.
type countingCloudProvider struct {
	cloudprovider.CloudProvider

	mu                    sync.Mutex
	getInstanceTypesCalls int
}

func (c *countingCloudProvider) GetInstanceTypes(ctx context.Context, np *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	c.mu.Lock()
	c.getInstanceTypesCalls++
	c.mu.Unlock()
	return c.CloudProvider.GetInstanceTypes(ctx, np)
}

func (c *countingCloudProvider) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getInstanceTypesCalls
}

var _ = Describe("Candidate Set", func() {
	var nodePools []*v1.NodePool
	var nodeClaims []*v1.NodeClaim
	var nodes []*corev1.Node
	var countingCP *countingCloudProvider
	var controller *disruption.Controller

	const nodePoolCount = 3

	BeforeEach(func() {
		nodePools = nil
		nodeClaims = nil
		nodes = nil
		for range nodePoolCount {
			// Nothing here is disruptable: consolidation is off, and the nodes below are neither empty nor drifted. So
			// every method builds candidates and then filters them all away, which is the shape this set optimizes.
			nodePool := test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Disruption: v1.Disruption{
						ConsolidationPolicy: v1.ConsolidationPolicyWhenEmpty,
						ConsolidateAfter:    v1.MustParseNillableDuration("Never"),
					},
				},
			})
			nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{ProviderID: test.RandomProviderID()},
			})
			pod := test.Pod()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)
			nodePools = append(nodePools, nodePool)
			nodeClaims = append(nodeClaims, nodeClaim)
			nodes = append(nodes, node)
		}
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		countingCP = &countingCloudProvider{CloudProvider: cloudProvider}
		controller = disruption.NewController(ctx, env.Clock, env.Client, prov, countingCP, recorder, cluster, queue, clusterCost)
	})

	It("should resolve each NodePool's instance types once per disruption loop", func() {
		ExpectSingletonReconciled(ctx, controller)
		// One GetInstanceTypes per NodePool for the whole loop, not one per NodePool per method.
		Expect(countingCP.calls()).To(Equal(nodePoolCount))
	})
})
