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
	"errors"
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func TestFilterMultiNodeMultiReplacementByPrice(t *testing.T) { //nolint:gocyclo
	leftCheap := multiReplacementInstanceType("left-cheap", 3)
	leftExpensive := multiReplacementInstanceType("left-expensive", 6)
	rightCheap := multiReplacementInstanceType("right-cheap", 4)
	rightExpensive := multiReplacementInstanceType("right-expensive", 7)

	t.Run("filters both claims against one aggregate ceiling", func(t *testing.T) {
		nodeClaims := []*pscheduling.NodeClaim{
			multiReplacementNodeClaim("pool", leftCheap, leftExpensive),
			multiReplacementNodeClaim("pool", rightCheap, rightExpensive),
		}
		filtered, err := filterMultiNodeMultiReplacementByPrice(nodeClaims, 10)
		if err != nil {
			t.Fatalf("filtering replacements, %v", err)
		}
		if len(filtered[0].InstanceTypeOptions) != 1 || filtered[0].InstanceTypeOptions[0].Name != leftCheap.Name {
			t.Fatalf("expected only left cheap option, got %v", instanceTypeNames(filtered[0].InstanceTypeOptions))
		}
		if len(filtered[1].InstanceTypeOptions) != 1 || filtered[1].InstanceTypeOptions[0].Name != rightCheap.Name {
			t.Fatalf("expected only right cheap option, got %v", instanceTypeNames(filtered[1].InstanceTypeOptions))
		}
	})

	t.Run("rejects aggregate price equality", func(t *testing.T) {
		nodeClaims := []*pscheduling.NodeClaim{
			multiReplacementNodeClaim("pool", leftCheap),
			multiReplacementNodeClaim("pool", rightCheap),
		}
		_, err := filterMultiNodeMultiReplacementByPrice(nodeClaims, 7)
		if !errors.Is(err, errMultiNodeMultiReplacementPrice) {
			t.Fatalf("expected aggregate price rejection, got %v", err)
		}
	})

	t.Run("uses the worst compatible offering price", func(t *testing.T) {
		nodeClaims := []*pscheduling.NodeClaim{
			multiReplacementNodeClaim("pool", multiReplacementInstanceTypeWithPrices("left-flexible", 3, 8)),
			multiReplacementNodeClaim("pool", multiReplacementInstanceType("right", 1)),
		}
		_, err := filterMultiNodeMultiReplacementByPrice(nodeClaims, 9)
		if !errors.Is(err, errMultiNodeMultiReplacementPrice) {
			t.Fatalf("expected worst-offering aggregate price rejection, got %v", err)
		}
	})

	t.Run("rejects non-finite offering prices", func(t *testing.T) {
		nodeClaims := []*pscheduling.NodeClaim{
			multiReplacementNodeClaim("pool", multiReplacementInstanceType("invalid", math.NaN())),
			multiReplacementNodeClaim("pool", rightCheap),
		}
		_, err := filterMultiNodeMultiReplacementByPrice(nodeClaims, 10)
		if !errors.Is(err, errMultiNodeMultiReplacementPrice) {
			t.Fatalf("expected non-finite price rejection, got %v", err)
		}
	})

	t.Run("allows two replacements of the homogeneous source type when aggregate cost decreases", func(t *testing.T) {
		sourceType := multiReplacementInstanceType("source", 6)
		nodeClaims := []*pscheduling.NodeClaim{
			multiReplacementNodeClaim("pool", sourceType),
			multiReplacementNodeClaim("pool", sourceType),
		}
		filtered, err := filterMultiNodeMultiReplacementByPrice(nodeClaims, 18)
		if err != nil {
			t.Fatalf("expected aggregate source-type savings to be accepted, got %v", err)
		}
		if len(filtered[0].InstanceTypeOptions) != 1 || len(filtered[1].InstanceTypeOptions) != 1 {
			t.Fatalf("expected source instance type to remain available, got %v and %v",
				instanceTypeNames(filtered[0].InstanceTypeOptions),
				instanceTypeNames(filtered[1].InstanceTypeOptions),
			)
		}
	})
}

func TestReplacementPairsMatchUnordered(t *testing.T) {
	first := multiReplacementInstanceType("first", 3)
	second := multiReplacementInstanceType("second", 4)
	replacements := []*Replacement{
		{NodeClaim: multiReplacementNodeClaim("pool", first)},
		{NodeClaim: multiReplacementNodeClaim("pool", second)},
	}

	if !replacementPairsMatch(replacements, []*pscheduling.NodeClaim{
		multiReplacementNodeClaim("pool", second),
		multiReplacementNodeClaim("pool", first),
	}) {
		t.Fatal("expected reversed replacements to match as an unordered pair")
	}
	if replacementPairsMatch(replacements, []*pscheduling.NodeClaim{
		multiReplacementNodeClaim("pool", first),
		multiReplacementNodeClaim("pool", first),
	}) {
		t.Fatal("expected each replacement to match a distinct simulated claim")
	}

	changedTemplate := multiReplacementNodeClaim("pool", first)
	changedTemplate.Spec.Taints = []corev1.Taint{{Key: "changed", Effect: corev1.TaintEffectNoSchedule}}
	if replacementPairsMatch(replacements, []*pscheduling.NodeClaim{
		changedTemplate,
		multiReplacementNodeClaim("pool", second),
	}) {
		t.Fatal("expected a changed NodeClaim template to fail validation")
	}

	changedRequirements := multiReplacementNodeClaim("pool", first)
	changedRequirements.Requirements.Add(scheduling.NewRequirement("example.com/changed", corev1.NodeSelectorOpIn, "true"))
	if replacementPairsMatch(replacements, []*pscheduling.NodeClaim{
		changedRequirements,
		multiReplacementNodeClaim("pool", second),
	}) {
		t.Fatal("expected changed scheduling requirements to fail validation")
	}

	replacementsWithPods := []*Replacement{
		{NodeClaim: multiReplacementNodeClaim("pool", first)},
		{NodeClaim: multiReplacementNodeClaim("pool", second)},
	}
	replacementsWithPods[0].Pods = []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{UID: "pod-1"}}}
	simulatedFirst := multiReplacementNodeClaim("pool", first)
	simulatedFirst.Pods = []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{UID: "pod-2"}}}
	if replacementPairsMatch(replacementsWithPods, []*pscheduling.NodeClaim{
		simulatedFirst,
		multiReplacementNodeClaim("pool", second),
	}) {
		t.Fatal("expected changed pod grouping to fail validation")
	}
}

