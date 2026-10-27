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

package cloudprovider_test

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// orderByPriceReference is the original OrderByPrice, which evaluates offering compatibility inside the comparator.
// OrderByPrice must produce exactly the same order, including among equally priced instance types.
func orderByPriceReference(its cloudprovider.InstanceTypes, reqs scheduling.Requirements) cloudprovider.InstanceTypes {
	sort.Slice(its, func(i, j int) bool {
		iPrice := math.MaxFloat64
		jPrice := math.MaxFloat64
		for _, of := range its[i].Offerings {
			if of.Launchable() && reqs.IsCompatible(of.Requirements, scheduling.AllowUndefinedWellKnownLabels) && of.Price < iPrice {
				iPrice = of.Price
			}
		}
		for _, of := range its[j].Offerings {
			if of.Launchable() && reqs.IsCompatible(of.Requirements, scheduling.AllowUndefinedWellKnownLabels) && of.Price < jPrice {
				jPrice = of.Price
			}
		}
		return iPrice < jPrice
	})
	return its
}

var (
	orderByPriceZones         = []string{"test-zone-1", "test-zone-2", "test-zone-3", "test-zone-4"}
	orderByPriceCapacityTypes = []string{v1.CapacityTypeOnDemand, v1.CapacityTypeSpot, v1.CapacityTypeReserved}
)

// randomInstanceTypes builds n instance types whose offerings vary in zone, capacity type, availability, reservation
// capacity and price. Prices are drawn from a small set (or, with fewPrices=false, a large one) so that ties are common,
// and some instance types end up with no launchable compatible offering at all.
func randomInstanceTypes(r *rand.Rand, n int, fewPrices bool) cloudprovider.InstanceTypes {
	its := make(cloudprovider.InstanceTypes, 0, n)
	for i := range n {
		var offerings cloudprovider.Offerings
		for range r.IntN(9) {
			price := float64(r.IntN(1000)) / 100
			if fewPrices {
				price = float64(r.IntN(4)) / 2
			}
			ct := orderByPriceCapacityTypes[r.IntN(len(orderByPriceCapacityTypes))]
			offerings = append(offerings, &cloudprovider.Offering{
				Available:           r.IntN(5) != 0,
				ReservationCapacity: r.IntN(2),
				Price:               price,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  ct,
					corev1.LabelTopologyZone: orderByPriceZones[r.IntN(len(orderByPriceZones))],
				}),
			})
		}
		its = append(its, &cloudprovider.InstanceType{
			Name:         fmt.Sprintf("it-%d", i),
			Requirements: scheduling.NewRequirements(),
			Offerings:    offerings,
		})
	}
	return its
}

func randomRequirements(r *rand.Rand) scheduling.Requirements {
	reqs := scheduling.NewRequirements()
	if r.IntN(3) != 0 {
		reqs.Add(scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, randomSubset(r, orderByPriceZones)...))
	}
	if r.IntN(3) != 0 {
		reqs.Add(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, randomSubset(r, orderByPriceCapacityTypes)...))
	}
	return reqs
}

// randomSubset returns a non-empty random subset of values.
func randomSubset(r *rand.Rand, values []string) []string {
	shuffled := append([]string{}, values...)
	r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	return shuffled[:1+r.IntN(len(shuffled))]
}

func instanceTypeNames(its cloudprovider.InstanceTypes) []string {
	return lo.Map(its, func(it *cloudprovider.InstanceType, _ int) string { return it.Name })
}

