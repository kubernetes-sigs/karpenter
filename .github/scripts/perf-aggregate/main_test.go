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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPerfAggregate(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Perf Aggregate Suite")
}

var _ = Describe("Perf Aggregate", func() {
	Describe("computeStats", func() {
		It("computes n/median/mean/min/max/stddev/cv for odd-count values", func() {
			got := computeStats([]float64{1, 2, 3, 4, 5})
			Expect(got.N).To(Equal(5))
			Expect(got.Median).To(Equal(3.0))
			Expect(got.Mean).To(Equal(3.0))
			Expect(got.Min).To(Equal(1.0))
			Expect(got.Max).To(Equal(5.0))
			// Population stddev of {1,2,3,4,5} == sqrt(2) ~= 1.414.
			Expect(got.Stddev).To(BeNumerically("~", 1.41, 0.01))
			// CV = stddev/mean * 100 = 47.1%.
			Expect(got.CVPct).To(BeNumerically("~", 47.1, 0.1))
		})

		It("averages the two middle values for even-count median", func() {
			got := computeStats([]float64{10, 20, 30, 40})
			Expect(got.Median).To(Equal(25.0))
		})
	})

	Describe("extractValues", func() {
		It("converts total_time > 1e9 from ns to seconds and passes cpu cores through unchanged", func() {
			// total_time > 1e9 is interpreted as nanoseconds and converted to
			// seconds. karpenter_p95_cpu_cores is already in cores in
			// types.go, so it passes through unchanged.
			datas := []map[string]any{
				{"total_time": 2e9},
				{"total_time": 3e9},
				{"karpenter_p95_cpu_cores": 0.5},
			}
			Expect(extractValues(datas, "total_time")).To(Equal([]float64{2.0, 3.0}))
			Expect(extractValues(datas, "karpenter_p95_cpu_cores")).To(Equal([]float64{0.5}))
		})
	})

	Describe("run end to end", func() {
		var tmp string

		BeforeEach(func() {
			var err error
			tmp, err = os.MkdirTemp("", "perf-aggregate-*")
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			Expect(os.RemoveAll(tmp)).To(Succeed())
		})

		Context("with synthetic per-iteration reports seeded across 3 iterations", func() {
			const iters = 3

			BeforeEach(func() {
				seedSyntheticIterations(tmp, iters)
				Expect(run(tmp, iters, os.Stdout)).To(Succeed())
			})

			It("routes utilization/efficiency into bigger-is-better, Controller CPU into smaller-loose, and other smaller metrics into smaller-tight", func() {
				smallerTight := loadEntries(filepath.Join(tmp, "benchmark-results-smaller-tight.json"))
				smallerLoose := loadEntries(filepath.Join(tmp, "benchmark-results-smaller-loose.json"))
				bigger := loadEntries(filepath.Join(tmp, "benchmark-results-bigger.json"))
				Expect(smallerTight).ToNot(BeEmpty(), "expected at least one smaller-tight entry")
				Expect(smallerLoose).ToNot(BeEmpty(), "expected at least one smaller-loose entry")
				Expect(bigger).ToNot(BeEmpty(), "expected at least one bigger-is-better entry")
				for _, e := range smallerTight {
					Expect(e.Name).ToNot(ContainSubstring("Utilization"), "smaller-tight group leaked bigger-is-better metric: %s", e.Name)
					Expect(e.Name).ToNot(ContainSubstring("Efficiency"), "smaller-tight group leaked bigger-is-better metric: %s", e.Name)
					Expect(e.Name).ToNot(ContainSubstring("Controller CPU"), "smaller-tight group leaked Controller CPU (should be loose): %s", e.Name)
					Expect(e.Name).ToNot(ContainSubstring("Consolidation Rounds"), "smaller-tight group leaked Consolidation Rounds (should be informational-only): %s", e.Name)
				}
				for _, e := range smallerLoose {
					Expect(e.Name).To(ContainSubstring("Controller CPU"), "smaller-loose group has non-CPU metric: %s", e.Name)
				}
				for _, e := range bigger {
					Expect(strings.Contains(e.Name, "Utilization") || strings.Contains(e.Name, "Efficiency")).To(BeTrue(), "bigger group has non-utilization metric: %s", e.Name)
				}
			})

			It("emits Consolidation Rounds into the CV file only, never into gate files", func() {
				smallerTight := loadEntries(filepath.Join(tmp, "benchmark-results-smaller-tight.json"))
				smallerLoose := loadEntries(filepath.Join(tmp, "benchmark-results-smaller-loose.json"))
				bigger := loadEntries(filepath.Join(tmp, "benchmark-results-bigger.json"))
				cv := loadEntries(filepath.Join(tmp, "benchmark-results-cv.json"))
				hasRoundsCV := false
				for _, e := range cv {
					if strings.Contains(e.Name, "Consolidation Rounds") {
						hasRoundsCV = true
						break
					}
				}
				Expect(hasRoundsCV).To(BeTrue(), "expected Consolidation Rounds in CV list")
				for _, e := range append(append(smallerTight, smallerLoose...), bigger...) {
					Expect(e.Name).ToNot(ContainSubstring("Consolidation Rounds"), "gate file leaked Consolidation Rounds: %s", e.Name)
				}
			})

			It("records the correct per-metric medians in aggregated_summary.json", func() {
				var summary map[string]map[string]stats
				loadJSON(filepath.Join(tmp, "aggregated_summary.json"), &summary)
				testA := summary["test_a_performance_report.json"]
				Expect(testA).ToNot(BeNil(), "summary missing test_a")
				Expect(testA["Duration"].Median).To(Equal(3.0))
				Expect(testA["Efficiency Score"].Median).To(Equal(72.0))
				testB := summary["test_b_performance_report.json"]
				Expect(testB).ToNot(BeNil(), "summary missing test_b")
				Expect(testB["Controller CPU"].Median).To(Equal(0.2))
			})

			It("emits one CV entry per (test, metric) tagged with cv-percent and embeds median/stddev/n in Extra", func() {
				entries := loadEntries(filepath.Join(tmp, "benchmark-results-cv.json"))
				Expect(entries).ToNot(BeEmpty(), "expected at least one CV entry")
				// Expected count: sum of metrics present across test_a and
				// test_b in the seed helper. Test A supplies Duration + Final
				// Nodes + CPU Util + Efficiency + Mem Util + Rounds = 6. Test
				// B supplies Controller CPU + Final Nodes = 2. Total = 8.
				Expect(entries).To(HaveLen(8))
				for _, e := range entries {
					Expect(e.Unit).To(Equal("cv-percent"))
					Expect(e.Name).To(ContainSubstring("CV%"))
					Expect(e.Extra).To(ContainSubstring("median="))
					Expect(e.Extra).To(ContainSubstring("stddev="))
				}
			})
		})

		Context("with no iter_* subdirs at all", func() {
			It("fails closed rather than emitting empty benchmark files", func() {
				err := run(tmp, 5, os.Stdout)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("no performance reports found"))
			})
		})
	})

	Describe("prettifyTestName", func() {
		DescribeTable("converts snake_case_performance_report.json filenames to Title Case",
			func(in, want string) {
				Expect(prettifyTestName(in)).To(Equal(want))
			},
			Entry("host name spreading", "host_name_spreading_performance_report.json", "Host Name Spreading"),
			Entry("basic deployment", "basic_deployment_performance_report.json", "Basic Deployment"),
		)
	})
})