func TestValidateMultiNodeMultiReplacementBoundary(t *testing.T) {
	nodePool := &v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}}
	sourceInstanceType := multiReplacementInstanceType("source", 6)
	candidates := []*Candidate{
		{NodePool: nodePool, instanceType: sourceInstanceType, capacityType: v1.CapacityTypeOnDemand, zone: "test-zone-1"},
		{NodePool: nodePool, instanceType: sourceInstanceType, capacityType: v1.CapacityTypeOnDemand, zone: "test-zone-1"},
		{NodePool: nodePool, instanceType: sourceInstanceType, capacityType: v1.CapacityTypeOnDemand, zone: "test-zone-1"},
	}

	replacements := []*pscheduling.NodeClaim{
		multiReplacementNodeClaim(nodePool.Name, multiReplacementInstanceType("left", 3)),
		multiReplacementNodeClaim(nodePool.Name, multiReplacementInstanceType("right", 4)),
	}
	if err := validateMultiNodeMultiReplacementBoundary(candidates, replacements, true); err != nil {
		t.Fatalf("expected valid boundary, got %v", err)
	}

	t.Run("requires three sources", func(t *testing.T) {
		if err := validateMultiNodeMultiReplacementBoundary(candidates[:2], replacements, true); !errors.Is(err, errMultiNodeMultiReplacementIneligible) {
			t.Fatalf("expected source count rejection, got %v", err)
		}
	})
	t.Run("rejects more than three sources", func(t *testing.T) {
		tooMany := append(append([]*Candidate{}, candidates...), candidates[0])
		if err := validateMultiNodeMultiReplacementBoundary(tooMany, replacements, true); !errors.Is(err, errMultiNodeMultiReplacementIneligible) {
			t.Fatalf("expected upper source count rejection, got %v", err)
		}
	})
	t.Run("requires homogeneous zones", func(t *testing.T) {
		heterogeneous := append([]*Candidate{}, candidates...)
		copyOfThird := *heterogeneous[2]
		copyOfThird.zone = "test-zone-2"
		heterogeneous[2] = &copyOfThird
		if err := validateMultiNodeMultiReplacementBoundary(heterogeneous, replacements, true); !errors.Is(err, errMultiNodeMultiReplacementIneligible) {
			t.Fatalf("expected zone rejection, got %v", err)
		}
	})
	t.Run("requires on-demand sources", func(t *testing.T) {
		spot := append([]*Candidate{}, candidates...)
		copyOfThird := *spot[2]
		copyOfThird.capacityType = v1.CapacityTypeSpot
		spot[2] = &copyOfThird
		if err := validateMultiNodeMultiReplacementBoundary(spot, replacements, true); !errors.Is(err, errMultiNodeMultiReplacementIneligible) {
			t.Fatalf("expected capacity type rejection, got %v", err)
		}
	})
	t.Run("requires one replacement nodepool", func(t *testing.T) {
		crossPool := []*pscheduling.NodeClaim{replacements[0], multiReplacementNodeClaim("other-pool", multiReplacementInstanceType("right", 4))}
		if err := validateMultiNodeMultiReplacementBoundary(candidates, crossPool, true); !errors.Is(err, errMultiNodeMultiReplacementIneligible) {
			t.Fatalf("expected nodepool rejection, got %v", err)
		}
	})
}

func multiReplacementInstanceType(name string, price float64) *cloudprovider.InstanceType {
	return multiReplacementInstanceTypeWithPrices(name, price)
}

func multiReplacementInstanceTypeWithPrices(name string, prices ...float64) *cloudprovider.InstanceType {
	offerings := make(cloudprovider.Offerings, 0, len(prices))
	for _, price := range prices {
		offerings = append(offerings, &cloudprovider.Offering{
			Available: true,
			Price:     price,
			Requirements: scheduling.NewLabelRequirements(map[string]string{
				v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
				corev1.LabelTopologyZone: "test-zone-1",
			}),
		})
	}
	return &cloudprovider.InstanceType{
		Name:      name,
		Offerings: offerings,
	}
}

func multiReplacementNodeClaim(nodePoolName string, instanceTypes ...*cloudprovider.InstanceType) *pscheduling.NodeClaim {
	return &pscheduling.NodeClaim{
		NodeClaimTemplate: pscheduling.NodeClaimTemplate{
			NodePoolName:        nodePoolName,
			InstanceTypeOptions: instanceTypes,
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "test-zone-1"),
			),
		},
	}
}

func instanceTypeNames(instanceTypes cloudprovider.InstanceTypes) []string {
	names := make([]string, 0, len(instanceTypes))
	for _, instanceType := range instanceTypes {
		names = append(names, instanceType.Name)
	}
	return names
}
