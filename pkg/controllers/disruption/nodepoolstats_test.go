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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("NodePoolStats", func() {
	var nodePool *v1.NodePool
	var nodeClaims []*v1.NodeClaim
	var nodes []*corev1.Node

	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaims, nodes = test.NodeClaimsAndNodes(5, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			}},
		})
		ExpectApplied(ctx, env.Client, nodePool)
		for i := range nodeClaims {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}
		// A mix of states, so the counts exercise every branch: three initialized, one of them marked for deletion and
		// one NotReady, plus one left uninitialized.
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes[:4], nodeClaims[:4])
		ExpectMakeNodesNotReady(ctx, env.Client, env.Clock, nodes[3])
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[3]))
		cluster.MarkForDeletion(nodes[2].Spec.ProviderID)
	})

	It("should count the same nodes as a snapshot of cluster state", func() {
		// NodePoolStats counts against cluster state under its read lock rather than against a copy of it, so it has
		// to agree with the snapshot-based form the pod deletion cost controller passes its own nodes to.
		for _, reason := range allKnownDisruptionReasons {
			numNodes, disrupting := disruption.NodePoolStats(cluster, reason)
			snapshotNumNodes, snapshotDisrupting := disruption.NodePoolStatsFromNodes(cluster.DeepCopyNodes(), reason)
			Expect(numNodes).To(Equal(snapshotNumNodes), "numNodes for reason %s", reason)
			Expect(disrupting).To(Equal(snapshotDisrupting), "disrupting for reason %s", reason)
		}
	})

	It("should count initialized nodes and the disruptions already in flight", func() {
		numNodes, disrupting := disruption.NodePoolStats(cluster, v1.DisruptionReasonDrifted)
		// Four of the five are initialized; the fifth never was.
		Expect(numNodes).To(HaveKeyWithValue(nodePool.Name, 4))
		// The node marked for deletion and the NotReady node each consume budget.
		Expect(disrupting).To(HaveKeyWithValue(nodePool.Name, 2))
	})

	It("should not count a NotReady node against the repair budget", func() {
		_, disrupting := disruption.NodePoolStats(cluster, v1.DisruptionReasonUnhealthy)
		// NotReady is repair's own trigger, so only the node marked for deletion counts.
		Expect(disrupting).To(HaveKeyWithValue(nodePool.Name, 1))
	})

	It("should leave cluster state unchanged", func() {
		// Counting against cluster state only reads it, so repeating the pass has to produce the same counts.
		numNodes, disrupting := disruption.NodePoolStats(cluster, v1.DisruptionReasonDrifted)
		again, againDisrupting := disruption.NodePoolStats(cluster, v1.DisruptionReasonDrifted)
		Expect(again).To(Equal(numNodes))
		Expect(againDisrupting).To(Equal(disrupting))
	})
})
