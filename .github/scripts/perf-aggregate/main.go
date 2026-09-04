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

// Command perf-aggregate consumes per-iteration performance reports produced
// by the Karpenter e2e performance suite and emits benchmark-action inputs
// plus a detailed statistical summary.
//
// It reads OUTPUT_DIR (required) and ITERATIONS (default 1) from the
// environment, matching the invocation contract the workflow used for the
// prior Python implementation.
//
// Two benchmark-action files are emitted because github-action-benchmark
// treats direction (smaller-is-better vs bigger-is-better) as a per-tool,
// not per-metric, property. Utilization and efficiency metrics belong in the
// bigger-is-better group; latency/resource cost metrics belong in the
// smaller-is-better group.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type direction int

const (
	smallerIsBetter direction = iota
	biggerIsBetter
)

// metricSpec describes how to extract, label, and classify one field from a
// performance report.
type metricSpec struct {
	jsonField string
	display   string
	unit      string
	dir       direction
}

var metrics = []metricSpec{
	{"total_time", "Duration", "seconds", smallerIsBetter},
	{"karpenter_p95_memory_mb", "Controller Peak Memory", "MB", smallerIsBetter},
	{"karpenter_p95_cpu_cores", "Controller CPU", "cores", smallerIsBetter},
	{"total_nodes", "Final Nodes", "nodes", smallerIsBetter},
	{"total_reserved_cpu_utilization", "CPU Utilization", "percent", biggerIsBetter},
	{"resource_efficiency_score", "Efficiency Score", "score", biggerIsBetter},
	{"total_reserved_memory_utilization", "Memory Utilization", "percent", biggerIsBetter},
	{"rounds", "Consolidation Rounds", "rounds", smallerIsBetter},
}

