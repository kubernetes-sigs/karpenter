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
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Providers can override individual performance thresholds without patching the
// tests, via per-test-case JSON (KARPENTER_PERF_THRESHOLDS_FILE or inline
// KARPENTER_PERF_THRESHOLDS), keyed by "<testCase>/<phase>", e.g.
//   {"basic/scaleOut": {"memory_mb": 400, "cpu_cores": 1.2, "total_time_minutes": 3}}
// An override wins for the metric it sets; otherwise the inline base default is
// used.

// thresholdOverride is the per-test-case, per-metric absolute override. A nil
// field means "no override for this metric; use the inline base default".
type thresholdOverride struct {
	MemoryMB         *float64 `json:"memory_mb,omitempty"`          // peak memory upper bound, MB
	CPUCores         *float64 `json:"cpu_cores,omitempty"`          // avg CPU upper bound, cores
	TotalTimeMinutes *float64 `json:"total_time_minutes,omitempty"` // total time upper bound, minutes
	CPUUtil          *float64 `json:"cpu_util,omitempty"`           // reserved CPU utilization lower bound, fraction
	MemoryUtil       *float64 `json:"memory_util,omitempty"`        // reserved memory utilization lower bound, fraction
}

var (
	overridesOnce sync.Once
	overrides     map[string]thresholdOverride
	overridesErr  error
)

// loadOverrides parses the override table once. KARPENTER_PERF_THRESHOLDS_FILE
// (a path) takes precedence over KARPENTER_PERF_THRESHOLDS (inline JSON). When
// neither is set the table is empty and all thresholds use their defaults.
func loadOverrides() (map[string]thresholdOverride, error) {
	overridesOnce.Do(func() {
		var raw []byte
		if path, ok := os.LookupEnv("KARPENTER_PERF_THRESHOLDS_FILE"); ok && path != "" {
			raw, overridesErr = os.ReadFile(path)
			if overridesErr != nil {
				overridesErr = fmt.Errorf("reading KARPENTER_PERF_THRESHOLDS_FILE %q: %w", path, overridesErr)
				return
			}
		} else if inline, ok := os.LookupEnv("KARPENTER_PERF_THRESHOLDS"); ok && inline != "" {
			raw = []byte(inline)
		}
		if len(raw) == 0 {
			overrides = map[string]thresholdOverride{}
			return
		}
		if overridesErr = json.Unmarshal(raw, &overrides); overridesErr != nil {
			overridesErr = fmt.Errorf("parsing performance threshold overrides: %w", overridesErr)
		}
	})
	return overrides, overridesErr
}

// override returns the parsed override for a key. It panics on malformed
// configuration so a typo surfaces as an immediate suite failure rather than
// silently letting tests run against the wrong thresholds.
func override(key string) (thresholdOverride, bool) {
	table, err := loadOverrides()
	if err != nil {
		panic(err)
	}
	o, ok := table[key]
	return o, ok
}

func MemoryThreshold(key string, base float64) float64 {
	if o, ok := override(key); ok && o.MemoryMB != nil {
		return *o.MemoryMB
	}
	return base
}

func CPUThreshold(key string, base float64) float64 {
	if o, ok := override(key); ok && o.CPUCores != nil {
		return *o.CPUCores
	}
	return base
}

func TotalTimeThreshold(key string, base time.Duration) time.Duration {
	if o, ok := override(key); ok && o.TotalTimeMinutes != nil {
		return time.Duration(*o.TotalTimeMinutes * float64(time.Minute))
	}
	return base
}

func CPUUtilThreshold(key string, base float64) float64 {
	if o, ok := override(key); ok && o.CPUUtil != nil {
		return *o.CPUUtil
	}
	return base
}

func MemoryUtilThreshold(key string, base float64) float64 {
	if o, ok := override(key); ok && o.MemoryUtil != nil {
		return *o.MemoryUtil
	}
	return base
}
