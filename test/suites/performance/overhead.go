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

	. "github.com/onsi/ginkgo/v2"
)

// MemoryOverheadMB returns the additional memory overhead (in MB) configured via
// the KARPENTER_MEMORY_OVERHEAD_MB environment variable (defaults to 0).
// This value represents expected baseline memory overhead from daemonsets and
// other environment-specific components. It is reported in test output for
// observability but is NOT used to gate test pass/fail.
func MemoryOverheadMB() float64 {
	return envFloat64("KARPENTER_MEMORY_OVERHEAD_MB", 0)
}

// CPUOverheadNanos returns the additional CPU overhead (in nanoseconds) configured
// via the KARPENTER_CPU_OVERHEAD_NANOS environment variable (defaults to 0).
// This value represents expected baseline CPU overhead from environment-specific
// components. It is reported in test output for observability but is NOT used to
// gate test pass/fail.
func CPUOverheadNanos() float64 {
	return envFloat64("KARPENTER_CPU_OVERHEAD_NANOS", 0)
}

// ReportOverhead logs the configured overhead values to the test output.
// Call this at the beginning of performance test suites to record the
// environment-specific overhead configuration for later analysis.
func ReportOverhead() {
	memOverhead := MemoryOverheadMB()
	cpuOverhead := CPUOverheadNanos()
	GinkgoWriter.Printf("Configured memory overhead: %.2f MB\n", memOverhead)
	GinkgoWriter.Printf("Configured CPU overhead: %.0f ns\n", cpuOverhead)
}

func envFloat64(key string, defaultVal float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return defaultVal
}