type stats struct {
	N      int     `json:"n"`
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
	Stddev float64 `json:"stddev"`
	CVPct  float64 `json:"cv_pct"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

// benchmarkEntry matches the schema consumed by
// benchmark-action/github-action-benchmark for the customSmallerIsBetter
// and customBiggerIsBetter tools.
type benchmarkEntry struct {
	Name  string  `json:"name"`
	Unit  string  `json:"unit"`
	Value float64 `json:"value"`
	Range string  `json:"range,omitempty"`
	Extra string  `json:"extra,omitempty"`
}

func main() {
	outputDir := os.Getenv("OUTPUT_DIR")
	if outputDir == "" {
		fmt.Fprintln(os.Stderr, "OUTPUT_DIR is required")
		os.Exit(2)
	}
	iterations := 1
	if v := os.Getenv("ITERATIONS"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &iterations); err != nil || iterations < 1 {
			fmt.Fprintf(os.Stderr, "invalid ITERATIONS=%q: %v\n", v, err)
			os.Exit(2)
		}
	}
	if err := run(outputDir, iterations, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run performs the aggregation and returns nil on success. It is separated
// from main so tests can drive it with synthetic input.
func run(outputDir string, iterations int, out *os.File) error {
	reportsByTest, err := collectReports(outputDir, iterations)
	if err != nil {
		return err
	}

	// Iterate test keys in a stable order so the emitted arrays and the
	// printed table are reproducible across runs.
	testKeys := make([]string, 0, len(reportsByTest))
	for k := range reportsByTest {
		testKeys = append(testKeys, k)
	}
	sort.Strings(testKeys)

	summary, results := buildResults(testKeys, reportsByTest)

	if err := writeJSON(filepath.Join(outputDir, "benchmark-results-smaller.json"), results.smaller); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outputDir, "benchmark-results-bigger.json"), results.bigger); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outputDir, "benchmark-results-cv.json"), results.cv); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outputDir, "aggregated_summary.json"), summary); err != nil {
		return err
	}
	printTable(out, testKeys, summary)
	fmt.Fprintf(out, "\nEmitted %d smaller-is-better, %d bigger-is-better, %d CV metrics\n",
		len(results.smaller), len(results.bigger), len(results.cv))
	return nil
}

// benchmarkResults holds the three parallel benchmark-action arrays this
// aggregator emits: one gating file per direction plus one informational
// CV file.
type benchmarkResults struct {
	smaller []benchmarkEntry
	bigger  []benchmarkEntry
	cv      []benchmarkEntry
}

// buildResults computes stats for every (test, metric) pair and appends
// the corresponding gating and CV benchmark entries. It also returns the
// summary map keyed by test then metric display name.
func buildResults(testKeys []string, reportsByTest map[string][]map[string]any) (map[string]map[string]stats, benchmarkResults) {
	summary := map[string]map[string]stats{}
	var results benchmarkResults
	for _, testKey := range testKeys {
		datas := reportsByTest[testKey]
		if len(datas) == 0 {
			continue
		}
		name := prettifyTestName(testKey)
		testSummary := map[string]stats{}
		for _, m := range metrics {
			values := extractValues(datas, m.jsonField)
			if len(values) == 0 {
				continue
			}
			s := computeStats(values)
			testSummary[m.display] = s
			results.appendMetric(name, m, s)
		}
		summary[testKey] = testSummary
	}
	return summary, results
}

// appendMetric records both the gating (median-based) entry and the
// informational CV entry for a single (test, metric) pair. Gating entries
// route to smaller/bigger by metric direction; CV entries share a single
// smaller-is-better list because lower batch variance is always better.
func (r *benchmarkResults) appendMetric(name string, m metricSpec, s stats) {
	gate := benchmarkEntry{
		Name:  fmt.Sprintf("%s - %s (median, n=%d)", name, m.display, s.N),
		Unit:  m.unit,
		Value: s.Median,
		Range: fmt.Sprintf("%v", s.Stddev),
		Extra: fmt.Sprintf(
			"mean=%v stddev=%v cv=%v%% min=%v max=%v n=%d",
			s.Mean, s.Stddev, s.CVPct, s.Min, s.Max, s.N,
		),
	}
	if m.dir == biggerIsBetter {
		r.bigger = append(r.bigger, gate)
	} else {
		r.smaller = append(r.smaller, gate)
	}
	// CV entries feed an informational-only benchmark-action invocation
	// (fail-on-alert: false). They surface when a batch's within-batch
	// coefficient of variation grows beyond its historical envelope, so a
	// reviewer can notice noise-floor regressions without gating the
	// pipeline — the informational answer to Ryan's PR#2994 variance
	// question (comment 3857936029).
	r.cv = append(r.cv, benchmarkEntry{
		Name:  fmt.Sprintf("%s - %s (CV%%, n=%d)", name, m.display, s.N),
		Unit:  "cv-percent",
		Value: s.CVPct,
		Extra: fmt.Sprintf("median=%v stddev=%v n=%d", s.Median, s.Stddev, s.N),
	})
}

// collectReports loads every iter_N/*_performance_report.json under
// outputDir and groups the parsed maps by the file's basename, which stands
// in as the test-key across iterations.
func collectReports(outputDir string, iterations int) (map[string][]map[string]any, error) {
	reportsByTest := map[string][]map[string]any{}
	for i := 1; i <= iterations; i++ {
		pattern := filepath.Join(outputDir, fmt.Sprintf("iter_%d", i), "*_performance_report.json")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("glob %s: %w", pattern, err)
		}
		for _, path := range matches {
			data, err := readReport(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warn: skipping %s: %v\n", path, err)
				continue
			}
			testKey := filepath.Base(path)
			reportsByTest[testKey] = append(reportsByTest[testKey], data)
		}
	}
	return reportsByTest, nil
}

func readReport(path string) (map[string]any, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is scoped to CI-created OUTPUT_DIR
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func extractValues(datas []map[string]any, jsonField string) []float64 {
	values := make([]float64, 0, len(datas))
	for _, d := range datas {
		raw, ok := d[jsonField]
		if !ok || raw == nil {
			continue
		}
		v, ok := toFloat(raw)
		if !ok {
			continue
		}
		if jsonField == "total_time" && v > 1e9 {
			v = v / 1e9
		}
		values = append(values, v)
	}
	return values
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func computeStats(values []float64) stats {
	n := len(values)
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(n)
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	var median float64
	if n%2 == 1 {
		median = sorted[n/2]
	} else {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	variance := 0.0
	for _, v := range values {
		variance += (v - mean) * (v - mean)
	}
	variance /= float64(n)
	stddev := math.Sqrt(variance)
	cv := 0.0
	if mean > 0 {
		cv = stddev / mean * 100
	}
	return stats{
		N:      n,
		Mean:   round2(mean),
		Median: round2(median),
		Stddev: round2(stddev),
		CVPct:  round1(cv),
		Min:    round2(sorted[0]),
		Max:    round2(sorted[n-1]),
	}
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round1(f float64) float64 { return math.Round(f*10) / 10 }

func prettifyTestName(fileBasename string) string {
	base := strings.TrimSuffix(fileBasename, "_performance_report.json")
	words := strings.Split(base, "_")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// path is constructed from OUTPUT_DIR (CI-created via mktemp -d) plus a
	// literal filename. Not attacker-controlled.
	return os.WriteFile(path, b, 0o600) //nolint:gosec // G304: path is scoped to CI-created OUTPUT_DIR
}

func printTable(out *os.File, testKeys []string, summary map[string]map[string]stats) {
	fmt.Fprintf(out, "\n%-55s %3s %10s %10s %10s %6s\n",
		"Test / Metric", "n", "Median", "Mean", "Stddev", "CV")
	fmt.Fprintln(out, "----------------------------------------------------------------------------------------------------")
	for _, testKey := range testKeys {
		testName := strings.TrimSuffix(testKey, "_performance_report.json")
		testSummary, ok := summary[testKey]
		if !ok {
			continue
		}
		// Emit metrics in the canonical order defined by `metrics` so the
		// table matches the benchmark-action file ordering.
		for _, m := range metrics {
			s, ok := testSummary[m.display]
			if !ok {
				continue
			}
			label := fmt.Sprintf("  %s / %s", testName, m.display)
			fmt.Fprintf(out, "%-55s %3d %10.1f %10.1f %10.1f %5.1f%%\n",
				label, s.N, s.Median, s.Mean, s.Stddev, s.CVPct)
		}
	}
}
