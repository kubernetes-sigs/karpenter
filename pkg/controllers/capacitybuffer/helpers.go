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

package capacitybuffer

import (
	"math"

	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// calculateLimitReplicas determines how many buffer pods fit within the given
// resource budget. For each resource present in both limits and pod requests,
// it floors(limit/request). Returns the minimum across all matched resources.
// The bool indicates whether any resource overlapped; false means limits don't
// constrain anything.
func calculateLimitReplicas(limits v1.ResourceList, podSpec *v1.PodSpec) (int32, bool) {
	totalRequests := resources.RequestsForSpec(podSpec)
	if len(totalRequests) == 0 {
		return 0, false
	}

	minReplicas := int32(math.MaxInt32)
	matched := false
	for resourceName, limit := range limits {
		request, ok := totalRequests[resourceName]
		if !ok || request.IsZero() {
			continue
		}
		matched = true
		n := int64(math.Floor(float64(limit.MilliValue()) / float64(request.MilliValue())))
		clamped := int32(min(n, math.MaxInt32)) //nolint:gosec // clamped to MaxInt32
		if clamped < minReplicas {
			minReplicas = clamped
		}
	}

	if !matched {
		return 0, false
	}
	return minReplicas, true
}

// calculatePercentageReplicas computes ceil(scalableReplicas * percentage / 100)
// with a floor of 1 when both inputs are positive (the API guarantees at least
// one buffer chunk if the user asks for any percentage of a running workload).
func calculatePercentageReplicas(scalableReplicas int32, percentage int32) int32 {
	pctReplicas := int32(math.Ceil(float64(scalableReplicas) * float64(percentage) / 100.0))
	if pctReplicas < 1 && percentage > 0 && scalableReplicas > 0 {
		pctReplicas = 1
	}
	return pctReplicas
}
