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

// Command perf-aggregate reduces one batch of performance samples into one
// statistic per (phase, metric) pair. One sample is one run of the performance
// suite, which writes one <phase>_performance_report.json per phase. It compares
// nothing and decides nothing: the exit code says whether the batch was read,
// not whether the numbers are acceptable.
//
// It walks OUTPUT_DIR rather than indexing known paths because the samples arrive
// as download-artifact directories whose names the uploader chooses. The package
// lives under .github, which Go's ./... does not reach, so the Makefile test
// target names the path explicitly.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const reportSuffix = "_performance_report.json"

const summaryFile = "aggregated_summary.json"

type metricSpec struct {
	jsonField string
	display   string
	unit      string
}

// resource_efficiency_score is deliberately absent. report.go computes it as
// 90*avgCPUUtil + 10*avgMemUtil, so it cannot move unless one of the two
// utilization fields below moves. The raw field stays in every uploaded sample.
var metrics = []metricSpec{
	{"total_time", "Duration", "seconds"},
	{"karpenter_p95_memory_mb", "Controller Peak Memory", "MB"},
	{"karpenter_p95_cpu_cores", "Controller CPU", "cores"},
	{"total_nodes", "Final Nodes", "nodes"},
	{"total_reserved_cpu_utilization", "CPU Utilization", "percent"},
	{"total_reserved_memory_utilization", "Memory Utilization", "percent"},
	{"rounds", "Consolidation Rounds", "rounds"},
}

