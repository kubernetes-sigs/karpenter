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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPerfAggregate(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Perf Aggregate Suite")
}

// sampleDir stands in for the directory download-artifact produces.
func writeSample(root, sampleDir, phase string, fields map[string]any) {
	dir := filepath.Join(root, sampleDir)
	Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
	b, err := json.Marshal(fields)
	Expect(err).ToNot(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(dir, phase+reportSuffix), b, 0o600)).To(Succeed())
}

var _ = Describe("Perf Aggregate", func() {
	Describe("computeStats", func() {
		It("reports n, median, mean, min and max", func() {
			got := computeStats([]float64{1, 2, 3, 4, 5}, "seconds")
			Expect(got.N).To(Equal(5))
			Expect(got.Median).To(Equal(3.0))
			Expect(got.Mean).To(Equal(3.0))
			Expect(got.Min).To(Equal(1.0))
			Expect(got.Max).To(Equal(5.0))
			Expect(got.Unit).To(Equal("seconds"))
		})

		It("uses the sample standard deviation, dividing by n-1", func() {
			// Sum of squared deviations of {1,2,3,4,5} about 3 is 10. Divided
			// by n-1 = 4 that is 2.5, so stddev is sqrt(2.5) = 1.5811. The
			// population form would divide by 5 and give sqrt(2) = 1.4142.
			got := computeStats([]float64{1, 2, 3, 4, 5}, "seconds")
			Expect(got.Stddev).To(BeNumerically("~", 1.5811, 0.0001))
			// StdErr is stddev over sqrt(n) = 1.5811/sqrt(5).
			Expect(got.StdErr).To(BeNumerically("~", 0.7071, 0.0001))
			// CV is stddev over mean, as a percentage.
			Expect(got.CVPct).To(BeNumerically("~", 52.7, 0.1))
		})

		It("reports zero spread for a single sample rather than dividing by zero", func() {
			got := computeStats([]float64{7}, "cores")
			Expect(got.N).To(Equal(1))
			Expect(got.Median).To(Equal(7.0))
			Expect(got.Stddev).To(Equal(0.0))
			Expect(got.StdErr).To(Equal(0.0))
			Expect(got.CVPct).To(Equal(0.0))
		})

		It("averages the two middle values for an even sample count", func() {
			Expect(computeStats([]float64{10, 20, 30, 40}, "nodes").Median).To(Equal(25.0))
		})
	})

	Describe("extractValues", func() {
		It("converts total_time from nanoseconds to seconds and leaves other fields alone", func() {
			samples := []map[string]any{
				{"total_time": 2e9, "karpenter_p95_cpu_cores": 0.5},
				{"total_time": 3e9, "karpenter_p95_cpu_cores": 0.75},
			}
			Expect(extractValues(samples, "total_time")).To(Equal([]float64{2.0, 3.0}))
			Expect(extractValues(samples, "karpenter_p95_cpu_cores")).To(Equal([]float64{0.5, 0.75}))
		})

		It("converts a sub-second total_time without a magnitude guard sending it to a billion", func() {
			samples := []map[string]any{{"total_time": 5e8}}
			Expect(extractValues(samples, "total_time")).To(Equal([]float64{0.5}))
		})

		It("skips a sample that is missing the field so n counts only measurements", func() {
			samples := []map[string]any{
				{"total_nodes": 4.0},
				{},
				{"total_nodes": nil},
				{"total_nodes": "not a number"},
				{"total_nodes": 6.0},
			}
			Expect(extractValues(samples, "total_nodes")).To(Equal([]float64{4.0, 6.0}))
		})
	})

	Describe("collectReports", func() {
		var root string
		BeforeEach(func() { root = GinkgoT().TempDir() })

		It("groups by phase across sample directories at any depth", func() {
			// Two samples that arrived as download-artifact directories, and a
			// third nested a level deeper, which is what upload-artifact
			// produces when the uploaded glob shares a root with other files.
			writeSample(root, "results-iter-1", "scale_out", map[string]any{"total_nodes": 4.0})
			writeSample(root, "results-iter-1", "consolidation", map[string]any{"total_nodes": 1.0})
			writeSample(root, "results-iter-2", "scale_out", map[string]any{"total_nodes": 5.0})
			writeSample(root, "results-iter-3/iter_3", "scale_out", map[string]any{"total_nodes": 6.0})

			byPhase, files, err := collectReports(root)
			Expect(err).ToNot(HaveOccurred())
			Expect(files).To(Equal(4))
			Expect(byPhase).To(HaveLen(2))
			Expect(byPhase["scale_out"]).To(HaveLen(3))
			Expect(byPhase["consolidation"]).To(HaveLen(1))
		})

		It("lowers n for an unreadable sample instead of failing the batch", func() {
			writeSample(root, "iter_1", "scale_out", map[string]any{"total_nodes": 4.0})
			Expect(os.WriteFile(filepath.Join(root, "iter_1", "broken"+reportSuffix), []byte("{"), 0o600)).To(Succeed())

			byPhase, files, err := collectReports(root)
			Expect(err).ToNot(HaveOccurred())
			Expect(files).To(Equal(1))
			Expect(byPhase["scale_out"]).To(HaveLen(1))
			Expect(byPhase).ToNot(HaveKey("broken"))
		})
	})

	Describe("run", func() {
		var root string
		BeforeEach(func() {
			root = GinkgoT().TempDir()
			// Cleared by default: these specs run under Actions during presubmit,
			// where the variable is set and appending would land in the real job
			// summary. Specs that want it set it themselves.
			GinkgoT().Setenv("GITHUB_STEP_SUMMARY", "")
		})

		It("writes one summary entry per phase and metric", func() {
			for i, nodes := range []float64{4, 5, 6} {
				writeSample(root, "iter_"+strings.Repeat("i", i+1), "scale_out", map[string]any{
					"total_nodes":                    nodes,
					"total_time":                     float64(i+1) * 1e9,
					"total_reserved_cpu_utilization": 50.0,
				})
			}

			Expect(run(root, 3, io.Discard)).To(Succeed())

			b, err := os.ReadFile(filepath.Join(root, summaryFile))
			Expect(err).ToNot(HaveOccurred())
			var summary map[string]map[string]stats
			Expect(json.Unmarshal(b, &summary)).To(Succeed())

			Expect(summary).To(HaveLen(1))
			Expect(summary["scale_out"]["Final Nodes"].N).To(Equal(3))
			Expect(summary["scale_out"]["Final Nodes"].Median).To(Equal(5.0))
			Expect(summary["scale_out"]["Duration"].Median).To(Equal(2.0))
			Expect(summary["scale_out"]["CPU Utilization"].Stddev).To(Equal(0.0))
			// A metric no sample carried gets no entry, rather than an entry
			// at n=0.
			Expect(summary["scale_out"]).ToNot(HaveKey("Controller CPU"))
		})

		It("omits resource_efficiency_score, which is a fixed combination of two metrics it already reports", func() {
			writeSample(root, "iter_1", "scale_out", map[string]any{
				"total_reserved_cpu_utilization":    60.0,
				"total_reserved_memory_utilization": 30.0,
				"resource_efficiency_score":         60.0*90 + 30.0*10,
			})

			Expect(run(root, 1, io.Discard)).To(Succeed())

			b, err := os.ReadFile(filepath.Join(root, summaryFile))
			Expect(err).ToNot(HaveOccurred())
			var summary map[string]map[string]stats
			Expect(json.Unmarshal(b, &summary)).To(Succeed())
			Expect(summary["scale_out"]).To(HaveKey("CPU Utilization"))
			Expect(summary["scale_out"]).To(HaveKey("Memory Utilization"))
			Expect(summary["scale_out"]).ToNot(HaveKey("Efficiency Score"))
		})

		It("reports a short batch in a warning and still summarizes it at the n it has", func() {
			writeSample(root, "iter_1", "scale_out", map[string]any{"total_nodes": 4.0})
			writeSample(root, "iter_2", "scale_out", map[string]any{"total_nodes": 6.0})

			var out strings.Builder
			Expect(run(root, 10, &out)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("::warning title=Unexpected performance sample count::"))
			Expect(out.String()).To(ContainSubstring("returned 2 of 10 dispatched samples"))

			b, err := os.ReadFile(filepath.Join(root, summaryFile))
			Expect(err).ToNot(HaveOccurred())
			var summary map[string]map[string]stats
			Expect(json.Unmarshal(b, &summary)).To(Succeed())
			Expect(summary["scale_out"]["Final Nodes"].N).To(Equal(2))
		})

		It("reports a phase that came back with more samples than were dispatched", func() {
			// Two matrix legs writing the same phase filename would have their
			// samples pooled into one statistic. The warning is what surfaces
			// it.
			writeSample(root, "iter_1", "scale_out", map[string]any{"total_nodes": 4.0})
			writeSample(root, "iter_2", "scale_out", map[string]any{"total_nodes": 6.0})

			var out strings.Builder
			Expect(run(root, 1, &out)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("returned 2 of 1 dispatched samples"))
		})

		It("stays quiet when every dispatched sample came back", func() {
			writeSample(root, "iter_1", "scale_out", map[string]any{"total_nodes": 4.0})
			var out strings.Builder
			Expect(run(root, 1, &out)).To(Succeed())
			Expect(out.String()).ToNot(ContainSubstring("::warning"))
		})

		It("fails when the batch is empty, so a caller cannot read no data as no news", func() {
			err := run(root, 3, io.Discard)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no " + reportSuffix + " files found"))
		})

		It("appends a fenced table to GITHUB_STEP_SUMMARY when Actions sets it", func() {
			writeSample(root, "iter_1", "scale_out", map[string]any{"total_nodes": 4.0})
			summaryPath := filepath.Join(root, "step-summary.md")
			GinkgoT().Setenv("GITHUB_STEP_SUMMARY", summaryPath)

			Expect(run(root, 1, io.Discard)).To(Succeed())

			b, err := os.ReadFile(summaryPath)
			Expect(err).ToNot(HaveOccurred())
			written := string(b)
			Expect(written).To(HavePrefix("## Performance batch\n```\n"))
			Expect(written).To(HaveSuffix("```\n"))
			Expect(written).To(ContainSubstring("scale_out / Final Nodes"))
		})

		It("skips the step summary when the variable is unset, so local runs write no stray file", func() {
			writeSample(root, "iter_1", "scale_out", map[string]any{"total_nodes": 4.0})
			GinkgoT().Setenv("GITHUB_STEP_SUMMARY", "")

			var out strings.Builder
			Expect(run(root, 1, &out)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("scale_out / Final Nodes"))
		})

		It("reports a missing directory as an empty batch, not as a walk error", func() {
			// What a run whose every sample job failed looks like: the download
			// step matched nothing and created no directory.
			err := run(filepath.Join(root, "never-created"), 3, io.Discard)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no " + reportSuffix + " files found"))
		})
	})
})
