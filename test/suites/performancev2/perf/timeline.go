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

package perf

import (
	"cmp"
	"slices"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// roundGap splits NodeClaims into creation rounds; provisioning passes land about 10s apart.
const roundGap = 5 * time.Second

// OfferingPrice, if a provider captures it, is kept only for instance types in the node mix.
const OfferingPrice = "karpenter_cloudprovider_instance_type_offering_price_estimate"

// Timeline is read at the end of the phase, plus the NodeClaims the phase saw deleted.
type Timeline struct {
	NodeClaimRounds     int         `json:"nodeclaim_rounds"`
	LatePods            int         `json:"late_pods"`
	OffRound1Pods       int         `json:"off_round1_pods"`
	PreRegDupNodeClaims int         `json:"prereg_dup_nodeclaims"`
	PostRegNodeClaims   int         `json:"postreg_nodeclaims"`
	NodeMix             []NodeCount `json:"node_mix,omitempty"`

	PlanKept
}

type NodeCount struct {
	InstanceType string `json:"instance_type"`
	CapacityType string `json:"capacity_type"`
	Zone         string `json:"zone"`
	Count        int    `json:"count"`
}

// timeline counts NodeClaims and bound pods created since the phase started. NodeClaims created after the first
// registration are post-registration and their pods late; those after round 1 but before it are duplicates.
func timeline(since time.Time, pods []corev1.Pod, nodes []corev1.Node, nodeClaims []v1.NodeClaim) Timeline {
	since = since.Truncate(time.Second)
	ncs := lo.Filter(nodeClaims, func(nc v1.NodeClaim, _ int) bool { return !nc.CreationTimestamp.Time.Before(since) })
	slices.SortFunc(ncs, func(a, b v1.NodeClaim) int { return a.CreationTimestamp.Compare(b.CreationTimestamp.Time) })
	var registered time.Time
	for _, nc := range ncs {
		if c := nc.StatusConditions().Get(v1.ConditionTypeRegistered); c.IsTrue() && (registered.IsZero() || c.LastTransitionTime.Time.Before(registered)) {
			registered = c.LastTransitionTime.Time
		}
	}
	type placement struct {
		round int
		late  bool
	}
	t, onNode := Timeline{NodeMix: nodeMix(nodes)}, map[string]placement{}
	for i, nc := range ncs {
		if i == 0 || nc.CreationTimestamp.Sub(ncs[i-1].CreationTimestamp.Time) > roundGap {
			t.NodeClaimRounds++
		}
		late := !registered.IsZero() && !nc.CreationTimestamp.Time.Before(registered)
		switch {
		case late:
			t.PostRegNodeClaims++
		case t.NodeClaimRounds > 1:
			t.PreRegDupNodeClaims++
		}
		if nc.Status.NodeName != "" {
			onNode[nc.Status.NodeName] = placement{round: t.NodeClaimRounds, late: late}
		}
	}
	for _, p := range pods {
		if p.Spec.NodeName == "" || p.DeletionTimestamp != nil || p.CreationTimestamp.Time.Before(since) {
			continue
		}
		at, ok := onNode[p.Spec.NodeName]
		t.OffRound1Pods += lo.Ternary(!ok || at.round != 1, 1, 0)
		t.LatePods += lo.Ternary(at.late, 1, 0)
	}
	return t
}

func nodeMix(nodes []corev1.Node) []NodeCount {
	counts := lo.CountValuesBy(nodes, func(n corev1.Node) NodeCount {
		return NodeCount{InstanceType: n.Labels[corev1.LabelInstanceTypeStable], CapacityType: n.Labels[v1.CapacityTypeLabelKey], Zone: n.Labels[corev1.LabelTopologyZone]}
	})
	out := lo.MapToSlice(counts, func(k NodeCount, n int) NodeCount { k.Count = n; return k })
	slices.SortFunc(out, func(a, b NodeCount) int {
		return cmp.Or(cmp.Compare(a.InstanceType, b.InstanceType), cmp.Compare(a.CapacityType, b.CapacityType), cmp.Compare(a.Zone, b.Zone))
	})
	return out
}

func keepMixPrices(gauges map[string]float64, mix []NodeCount) {
	types := sets.New(lo.Map(mix, func(c NodeCount, _ int) string { return c.InstanceType })...)
	for k := range gauges {
		if name, l := parseSeriesKey(k); name == OfferingPrice && !types.Has(l["instance_type"]) {
			delete(gauges, k)
		}
	}
}