type stats struct {
	N      int     `json:"n"`
	Unit   string  `json:"unit"`
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
	Stddev float64 `json:"stddev"`
	StdErr float64 `json:"stderr"`
	CVPct  float64 `json:"cv_pct"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

func main() {
	outputDir := os.Getenv("OUTPUT_DIR")
	if outputDir == "" {
		fmt.Fprintln(os.Stderr, "OUTPUT_DIR is required")
		os.Exit(2)
	}
	iterations := 0
	if v := os.Getenv("ITERATIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fmt.Fprintf(os.Stderr, "invalid ITERATIONS=%q: %v\n", v, err)
			os.Exit(2)
		}
		iterations = n
	}
	if err := run(outputDir, iterations, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Split from main so tests can drive it against a synthetic tree.
func run(outputDir string, iterations int, out io.Writer) error {
	reportsByPhase, files, err := collectReports(outputDir)
	if err != nil {
		return err
	}
	// Fail rather than write an empty summary and exit 0, which would let a
	// caller read "nothing ran" as "nothing to report".
	if len(reportsByPhase) == 0 {
		return fmt.Errorf("no %s files found under %s", reportSuffix, outputDir)
	}
	fmt.Fprintf(out, "Read %d sample report(s) across %d phase(s) under %s\n",
		files, len(reportsByPhase), outputDir)

	phases := make([]string, 0, len(reportsByPhase))
	for phase := range reportsByPhase {
		phases = append(phases, phase)
	}
	sort.Strings(phases)

	summary := map[string]map[string]stats{}
	for _, phase := range phases {
		samples := reportsByPhase[phase]
		phaseSummary := map[string]stats{}
		for _, m := range metrics {
			values := extractValues(samples, m.jsonField)
			if len(values) == 0 {
				continue
			}
			phaseSummary[m.display] = computeStats(values, m.unit)
		}
		summary[phase] = phaseSummary
		// Inequality, not shortfall: an over-count means two matrix legs wrote the
		// same report filename and this pooled their samples into one statistic.
		if iterations > 0 && len(samples) != iterations {
			fmt.Fprintf(out, "::warning title=Unexpected performance sample count::%s returned %d of %d dispatched samples. Its statistics are computed at n=%d.\n",
				phase, len(samples), iterations, len(samples))
		}
	}

	if err := writeJSON(filepath.Join(outputDir, summaryFile), summary); err != nil {
		return err
	}
	table := formatTable(phases, summary)
	fmt.Fprint(out, table)
	fmt.Fprintf(out, "\nWrote %s\n", filepath.Join(outputDir, summaryFile))
	return appendStepSummary(table)
}

// appendStepSummary fences the table, because the step summary renders as
// markdown and would otherwise collapse the columns. Done here rather than by
// piping the step's stdout through tee, which would swallow the exit code.
func appendStepSummary(table string) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600) //nolint:gosec // G304: path is the Actions-provided summary file
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "## Performance batch\n```\n%s```\n", table); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// A sample's reports share a directory, so one sample contributes at most one
// file per phase and the length of a phase's slice is its n.
func collectReports(root string) (map[string][]map[string]any, int, error) {
	reportsByPhase := map[string][]map[string]any{}
	files := 0
	//nolint:gosec // G703: root is OUTPUT_DIR, created by the workflow. No untrusted input, and the walk only reads.
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// WalkDir reports a missing root through this callback rather than
			// as a return value. Treat it as an empty batch, which is what it is
			// when every sample job failed and the download created nothing.
			if path == root && os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), reportSuffix) {
			return nil
		}
		data, readErr := readReport(path)
		if readErr != nil {
			// Lower n rather than end the batch, since the other samples are
			// still usable.
			fmt.Fprintf(os.Stderr, "warn: skipping %s: %v\n", path, readErr)
			return nil
		}
		phase := strings.TrimSuffix(d.Name(), reportSuffix)
		reportsByPhase[phase] = append(reportsByPhase[phase], data)
		files++
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("walking %s: %w", root, err)
	}
	return reportsByPhase, files, nil
}

func readReport(path string) (map[string]any, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path comes from a walk of the caller-supplied OUTPUT_DIR
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// A sample missing the field is skipped rather than counted as zero, so n
// stays the number of samples that measured the metric.
func extractValues(samples []map[string]any, jsonField string) []float64 {
	values := make([]float64, 0, len(samples))
	for _, s := range samples {
		raw, ok := s[jsonField]
		if !ok || raw == nil {
			continue
		}
		v, ok := raw.(float64)
		if !ok {
			continue
		}
		// total_time is a time.Duration (test/suites/performance/types.go), which
		// marshals as nanoseconds, so 1e9 always applies. Guarding the division
		// on magnitude would report a sub-second phase as a billion seconds.
		if jsonField == "total_time" {
			v /= 1e9
		}
		values = append(values, v)
	}
	return values
}

// Divides by n-1, not n: the samples are a draw from the population of runs a
// later comparison works against, not the population itself.
func computeStats(values []float64, unit string) stats {
	n := len(values)
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(n)

	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	median := sorted[n/2]
	if n%2 == 0 {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}

	stddev := 0.0
	if n > 1 {
		ss := 0.0
		for _, v := range values {
			ss += (v - mean) * (v - mean)
		}
		stddev = math.Sqrt(ss / float64(n-1))
	}
	stderr := stddev / math.Sqrt(float64(n))
	cv := 0.0
	if mean != 0 {
		cv = stddev / math.Abs(mean) * 100
	}

	return stats{
		N:      n,
		Unit:   unit,
		Mean:   round(mean, 4),
		Median: round(median, 4),
		Stddev: round(stddev, 4),
		StdErr: round(stderr, 4),
		CVPct:  round(cv, 1),
		Min:    round(sorted[0], 4),
		Max:    round(sorted[n-1], 4),
	}
}

func round(f float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(f*scale) / scale
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600) //nolint:gosec // G306: the file is CI output under the caller-supplied OUTPUT_DIR
}

func formatTable(phases []string, summary map[string]map[string]stats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-58s %3s %12s %12s %12s %12s %7s\n",
		"Phase / Metric", "n", "Median", "Mean", "Stddev", "StdErr", "CV")
	fmt.Fprintln(&b, strings.Repeat("-", 122))
	for _, phase := range phases {
		phaseSummary, ok := summary[phase]
		if !ok {
			continue
		}
		// Declared order, not map order, so two runs produce diffable tables.
		for _, m := range metrics {
			s, ok := phaseSummary[m.display]
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "%-58s %3d %12.3f %12.3f %12.3f %12.3f %6.1f%%\n",
				"  "+phase+" / "+m.display, s.N, s.Median, s.Mean, s.Stddev, s.StdErr, s.CVPct)
		}
	}
	return b.String()
}
