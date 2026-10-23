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

var metrics = []metricSpec{
	{"total_time", "Duration", "seconds"},
	{"karpenter_p95_memory_mb", "Controller Peak Memory", "MB"},
	{"karpenter_p95_heap_mb", "Controller Peak Heap", "MB"},
	{"karpenter_p95_cpu_cores", "Controller CPU", "cores"},
	{"karpenter_avg_cpu_cores", "Controller Mean CPU", "cores"},
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
	StdErr float64 `json:"stderr_of_mean"`
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

func run(outputDir string, iterations int, out io.Writer) error {
	reportsByPhase, files, err := collectReports(outputDir)
	if err != nil {
		return err
	}
	if len(reportsByPhase) == 0 {
		return fmt.Errorf("no %s files found under %s", reportSuffix, outputDir)
	}
	fmt.Fprintf(out, "Read %d sample report(s) across %d phase(s) under %s\n",
		files, len(reportsByPhase), outputDir)
	if commit := os.Getenv("COMMIT"); commit != "" {
		fmt.Fprintf(out, "Commit under test: %s\n", commit)
	}

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
	return appendStepSummary(table, os.Getenv("COMMIT"))
}

func appendStepSummary(table, commit string) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	heading := "## Performance batch"
	if commit != "" {
		heading = fmt.Sprintf("## Performance batch at `%s`", commit)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600) //nolint:gosec // G304: path is the Actions-provided summary file
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s\n```\n%s```\n", heading, table); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func collectReports(root string) (map[string][]map[string]any, int, error) {
	reportsByPhase := map[string][]map[string]any{}
	files := 0
	//nolint:gosec // G703: root is OUTPUT_DIR, created by the workflow. No untrusted input, and the walk only reads.
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
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
		if jsonField == "total_time" {
			v /= 1e9
		}
		values = append(values, v)
	}
	return values
}

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
		"Phase / Metric", "n", "Median", "Mean", "Stddev", "SE of mean", "CV")
	fmt.Fprintln(&b, strings.Repeat("-", 122))
	for _, phase := range phases {
		phaseSummary, ok := summary[phase]
		if !ok {
			continue
		}
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
