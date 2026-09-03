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

package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestComputeStats(t *testing.T) {
	got := computeStats([]float64{1, 2, 3, 4, 5})
	if got.N != 5 {
		t.Errorf("n: got %d, want 5", got.N)
	}
	if got.Median != 3 {
		t.Errorf("median: got %v, want 3", got.Median)
	}
	if got.Mean != 3 {
		t.Errorf("mean: got %v, want 3", got.Mean)
	}
	if got.Min != 1 || got.Max != 5 {
		t.Errorf("min/max: got %v/%v, want 1/5", got.Min, got.Max)
	}
	// Population stddev of {1,2,3,4,5} == sqrt(2) ~= 1.414.
	if diff := math.Abs(got.Stddev - 1.41); diff > 0.01 {
		t.Errorf("stddev: got %v, want ~1.41", got.Stddev)
	}
	// CV = stddev/mean * 100 = 47.1%.
	if diff := math.Abs(got.CVPct - 47.1); diff > 0.1 {
		t.Errorf("cv: got %v, want ~47.1", got.CVPct)
	}
}

func TestComputeStatsEvenN(t *testing.T) {
	// Even-count median is the average of the two middle values.
	got := computeStats([]float64{10, 20, 30, 40})
	if got.Median != 25 {
		t.Errorf("median: got %v, want 25", got.Median)
	}
}

func TestExtractValuesUnitConversions(t *testing.T) {
	// total_time > 1e9 is interpreted as nanoseconds and converted to seconds.
	// karpenter_p95_cpu_cores is already in cores in types.go, so it passes
	// through unchanged.
	datas := []map[string]any{
		{"total_time": 2e9},
		{"total_time": 3e9},
		{"karpenter_p95_cpu_cores": 0.5},
	}
	tt := extractValues(datas, "total_time")
	if len(tt) != 2 || tt[0] != 2.0 || tt[1] != 3.0 {
		t.Errorf("total_time conversion: got %v, want [2 3]", tt)
	}
	cpu := extractValues(datas, "karpenter_p95_cpu_cores")
	if len(cpu) != 1 || cpu[0] != 0.5 {
		t.Errorf("karpenter_p95_cpu_cores passthrough: got %v, want [0.5]", cpu)
	}
}

func TestRunEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	iters := 3
	seedSyntheticIterations(t, tmp, iters)

	if err := run(tmp, iters, os.Stdout); err != nil {
		t.Fatalf("run: %v", err)
	}

	assertMetricGrouping(t, tmp)
	assertSummaryMedians(t, tmp)
	assertCVEntries(t, tmp)
}

