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
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// thresholdCheck is one threshold assertion: the core default, the effective threshold after
// KARPENTER_PERF_THRESHOLDS, and the measured value.
type thresholdCheck struct {
	spec, key, metric, comparator      string
	coreThreshold, threshold, measured float64
	pass                               bool
}

func (c thresholdCheck) String() string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%g\t%g\t%.4f\t%t\n", c.spec, c.key, c.metric, c.comparator, c.coreThreshold, c.threshold, c.measured, c.pass)
}

// checks holds every threshold check of this Ginkgo process, in order.
var checks []thresholdCheck

func softThresholds() bool {
	soft, _ := strconv.ParseBool(os.Getenv("PERF_SOFT_THRESHOLDS"))
	return soft
}

// expectThreshold records a check, rewrites the results in OUTPUT_DIR and asserts the check. With
// PERF_SOFT_THRESHOLDS=true the assertion is deferred to expectNoBreaches so later phases still run.
func expectThreshold(key, metric, cmp string, measured, coreThreshold, threshold float64, desc string) {
	GinkgoHelper()
	checks = append(checks, thresholdCheck{spec: CurrentSpecReport().FullText(), key: key, metric: metric, comparator: cmp,
		coreThreshold: coreThreshold, threshold: threshold, measured: measured,
		pass: lo.Ternary(cmp == "<", measured < threshold, measured > threshold)})
	Expect(writeResults()).To(Succeed(), "writing performance thresholds to OUTPUT_DIR")
	if !softThresholds() {
		Expect(measured).To(BeNumerically(cmp, threshold), desc)
	}
}

func expectTotalTime(key string, r *PerformanceReport, base time.Duration, desc string) {
	GinkgoHelper()
	expectThreshold(key, "total_time_minutes", "<", r.TotalTime.Minutes(), base.Minutes(), TotalTimeThreshold(key, base).Minutes(), desc)
}

func expectCPUUtil(key string, r *PerformanceReport, base float64, desc string) {
	GinkgoHelper()
	expectThreshold(key, "cpu_util", ">", r.TotalReservedCPUUtil, base, CPUUtilThreshold(key, base), desc)
}

func expectMemoryUtil(key string, r *PerformanceReport, base float64, desc string) {
	GinkgoHelper()
	expectThreshold(key, "memory_util", ">", r.TotalReservedMemoryUtil, base, MemoryUtilThreshold(key, base), desc)
}

func expectHeap(key string, r *PerformanceReport, base float64, desc string) {
	GinkgoHelper()
	expectThreshold(key, "heap_mb", "<", r.KarpenterP95HeapMB, base, HeapThreshold(key, base), desc)
}

func expectCPU(key string, r *PerformanceReport, base float64, desc string) {
	GinkgoHelper()
	expectThreshold(key, "cpu_cores", "<", r.KarpenterAvgCPUCores, base, CPUThreshold(key, base), desc)
}

// expectNoBreaches fails the spec with every threshold it breached under PERF_SOFT_THRESHOLDS=true.
func expectNoBreaches() {
	breaches := lo.Filter(checks, func(c thresholdCheck, _ int) bool { return c.spec == CurrentSpecReport().FullText() && !c.pass })
	if softThresholds() && len(breaches) > 0 {
		Fail(fmt.Sprintf("%d performance threshold(s) breached:\n%s", len(breaches), lo.Reduce(breaches,
			func(s string, c thresholdCheck, _ int) string { return s + c.String() }, "")))
	}
}

// writeResults writes perf_thresholds.tsv and perf_summary.md to OUTPUT_DIR, if set. Parallel Ginkgo processes
// each write their own files.
func writeResults() error {
	dir := os.Getenv("OUTPUT_DIR")
	if dir == "" {
		return nil
	}
	suffix := ""
	if suiteConfig, _ := GinkgoConfiguration(); suiteConfig.ParallelTotal > 1 {
		suffix = fmt.Sprintf("_%d", GinkgoParallelProcess())
	}
	tsv := "spec\tkey\tmetric\tcomparator\tcore_threshold\tthreshold\tmeasured\tpass\n"
	for _, c := range checks {
		tsv += c.String()
	}
	if _, err := common.WriteArtifactUnder(dir, "perf_thresholds"+suffix+".tsv", []byte(tsv)); err != nil {
		return err
	}
	_, err := common.WriteArtifactUnder(dir, "perf_summary"+suffix+".md", []byte(renderSummary(checks)))
	return err
}

// renderSummary renders the checks as GitHub-flavored markdown for a CI step summary.
func renderSummary(checks []thresholdCheck) string {
	passed := lo.CountBy(checks, func(c thresholdCheck) bool { return c.pass })
	var b strings.Builder
	fmt.Fprintf(&b, "## Karpenter performance: %s (%d/%d threshold checks passed)\n\n",
		lo.Ternary(passed == len(checks), "PASS", "FAIL"), passed, len(checks))
	b.WriteString("| Phase | Metric | Measured | Threshold | Default | Result |\n| --- | --- | ---: | ---: | ---: | --- |\n")
	for _, c := range checks {
		fmt.Fprintf(&b, "| %s | %s | %.4g | %s %g | %g | %s |\n",
			c.key, c.metric, c.measured, c.comparator, c.threshold, c.coreThreshold, lo.Ternary(c.pass, "pass", "**FAIL**"))
	}
	return b.String()
}
