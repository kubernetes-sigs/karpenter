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

package performance

import (
	"os"
	"strconv"
)

// MemoryOverheadMB returns the additional memory overhead (in MB) to add to
// Karpenter controller memory thresholds. This is read from the
// KARPENTER_MEMORY_OVERHEAD_MB environment variable and defaults to 0.
// This is useful for internal providers where daemonsets and other overhead
// cause the karpenter pod to consistently use more memory.
func MemoryOverheadMB() float64 {
	return envFloat64("KARPENTER_MEMORY_OVERHEAD_MB", 0)
}

// CPUOverheadNanos returns the additional CPU overhead (in nanoseconds) to add
// to Karpenter controller CPU thresholds. This is read from the
// KARPENTER_CPU_OVERHEAD_NANOS environment variable and defaults to 0.
func CPUOverheadNanos() float64 {
	return envFloat64("KARPENTER_CPU_OVERHEAD_NANOS", 0)
}

func envFloat64(key string, defaultVal float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return defaultVal
}