// seedSyntheticIterations lays down iter_1..iter_N per-test performance
// reports whose values are chosen so median/mean/min/max are trivial to
// check in the assertions below.
func seedSyntheticIterations(t *testing.T, root string, iters int) {
	t.Helper()
	for i := 1; i <= iters; i++ {
		iterDir := filepath.Join(root, "iter_"+strconv.Itoa(i))
		if err := os.MkdirAll(iterDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Test A: total_time > 1e9 to force the ns->s conversion path.
		// Values 2e9,3e9,4e9 -> 2,3,4 seconds; median = 3.
		writeReport(t, filepath.Join(iterDir, "test_a_performance_report.json"), map[string]any{
			"total_time":                        float64(i+1) * 1e9,
			"total_nodes":                       10 + i,
			"total_reserved_cpu_utilization":    0.5 + float64(i)*0.1,
			"resource_efficiency_score":         70 + float64(i),
			"total_reserved_memory_utilization": 0.6 + float64(i)*0.05,
			"rounds":                            i,
		})
		// Test B: karpenter_p95_cpu_cores = i*0.1 -> 0.1, 0.2, 0.3; median = 0.2.
		// No conversion applied, types.go emits cores directly.
		writeReport(t, filepath.Join(iterDir, "test_b_performance_report.json"), map[string]any{
			"karpenter_p95_cpu_cores": float64(i) * 0.1,
			"total_nodes":             float64(20 + i),
		})
	}
}

// assertMetricGrouping verifies utilization / efficiency metrics land in the
// bigger-is-better file and latency / resource-cost metrics land in the
// smaller-is-better file. This is the metric-direction regression Ryan
// flagged on PR#2994.
func assertMetricGrouping(t *testing.T, dir string) {
	t.Helper()
	smaller := loadEntries(t, filepath.Join(dir, "benchmark-results-smaller.json"))
	bigger := loadEntries(t, filepath.Join(dir, "benchmark-results-bigger.json"))
	if len(smaller) == 0 {
		t.Fatal("expected at least one smaller-is-better entry")
	}
	if len(bigger) == 0 {
		t.Fatal("expected at least one bigger-is-better entry")
	}
	for _, e := range smaller {
		if strings.Contains(e.Name, "Utilization") || strings.Contains(e.Name, "Efficiency") {
			t.Errorf("smaller group leaked bigger-is-better metric: %s", e.Name)
		}
	}
	for _, e := range bigger {
		if !strings.Contains(e.Name, "Utilization") && !strings.Contains(e.Name, "Efficiency") {
			t.Errorf("bigger group has non-utilization metric: %s", e.Name)
		}
	}
}

func assertSummaryMedians(t *testing.T, dir string) {
	t.Helper()
	var summary map[string]map[string]stats
	loadJSON(t, filepath.Join(dir, "aggregated_summary.json"), &summary)
	testA := summary["test_a_performance_report.json"]
	if testA == nil {
		t.Fatal("summary missing test_a")
	}
	if got := testA["Duration"].Median; got != 3 {
		t.Errorf("test_a Duration median: got %v, want 3", got)
	}
	if got := testA["Efficiency Score"].Median; got != 72 {
		t.Errorf("test_a Efficiency median: got %v, want 72", got)
	}
	testB := summary["test_b_performance_report.json"]
	if testB == nil {
		t.Fatal("summary missing test_b")
	}
	if got := testB["Controller CPU"].Median; got != 0.2 {
		t.Errorf("test_b Controller CPU median (cores passthrough): got %v, want 0.2", got)
	}
}

func TestRunHandlesMissingIterationDir(t *testing.T) {
	// The test intentionally supplies no iter_* subdirs. run must succeed
	// and emit empty benchmark files rather than panic on nil maps.
	tmp := t.TempDir()
	if err := run(tmp, 5, os.Stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	entries := loadEntries(t, filepath.Join(tmp, "benchmark-results-smaller.json"))
	if len(entries) != 0 {
		t.Errorf("expected empty smaller results, got %d", len(entries))
	}
	// The CV file must also be present-but-empty so the informational
	// benchmark-action step doesn't fail on a missing input path.
	cvEntries := loadEntries(t, filepath.Join(tmp, "benchmark-results-cv.json"))
	if len(cvEntries) != 0 {
		t.Errorf("expected empty cv results, got %d", len(cvEntries))
	}
}

// assertCVEntries verifies the CV benchmark file is emitted, contains an
// entry for every metric-test combination in the summary, is tagged with
// the cv-percent unit, and embeds the batch median + stddev + n in Extra
// so a benchmark-action alert comment carries enough context to interpret.
func assertCVEntries(t *testing.T, dir string) {
	t.Helper()
	entries := loadEntries(t, filepath.Join(dir, "benchmark-results-cv.json"))
	if len(entries) == 0 {
		t.Fatal("expected at least one CV entry")
	}
	// Expected count: sum of metrics present across test_a and test_b in the
	// seed helper. Test A supplies Duration + Final Nodes + CPU Util +
	// Efficiency + Mem Util + Rounds = 6. Test B supplies Controller CPU
	// + Final Nodes = 2. Total = 8.
	if got, want := len(entries), 8; got != want {
		t.Errorf("cv entries: got %d, want %d", got, want)
	}
	for _, e := range entries {
		if e.Unit != "cv-percent" {
			t.Errorf("cv unit: got %q, want %q for entry %s", e.Unit, "cv-percent", e.Name)
		}
		if !strings.Contains(e.Name, "CV%") {
			t.Errorf("cv name missing CV%% marker: %s", e.Name)
		}
		if !strings.Contains(e.Extra, "median=") || !strings.Contains(e.Extra, "stddev=") {
			t.Errorf("cv extra missing median/stddev context: %s", e.Extra)
		}
	}
}

func TestPrettifyTestName(t *testing.T) {
	cases := map[string]string{
		"host_name_spreading_performance_report.json": "Host Name Spreading",
		"basic_deployment_performance_report.json":    "Basic Deployment",
	}
	for in, want := range cases {
		if got := prettifyTestName(in); got != want {
			t.Errorf("prettifyTestName(%q): got %q, want %q", in, got, want)
		}
	}
}

// --- helpers ---

func writeReport(t *testing.T, path string, data map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func loadEntries(t *testing.T, path string) []benchmarkEntry {
	t.Helper()
	var out []benchmarkEntry
	loadJSON(t, path, &out)
	return out
}

func loadJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled tempdir path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}
