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

package controller

import (
	"context"
	"math"

	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/pkg/operator/options"
)

// cpuCount calculates CPU count in cores from context options (in millicores)
func cpuCount(ctx context.Context) int {
	return int(math.Ceil(float64(options.FromContext(ctx).CPURequests) / 1000.0))
}

// LinearScaleReconciles calculates maxConcurrentReconciles using linear scaling
func LinearScaleReconciles(ctx context.Context, minReconciles int, maxReconciles int) int {
	cpuCount := cpuCount(ctx)
	// At 1 core: minReconciles; At 60 cores: maxReconciles
	slope := float64(maxReconciles-minReconciles) / 59.0
	result := int(slope*float64(cpuCount-1)) + minReconciles
	// Clamp to ensure we stay within bounds
	return lo.Clamp(result, minReconciles, maxReconciles)
}
