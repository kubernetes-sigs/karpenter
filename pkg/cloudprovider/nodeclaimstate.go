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

package cloudprovider

import (
	"context"
	"math"
	"time"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// NodeClaimStateProvider is an optional extension for providers whose running-node
// cost changes independently of launch offerings. RefreshNodeClaimStates must read
// the provider, bypassing caches. Missing entries and entries with zero ObservedAt
// are unknown, never confirmed uncovered. Retain Scope on failed observations when
// possible so callers can defer affected peers. Results must not advertise launch slots.
// Implementations may include peers sharing capacity across NodePools.
type NodeClaimStateProvider interface {
	RefreshNodeClaimStates(context.Context, []*v1.NodeClaim) (map[string]NodeClaimState, error)
}

// NodeClaimState is a coherent observation of one running instance, keyed by provider ID.
// Price is the current marginal hourly scheduling cost, independently of launch price.
// Scope identifies shared capacity consumers; it is opaque and must not be a metric label.
type NodeClaimState struct {
	ObservedAt time.Time
	Price      float64
	Scope      string
	Labels     map[string]string
}

// ValidAt bounds the age of the observation, including time spent in a slow API request.
func (s NodeClaimState) ValidAt(now time.Time) bool {
	age := now.Sub(s.ObservedAt)
	return !s.ObservedAt.IsZero() && age >= 0 && age <= time.Minute &&
		!math.IsNaN(s.Price) && !math.IsInf(s.Price, 0) && s.Price >= 0 &&
		(s.Labels[v1.CapacityTypeLabelKey] == v1.CapacityTypeOnDemand || s.Labels[v1.CapacityTypeLabelKey] == v1.CapacityTypeReserved)
}

func RefreshNodeClaimStates(ctx context.Context, provider CloudProvider, claims []*v1.NodeClaim) (map[string]NodeClaimState, error) {
	if p, ok := provider.(NodeClaimStateProvider); ok {
		return p.RefreshNodeClaimStates(ctx, claims)
	}
	return nil, nil
}
