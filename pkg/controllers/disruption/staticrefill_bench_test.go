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

package disruption

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// benchInstanceTypeCount approximates the AWS catalog a NodePool resolves to when it doesn't restrict instance types.
const benchInstanceTypeCount = 1300

// benchInstanceTypes returns count instance types with spot and on-demand offerings in four zones (8 offerings each, as
// for a typical AWS region). The first one also carries a full reservation "r-bench".
func benchInstanceTypes(count int) map[string]*cloudprovider.InstanceType {
	its := map[string]*cloudprovider.InstanceType{}
	for i := range count {
		var offerings cloudprovider.Offerings
		for _, zone := range []string{"test-zone-1", "test-zone-2", "test-zone-3", "test-zone-4"} {
			for _, ct := range []string{v1.CapacityTypeSpot, v1.CapacityTypeOnDemand} {
				offerings = append(offerings, &cloudprovider.Offering{Available: true, Price: 1, Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey: ct, corev1.LabelTopologyZone: zone,
				})})
			}
		}
		if i == 0 {
			offerings = append(offerings, &cloudprovider.Offering{Available: true, Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey: v1.CapacityTypeReserved, corev1.LabelTopologyZone: "test-zone-1", cloudprovider.ReservationIDLabel: "r-bench",
			})})
		}
		it := fake.NewInstanceType(fmt.Sprintf("bench-%04d", i), fake.WithOfferings(lo.FromSlicePtr(offerings)...))
		its[it.Name] = it
	}
	return its
}

func benchStaticPool(replicas int64, reqs ...v1.NodeSelectorRequirementWithMinValues) *v1.NodePool {
	np := test.StaticNodePool(v1.NodePool{Spec: v1.NodePoolSpec{
		Replicas: lo.ToPtr(replicas),
		// A zero node limit forces the at-limit (terminate-first) branch without populating cluster state.
		Limits:   v1.Limits{resources.Node: resource.MustParse("0")},
		Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{Requirements: reqs}},
	}})
	np.StatusConditions().SetTrue(v1.ConditionTypeValidationSucceeded)
	np.StatusConditions().SetTrue(v1.ConditionTypeNodeClassReady)
	np.StatusConditions().SetTrue(v1.ConditionTypeNodeRegistrationHealthy)
	return np
}

var (
	reservedOnly = []v1.NodeSelectorRequirementWithMinValues{
		{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"bench-0000"}},
		{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeReserved}},
	}
	reservedOnlyAnyType = []v1.NodeSelectorRequirementWithMinValues{
		{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeReserved}},
	}
	onDemandOnly = []v1.NodeSelectorRequirementWithMinValues{
		{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}},
	}
)

// BenchmarkStaticLaunchCapacity measures the per-NodePool refill-capacity evaluation over a ~1,300 type catalog. It is
// computed once per NodePool per pass, from instance types already resolved for the pass, so it never depends on node
// count.
func BenchmarkStaticLaunchCapacity(b *testing.B) {
	its := benchInstanceTypes(benchInstanceTypeCount)
	for _, tc := range []struct {
		name string
		reqs []v1.NodeSelectorRequirementWithMinValues
	}{
		{"reserved-only-pinned-type", reservedOnly},
		{"reserved-only-any-type", reservedOnlyAnyType},
		{"on-demand", onDemandOnly},
	} {
		np := benchStaticPool(1, tc.reqs...)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				pscheduling.NewLaunchCapacity(np, maps.Values(its))
			}
		})
	}
}

// BenchmarkStaticDriftTerminateFirst measures a full StaticDrift.ComputeCommands for one static NodePool at its node
// limit whose nodes are all drifted, at 10/1k/5k nodes, with a 10% budget.
//   - reservation-gone: reserved-only template, no reservation left (B5 after the provider drops it): nothing can be
//     refilled, so only budget-many candidates are touched.
//   - reservation-listed-full: the ended reservation is still listed (full) and the nodes were demoted: every
//     candidate is checked against the per-pool capacity (a map lookup each) and Blocked.
//   - on-demand: the pool can refill, so budget-many terminate-first commands are issued.
func BenchmarkStaticDriftTerminateFirst(b *testing.B) {
	ctx := options.ToContext(context.Background(), test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{TerminateFirstDrift: lo.ToPtr(true)}}))
	full := benchInstanceTypes(benchInstanceTypeCount)
	gone := maps.Clone(full)
	gone["bench-0000"] = fake.NewInstanceType("bench-0000", fake.WithOfferings(lo.FromSlicePtr(full["bench-0000"].Offerings[:8])...))
	for _, tc := range []struct {
		name         string
		reqs         []v1.NodeSelectorRequirementWithMinValues
		its          map[string]*cloudprovider.InstanceType
		capacityType string
		wantCmds     func(n int) int
	}{
		{"reservation-gone", reservedOnly, gone, v1.CapacityTypeOnDemand, func(int) int { return 0 }},
		{"reservation-listed-full", reservedOnly, full, v1.CapacityTypeOnDemand, func(int) int { return 0 }},
		{"on-demand", onDemandOnly, full, v1.CapacityTypeOnDemand, func(n int) int { return max(n/10, 1) }},
	} {
		for _, n := range []int{10, 1_000, 5_000} {
			b.Run(fmt.Sprintf("%s/nodes=%d", tc.name, n), func(b *testing.B) {
				np := benchStaticPool(int64(n), tc.reqs...)
				candidates := make([]*Candidate, n)
				for i := range candidates {
					nc := test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
						v1.NodePoolLabelKey: np.Name, v1.CapacityTypeLabelKey: tc.capacityType, corev1.LabelInstanceTypeStable: "bench-0000",
					}}})
					candidates[i] = &Candidate{
						StateNode:             &state.StateNode{NodeClaim: nc},
						NodePool:              np,
						capacityType:          tc.capacityType,
						nodePoolInstanceTypes: tc.its,
					}
				}
				cluster := state.NewCluster(clock.NewFakeClock(time.Now()), nil, nil)
				d := NewStaticDrift(cluster, nil, nil, test.NewEventRecorder())
				budgets := map[string]int{np.Name: max(n/10, 1)}
				cmds, err := d.ComputeCommands(ctx, budgets, candidates...)
				if err != nil || len(cmds) != tc.wantCmds(n) {
					b.Fatalf("got %d commands (err %v), want %d", len(cmds), err, tc.wantCmds(n))
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					_, _ = d.ComputeCommands(ctx, budgets, candidates...)
				}
			})
		}
	}
}
