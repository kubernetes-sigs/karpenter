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
	"fmt"
	"time"

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

type mutableObservationProvider struct {
	cloudprovider.CloudProvider
	observations map[string]cloudprovider.NodeClaimState
	err          error
	calls        int
}

func (p *mutableObservationProvider) RefreshNodeClaimStates(context.Context, []*v1.NodeClaim) (map[string]cloudprovider.NodeClaimState, error) {
	p.calls++
	return p.observations, p.err
}

var _ = Describe("Mutable running-node cost integration", func() {
	It("uses fresh coverage for candidates and pool totals before Kubernetes labels converge", func() {
		pool := test.NodePool()
		claim, node := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Labels:      map[string]string{v1.NodePoolLabelKey: pool.Name, corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name, corev1.LabelTopologyZone: mostExpensiveOffering.Zone(), v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand},
			Annotations: map[string]string{v1.CapacityTypeTransitionsAnnotationKey: "on-demand,reserved"},
		}})
		ExpectApplied(ctx, env.Client, pool, claim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{claim})
		cp := &mutableObservationProvider{CloudProvider: cloudProvider, observations: map[string]cloudprovider.NodeClaimState{}}
		for _, price := range []float64{1, .0000001, 1, 0} {
			cp.observations[claim.Status.ProviderID] = cloudprovider.NodeClaimState{ObservedAt: time.Now(), Scope: "group", Price: price, Labels: map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeReserved}}
			candidates, totals, err := disruption.GetCandidatesWithTotals(ctx, cluster, env.Client, recorder, env.Clock, cp, func(context.Context, *disruption.Candidate) bool { return true }, disruption.GracefulDisruptionClass, queue, clusterCost)
			Expect(err).ToNot(HaveOccurred())
			Expect(candidates).To(HaveLen(1))
			Expect(candidates[0].Price).To(Equal(price))
			Expect(totals[pool.Name].TotalCost).To(Equal(price))
			Expect(clusterCost.GetNodepoolCost(pool)).To(Equal(price))
			Expect(candidates[0].Labels()[v1.CapacityTypeLabelKey]).To(Equal(v1.CapacityTypeOnDemand))
		}
		// Failure does not hide candidates needed for drift or repair. The
		// consolidation predicate defers candidates whose pool cost is unknown.
		cp.err = fmt.Errorf("throttled")
		cp.observations = nil
		candidates, _, err := disruption.GetCandidatesWithTotals(ctx, cluster, env.Client, recorder, env.Clock, cp, func(context.Context, *disruption.Candidate) bool { return true }, disruption.GracefulDisruptionClass, queue, clusterCost)
		Expect(err).ToNot(HaveOccurred())
		Expect(candidates).To(HaveLen(1))
		c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cp, recorder, queue)
		Expect(disruption.NewSingleNodeConsolidation(c).ShouldDisrupt(ctx, candidates[0])).To(BeFalse())
		before := cp.calls
		_, err = disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cp, func(context.Context, *disruption.Candidate) bool { return true }, disruption.EventualDisruptionClass, queue)
		Expect(err).ToNot(HaveOccurred())
		Expect(cp.calls).To(Equal(before))
	})
})
