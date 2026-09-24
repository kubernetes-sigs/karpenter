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

package deletioncost_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// The suite hands env.Clock to state.NewCluster, matching every other suite that
// builds a state.Cluster. Handing it its own clock.FakeClock instead still
// compiles and leaves most specs green, but it decouples the cluster's view of
// time from the env.Clock.Step calls the specs in this package depend on: the
// steps advance the test's clock while state-layer time reads stay frozen.
//
// Cluster.IsNodeNominated reads the cluster's own clock rather than one the
// caller passes in, which makes it the cheapest observable that tells the two
// wirings apart. Shared clock: the nomination expires on schedule. Second
// frozen clock: the node reports nominated forever.
var _ = Describe("Suite Clock Wiring", func() {
	It("should let env.Clock drive the cluster's own time reads", func() {
		nodePool := test.NodePool()
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status:     v1.NodeClaimStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock,
			nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})

		// nominationWindow is max(2*BatchMaxDuration, 10s), so 20s at the suite's
		// default BatchMaxDuration of 10s.
		cluster.NominateNodeForPod(ctx, node.Spec.ProviderID)

		// Guards the assertions below against passing vacuously: both
		// NominateNodeForPod and IsNodeNominated no-op when the providerID is
		// absent from cluster.nodes, and IsNodeNominated then returns false.
		Expect(cluster.IsNodeNominated(node.Spec.ProviderID)).To(BeTrue())

		env.Clock.Step(10 * time.Second)
		Expect(cluster.IsNodeNominated(node.Spec.ProviderID)).To(BeTrue(),
			"10s is inside the 20s nomination window")

		env.Clock.Step(11 * time.Second)
		Expect(cluster.IsNodeNominated(node.Spec.ProviderID)).To(BeFalse(),
			"21s is past the 20s nomination window; a cluster holding a second clock would still report nominated")
	})
})
