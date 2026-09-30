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

// Guards the suite's choice to hand env.Clock to state.NewCluster. A second
// FakeClock still compiles but freezes state-layer time against env.Clock.Step.
// Cluster.IsNodeNominated reads the cluster's own clock, so it tells them apart.
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

		Expect(cluster.IsNodeNominated(node.Spec.ProviderID)).To(BeTrue())

		env.Clock.Step(10 * time.Second)
		Expect(cluster.IsNodeNominated(node.Spec.ProviderID)).To(BeTrue(),
			"10s is inside the 20s nomination window")

		env.Clock.Step(11 * time.Second)
		Expect(cluster.IsNodeNominated(node.Spec.ProviderID)).To(BeFalse(),
			"21s is past the 20s nomination window; a cluster holding a second clock would still report nominated")
	})
})
