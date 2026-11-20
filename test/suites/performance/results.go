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

	. "github.com/onsi/ginkgo/v2"
	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

// gateRecord is one evaluated gate of a phase, with the spec that evaluated it.
type gateRecord struct {
	spec, phase string
	GateResult
}

// breached reports whether the gate fails the spec: a stable gate that didn't pass. Experimental breaches only warn.
func (c gateRecord) breached() bool { return !c.Passed && c.Tier != TierExperimental }

func (c gateRecord) comparator() string { return lo.Ternary(c.Upper, "<", ">") }

func (c gateRecord) result() string {
	switch {
	case c.Passed:
		return "pass"
	case c.Tier == TierExperimental:
		return "warn"
	default:
		return "**FAIL**"
	}
}

func (c gateRecord) tsv() string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%g\t%g\t%.4f\t%t\n",
		c.spec, c.phase, c.Metric, c.Tier, c.comparator(), c.Default, c.Bound, c.Value, c.Passed)
}

// records holds every gate evaluated by this Ginkgo process, in order.
var records []gateRecord

func softThresholds() bool {
	soft, _ := strconv.ParseBool(os.Getenv("PERF_SOFT_THRESHOLDS"))
	return soft
}

// recordGates records a phase's gate results and rewrites the results files in OUTPUT_DIR.
func recordGates(phase string, results []GateResult) error {
	spec := CurrentSpecReport().FullText()
	for _, r := range results {
		records = append(records, gateRecord{spec: spec, phase: phase, GateResult: r})
	}
	return writeResults()
}

// expectNoBreaches fails the spec with every stable gate it breached under PERF_SOFT_THRESHOLDS=true.
func expectNoBreaches() {
	if !softThresholds() {
		return
	}
	spec := CurrentSpecReport().FullText()
	breaches := lo.Filter(records, func(c gateRecord, _ int) bool { return c.spec == spec && c.breached() })
	if len(breaches) > 0 {
		Fail(fmt.Sprintf("%d stable performance gate(s) breached:\n%s", len(breaches), strings.Join(
			lo.Map(breaches, func(c gateRecord, _ int) string { return fmt.Sprintf("%s: %s", c.phase, c.GateResult) }), "\n")))
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
	var tsv strings.Builder
	tsv.WriteString("spec\tphase\tmetric\ttier\tcomparator\tdefault\tthreshold\tmeasured\tpass\n")
	for _, c := range records {
		tsv.WriteString(c.tsv())
	}
	if _, err := common.WriteArtifactUnder(dir, "perf_thresholds"+suffix+".tsv", []byte(tsv.String())); err != nil {
		return err
	}
	_, err := common.WriteArtifactUnder(dir, "perf_summary"+suffix+".md", []byte(renderSummary(records)))
	return err
}

// renderSummary renders the gates as GitHub-flavored markdown for a CI step summary. The run fails only on
// stable breaches; experimental breaches are shown as warnings.
func renderSummary(records []gateRecord) string {
	passed := lo.CountBy(records, func(c gateRecord) bool { return c.Passed })
	var b strings.Builder
	fmt.Fprintf(&b, "## Karpenter performance: %s (%d/%d gates passed)\n\n",
		lo.Ternary(lo.ContainsBy(records, gateRecord.breached), "FAIL", "PASS"), passed, len(records))
	b.WriteString("| Phase | Metric | Tier | Measured | Threshold | Default | Result |\n| --- | --- | --- | ---: | ---: | ---: | --- |\n")
	for _, c := range records {
		fmt.Fprintf(&b, "| %s | %s | %s | %.4g | %s %g | %g | %s |\n",
			c.phase, c.Metric, c.Tier, c.Value, c.comparator(), c.Bound, c.Default, c.result())
	}
	return b.String()
}