var _ = Describe("OrderByPrice", func() {
	It("orders exactly like the per-comparison implementation on randomized inputs", func() {
		// Sizes cover pdqsort's insertion-sort cutoff (12), its ninther pivot selection (>50) and realistic instance
		// type counts.
		sizes := []int{0, 1, 2, 3, 5, 8, 12, 13, 20, 33, 50, 51, 64, 100, 257, 600, 1000}
		for seed := uint64(0); seed < 400; seed++ {
			r := rand.New(rand.NewPCG(seed, 0x6f726465726279))
			n := sizes[int(seed)%len(sizes)]
			its := randomInstanceTypes(r, n, seed%2 == 0)
			reqs := randomRequirements(r)

			want := orderByPriceReference(append(cloudprovider.InstanceTypes{}, its...), reqs)
			input := append(cloudprovider.InstanceTypes{}, its...)
			got := input.OrderByPrice(reqs)

			Expect(instanceTypeNames(got)).To(Equal(instanceTypeNames(want)), "seed %d, n %d, reqs %s", seed, n, reqs)
			// OrderByPrice sorts in place and returns the receiver.
			if n > 0 {
				Expect(&got[0]).To(BeIdenticalTo(&input[0]))
			}
		}
	})
	It("orders instance types without a launchable compatible offering last", func() {
		reqs := scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "test-zone-1"))
		incompatible := &cloudprovider.InstanceType{Name: "incompatible", Offerings: cloudprovider.Offerings{{
			Available: true, Price: 0.1,
			Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-2"}),
		}}}
		none := &cloudprovider.InstanceType{Name: "none"}
		cheap := &cloudprovider.InstanceType{Name: "cheap", Offerings: cloudprovider.Offerings{onDemandOffering(1.0)}}
		got := cloudprovider.InstanceTypes{incompatible, none, cheap}.OrderByPrice(reqs)
		Expect(instanceTypeNames(got)).To(Equal([]string{"cheap", "incompatible", "none"}))
		Expect(instanceTypeNames(got)).To(Equal(instanceTypeNames(orderByPriceReference(cloudprovider.InstanceTypes{incompatible, none, cheap}, reqs))))
	})
})

// benchmarkInstanceTypes builds n instance types with an offering per zone and capacity type (8 per instance type), in
// a shuffled order, plus NodeClaim-like requirements that are compatible with a subset of those offerings.
func benchmarkInstanceTypes(n int) (cloudprovider.InstanceTypes, scheduling.Requirements) {
	r := rand.New(rand.NewPCG(uint64(n), 1))
	its := make(cloudprovider.InstanceTypes, 0, n)
	for i := range n {
		var offerings cloudprovider.Offerings
		base := float64(1+r.IntN(200)) / 10
		for _, zone := range orderByPriceZones {
			for _, ct := range []string{v1.CapacityTypeOnDemand, v1.CapacityTypeSpot} {
				price := base
				if ct == v1.CapacityTypeSpot {
					price = base * (0.3 + 0.4*r.Float64())
				}
				offerings = append(offerings, &cloudprovider.Offering{
					Available: true,
					Price:     price,
					Requirements: scheduling.NewLabelRequirements(map[string]string{
						v1.CapacityTypeLabelKey:  ct,
						corev1.LabelTopologyZone: zone,
					}),
				})
			}
		}
		its = append(its, &cloudprovider.InstanceType{
			Name:         fmt.Sprintf("it-%d", i),
			Requirements: scheduling.NewRequirements(),
			Offerings:    offerings,
		})
	}
	r.Shuffle(len(its), func(i, j int) { its[i], its[j] = its[j], its[i] })
	reqs := scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "test-zone-1", "test-zone-2", "test-zone-3"),
		scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
		scheduling.NewRequirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpIn, "default"),
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
	)
	return its, reqs
}

func BenchmarkOrderByPrice(b *testing.B) {
	for _, n := range []int{60, 400, 800} {
		b.Run(fmt.Sprintf("instanceTypes=%d", n), func(b *testing.B) {
			its, reqs := benchmarkInstanceTypes(n)
			buf := make(cloudprovider.InstanceTypes, len(its))
			b.ReportAllocs()
			for b.Loop() {
				copy(buf, its)
				buf.OrderByPrice(reqs)
			}
		})
	}
}
