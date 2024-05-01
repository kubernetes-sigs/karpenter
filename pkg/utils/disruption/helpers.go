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
	"math"
	"strconv"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// FilterByPriceWithMinValues returns the instanceTypes that are lower priced than the current candidate and iterates over the cumulative minimum requirement of the InstanceTypeOptions to see if it meets the minValues of requirements.
// The minValues requirement is checked again after filterByPrice as it may result in more constrained InstanceTypeOptions for a NodeClaim
func FilterByPriceWithMinValues(options []*cloudprovider.InstanceType, reqs scheduling.Requirements, price float64) ([]*cloudprovider.InstanceType, string, int) {
	var result []*cloudprovider.InstanceType

	for _, it := range options {
		launchPrice := WorstLaunchPrice(it.Offerings.Available(), reqs)
		if launchPrice < price {
			result = append(result, it)
		}
	}
	var incompatibleReqKey string
	var numInstanceTypes int
	// Only try to find the incompatible minValue requirement key if requirements have minValues.
	if reqs.HasMinValues() {
		// We would have already filtered the invalid nodeclaim not meeting the minimum requirements in simulated scheduling results.
		// Here the instanceTypeOptions changed again based on the price and requires re-validation.
		incompatibleReqKey, numInstanceTypes = pscheduling.IncompatibleReqAcrossInstanceTypes(reqs, lo.Slice(result, 0, pscheduling.MaxInstanceTypes))
	}
	// If minValues is NOT met for any of the requirement across InstanceTypes, then return empty InstanceTypeOptions as we cannot launch with the remaining InstanceTypes.
	result = lo.Ternary(len(incompatibleReqKey) > 0, []*cloudprovider.InstanceType{}, result)
	return result, incompatibleReqKey, numInstanceTypes
}

// WorstLaunchPrice gets the worst-case launch price from the offerings that are offered
// on an instance type. If the instance type has a spot offering available, then it uses the spot offering
// to get the launch price; else, it uses the on-demand launch price
func WorstLaunchPrice(ofs []cloudprovider.Offering, reqs scheduling.Requirements) float64 {
	// We prefer to launch spot offerings, so we will get the worst price based on the node requirements
	if reqs.Get(v1beta1.CapacityTypeLabelKey).Has(v1beta1.CapacityTypeSpot) {
		spotOfferings := lo.Filter(ofs, func(of cloudprovider.Offering, _ int) bool {
			return of.CapacityType == v1beta1.CapacityTypeSpot && reqs.Get(v1.LabelTopologyZone).Has(of.Zone)
		})
		if len(spotOfferings) > 0 {
			return lo.MaxBy(spotOfferings, func(of1, of2 cloudprovider.Offering) bool {
				return of1.Price > of2.Price
			}).Price
		}
	}
	if reqs.Get(v1beta1.CapacityTypeLabelKey).Has(v1beta1.CapacityTypeOnDemand) {
		onDemandOfferings := lo.Filter(ofs, func(of cloudprovider.Offering, _ int) bool {
			return of.CapacityType == v1beta1.CapacityTypeOnDemand && reqs.Get(v1.LabelTopologyZone).Has(of.Zone)
		})
		if len(onDemandOfferings) > 0 {
			return lo.MaxBy(onDemandOfferings, func(of1, of2 cloudprovider.Offering) bool {
				return of1.Price > of2.Price
			}).Price
		}
	}
	return math.MaxFloat64
}

// GetPodEvictionCost returns the disruption cost computed for evicting the given pod.
func GetPodEvictionCost(ctx context.Context, p *v1.Pod) float64 {
	cost := 1.0
	podDeletionCostStr, ok := p.Annotations[v1.PodDeletionCost]
	if ok {
		podDeletionCost, err := strconv.ParseFloat(podDeletionCostStr, 64)
		if err != nil {
			logging.FromContext(ctx).Errorf("parsing %s=%s from pod %s, %s",
				v1.PodDeletionCost, podDeletionCostStr, client.ObjectKeyFromObject(p), err)
		} else {
			// the pod deletion disruptionCost is in [-2147483647, 2147483647]
			// the min pod disruptionCost makes one pod ~ -15 pods, and the max pod disruptionCost to ~ 17 pods.
			cost += podDeletionCost / math.Pow(2, 27.0)
		}
	}
	// the scheduling priority is in [-2147483648, 1000000000]
	if p.Spec.Priority != nil {
		cost += float64(*p.Spec.Priority) / math.Pow(2, 25)
	}

	// overall we clamp the pod cost to the range [-10.0, 10.0] with the default being 1.0
	return lo.Clamp(cost, -10.0, 10.0)
}

func DisruptionCost(ctx context.Context, pods []*v1.Pod) float64 {
	cost := 0.0
	for _, p := range pods {
		cost += GetPodEvictionCost(ctx, p)
	}
	return cost
}

// LifetimeRemaining calculates the fraction of node lifetime remaining in the range [0.0, 1.0].  If the TTLSecondsUntilExpired
// is non-zero, we use it to scale down the disruption costs of candidates that are going to expire.  Just after creation, the
// disruption cost is highest, and it approaches zero as the node ages towards its expiration time.
func LifetimeRemaining(clock clock.Clock, nodePool *v1beta1.NodePool, node *v1.Node) float64 {
	remaining := 1.0
	if nodePool.Spec.Disruption.ExpireAfter.Duration != nil {
		ageInSeconds := clock.Since(node.CreationTimestamp.Time).Seconds()
		totalLifetimeSeconds := nodePool.Spec.Disruption.ExpireAfter.Duration.Seconds()
		lifetimeRemainingSeconds := totalLifetimeSeconds - ageInSeconds
		remaining = lo.Clamp(lifetimeRemainingSeconds/totalLifetimeSeconds, 0.0, 1.0)
	}
	return remaining
}
