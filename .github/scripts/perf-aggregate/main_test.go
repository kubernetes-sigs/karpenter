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
	datas := []map[string]any{
		{"total_time": 2e9},
		{"total_time": 3e9},
		{"karpenter_cpu_nanos": 5e6},
	}
	tt := extractValues(datas, "total_time")
	if len(tt) != 2 || tt[0] != 2.0 || tt[1] != 3.0 {
		t.Errorf("total_time conversion: got %v, want [2 3]", tt)
	}
	cpu := extractValues(datas, "karpenter_cpu_nanos")
	if len(cpu) != 1 || cpu[0] != 5.0 {
		t.Errorf("karpenter_cpu_nanos conversion: got %v, want [5]", cpu)
	}
}

func TestRunEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	// Fabricate three iterations of two test suites. Values are chosen so
	// median/mean/min/max are trivially checkable in the assertions below.
	iters := 3
	for i := 1; i <= iters; i++ {
		iterDir := filepath.Join(tmp, "iter_"+itoa(i))
		if err := os.MkdirAll(iterDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Test A: total_time is > 1e9 to force the ns->s conversion path.
		// Values 2e9,3e9,4e9 -> 2,3,4 seconds; median = 3.
		writeReport(t, filepath.Join(iterDir, "test_a_performance_report.json"), map[string]any{
			"total_time":                        float64(i+1) * 1e9,
			"total_nodes":                       10 + i,
			"total_reserved_cpu_utilization":    0.5 + float64(i)*0.1,
			"resource_efficiency_score":         70 + float64(i),
			"total_reserved_memory_utilization": 0.6 + float64(i)*0.05,
			"rounds":                            i,
		})
		// Test B: karpenter_cpu_nanos = i*1e6 -> i ms; median = 2.
		writeReport(t, filepath.Join(iterDir, "test_b_performance_report.json"), map[string]any{
			"karpenter_cpu_nanos": float64(i) * 1e6,
			"total_nodes":         float64(20 + i),
		})
	}

	if err := run(tmp, iters, os.Stdout); err != nil {
		t.Fatalf("run: %v", err)
	}

	// benchmark-results-smaller.json should hold the smaller-is-better set.
	smaller := loadEntries(t, filepath.Join(tmp, "benchmark-results-smaller.json"))
	bigger := loadEntries(t, filepath.Join(tmp, "benchmark-results-bigger.json"))
	if len(smaller) == 0 {
		t.Fatal("expected at least one smaller-is-better entry")
	}
	if len(bigger) == 0 {
		t.Fatal("expected at least one bigger-is-better entry")
	}
	for _, e := range smaller {
		if contains(e.Name, "Utilization") || contains(e.Name, "Efficiency") {
			t.Errorf("smaller group leaked bigger-is-better metric: %s", e.Name)
		}
	}
	for _, e := range bigger {
		if !(contains(e.Name, "Utilization") || contains(e.Name, "Efficiency")) {
			t.Errorf("bigger group has non-utilization metric: %s", e.Name)
		}
	}

	// aggregated_summary.json should contain both tests with the expected duration median.
	var summary map[string]map[string]stats
	loadJSON(t, filepath.Join(tmp, "aggregated_summary.json"), &summary)
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
	if got := testB["Controller CPU"].Median; got != 2 {
		t.Errorf("test_b Controller CPU median (post ns->ms): got %v, want 2", got)
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
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