// --- helpers ---

// seedSyntheticIterations lays down iter_1..iter_N per-test performance
// reports whose values are chosen so median/mean/min/max are trivial to
// check in the assertions above.
func seedSyntheticIterations(root string, iters int) {
	for i := 1; i <= iters; i++ {
		iterDir := filepath.Join(root, "iter_"+strconv.Itoa(i))
		Expect(os.MkdirAll(iterDir, 0o755)).To(Succeed())
		// Test A: total_time > 1e9 to force the ns->s conversion path.
		// Values 2e9,3e9,4e9 -> 2,3,4 seconds; median = 3.
		writeReport(filepath.Join(iterDir, "test_a_performance_report.json"), map[string]any{
			"total_time":                        float64(i+1) * 1e9,
			"total_nodes":                       10 + i,
			"total_reserved_cpu_utilization":    0.5 + float64(i)*0.1,
			"resource_efficiency_score":         70 + float64(i),
			"total_reserved_memory_utilization": 0.6 + float64(i)*0.05,
			"rounds":                            i,
		})
		// Test B: karpenter_p95_cpu_cores = i*0.1 -> 0.1, 0.2, 0.3; median = 0.2.
		// No conversion applied, types.go emits cores directly.
		writeReport(filepath.Join(iterDir, "test_b_performance_report.json"), map[string]any{
			"karpenter_p95_cpu_cores": float64(i) * 0.1,
			"total_nodes":             float64(20 + i),
		})
	}
}

func writeReport(path string, data map[string]any) {
	b, err := json.MarshalIndent(data, "", "  ")
	Expect(err).ToNot(HaveOccurred())
	Expect(os.WriteFile(path, b, 0o600)).To(Succeed())
}

func loadEntries(path string) []benchmarkEntry {
	var out []benchmarkEntry
	loadJSON(path, &out)
	return out
}

func loadJSON(path string, v any) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled tempdir path
	Expect(err).ToNot(HaveOccurred())
	Expect(json.Unmarshal(b, v)).To(Succeed())
}
