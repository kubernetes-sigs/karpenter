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

package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LatencySidecar is the JSON schema written alongside a PerformanceReport
// when the LatencyHarness observed histogram or counter deltas. Offline
// analysis tools unmarshal into this type to pair a PerformanceReport with
// its latency companion; keep it stable across performance-suite tests so
// prior artifacts continue to load.
type LatencySidecar struct {
	TestName            string                    `json:"test_name"`
	ConsolidationPolicy string                    `json:"consolidation_policy"`
	Timestamp           time.Time                 `json:"timestamp"`
	LatencyStats        map[string]HistogramStats `json:"latency_stats,omitempty"`
	Counters            map[string]uint64         `json:"counters,omitempty"`
}

// WriteLatencySidecar writes sc to <dir>/<filePrefix>_latency.json. Returns
// nil (no-op) when dir is empty, matching the report.go artifact posture so
// suites can run without OUTPUT_DIR configured. filePrefix is sanitized via
// filepath.Base + filepath.Clean and the resolved path is checked to stay
// under dir before writing.
func WriteLatencySidecar(dir, filePrefix string, sc LatencySidecar) error {
	if dir == "" {
		return nil
	}
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal latency sidecar: %w", err)
	}
	safeDir := filepath.Clean(dir)
	safePrefix := filepath.Base(filepath.Clean(filePrefix))
	path := filepath.Join(safeDir, fmt.Sprintf("%s_latency.json", safePrefix))
	if rel, relErr := filepath.Rel(safeDir, path); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("latency sidecar path escapes %q", safeDir)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write latency sidecar %s: %w", path, err)
	}
	return nil
}
