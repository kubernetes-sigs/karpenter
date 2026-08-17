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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// recordingMethod is a minimal disruption.Method that also implements disruption.NodesSetter, recording every
// snapshot the controller hands it. It never proposes a command, so it never triggers real disruption.
type recordingMethod struct {
	setNodesCalls []state.StateNodes
}

func (r *recordingMethod) ShouldDisrupt(_ context.Context, _ *disruption.Candidate) bool { return true }
func (r *recordingMethod) ComputeCommands(_ context.Context, _ map[string]int, _ ...*disruption.Candidate) ([]disruption.Command, error) {
	return nil, nil
}
func (r *recordingMethod) Reason() v1.DisruptionReason { return v1.DisruptionReasonUnderutilized }
func (r *recordingMethod) Class() string               { return disruption.GracefulDisruptionClass }
func (r *recordingMethod) ConsolidationType() string   { return "recording-method" }
func (r *recordingMethod) SetNodes(nodes state.StateNodes) {
	r.setNodesCalls = append(r.setNodesCalls, nodes)
}

var _ disruption.Method = (*recordingMethod)(nil)
var _ disruption.NodesSetter = (*recordingMethod)(nil)

var _ = Describe("NodesSetter wiring", func() {
	It("shares the reconcile-cycle snapshot with methods that implement NodesSetter", func() {
		nodePool := test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)

		nodeClaims, nodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
					v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0])
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		method := &recordingMethod{}
		c := disruption.NewController(env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(method))

		_, err := c.Reconcile(ctx)
		Expect(err).To(Succeed())

		// The controller must have handed the method a non-empty snapshot -- if the NodesSetter wiring were ever
		// dropped, this would silently be an empty/nil StateNodes, and every method would see zero existing nodes.
		Expect(method.setNodesCalls).ToNot(BeEmpty())
		Expect(method.setNodesCalls[0]).ToNot(BeEmpty())
		Expect(method.setNodesCalls[0]).To(HaveLen(1))
		Expect(method.setNodesCalls[0][0].Name()).To(Equal(nodes[0].Name))
	})
})
