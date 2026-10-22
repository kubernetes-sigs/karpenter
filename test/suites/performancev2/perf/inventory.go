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

package perf

import (
	"fmt"
	"sort"
	"strings"
)

func (s *Scenario) phase(name string) (PhaseSpec, bool) {
	for _, p := range s.Phases {
		if p.Name == name {
			return p, true
		}
	}
	return PhaseSpec{}, false
}

// Validate checks that every level a provider sets names a declared scenario phase, that quantile bounds don't
// decrease, and that a phase doesn't make two measures of the same thing both Stable.
func Validate(p *Provider, scenarios []Scenario) error {
	var errs []string
	for id, gates := range p.Levels {
		if !declared(scenarios, id) {
			errs = append(errs, fmt.Sprintf("provider %s sets levels for %q, which no scenario declares", p.Name, id))
		}
		errs = append(errs, validateGates(p.Name, id, gates)...)
	}
	sort.Strings(errs)
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func declared(scenarios []Scenario, id string) bool {
	name, phase, _ := strings.Cut(id, "/")
	for i := range scenarios {
		if scenarios[i].Name == name {
			_, ok := scenarios[i].phase(phase)
			return ok
		}
	}
	return false
}

func validateGates(provider, id string, gates []Gate) []string {
	var errs []string
	bound, stable := map[string]float64{}, map[string]bool{}
	for _, g := range gates {
		bound[g.Metric.Name] = g.Bound
		stable[g.Metric.Name] = stable[g.Metric.Name] || g.Tier == TierStable
	}
	if stable[TotalTimeSeconds.Name] && stable[PodReady.Max.Name] {
		errs = append(errs, fmt.Sprintf("provider %s makes both %s and %s Stable on %s; they measure nearly the same thing, so at most one should block", provider, TotalTimeSeconds.Name, PodReady.Max.Name, id))
	}
	last, lastName := 0.0, ""
	for _, m := range []Metric{PodReady.P50, PodReady.P95, PodReady.P99, PodReady.Max} {
		if b, ok := bound[m.Name]; ok {
			if b < last {
				errs = append(errs, fmt.Sprintf("provider %s sets %s %s < %g below %s < %g; quantile bounds must not decrease", provider, id, m.Name, b, lastName, last))
			}
			last, lastName = b, m.Name
		}
	}
	return errs
}

func metricByName(name string) Metric {
	for _, m := range Metrics {
		if m.Name == name {
			return m
		}
	}
	return Metric{Name: name}
}

func (r *Run) alarm(id string, breached []GateResult) string {
	_, name, _ := strings.Cut(id, "/")
	ph, _ := r.Scenario.phase(name)
	var b strings.Builder
	fmt.Fprintf(&b, "ALARM %s on %s (%s)\n%s\n%s\n", id, r.Provider.Name, sizesLabel(r.Sizes, "=", " "), r.Scenario.Summary, ph.Summary)
	for _, g := range breached {
		m := metricByName(g.Metric)
		fmt.Fprintf(&b, "\n%s\n  measures: %s\n  check: %s\n", g, m.Measures, m.OnBreach)
	}
	fmt.Fprintf(&b, "\nreproduce: go test ./test/suites/performancev2/... --ginkgo.focus=%s, then diff the report JSON against the last green main run\n", r.Scenario.Name)
	return b.String()
}

func (p *Provider) sizes(s *Scenario) map[Vector]int {
	out := map[Vector]int{}
	for v, def := range s.Vectors {
		out[v] = p.size(s.Name, v, def)
	}
	return out
}

func cells(grid map[Vector][]int) []map[Vector]int {
	out := []map[Vector]int{{}}
	keys := make([]string, 0, len(grid))
	for v := range grid {
		keys = append(keys, string(v))
	}
	sort.Strings(keys)
	for _, k := range keys {
		var next []map[Vector]int
		for _, c := range out {
			for _, n := range grid[Vector(k)] {
				cc := map[Vector]int{Vector(k): n}
				for kk, vv := range c {
					cc[kk] = vv
				}
				next = append(next, cc)
			}
		}
		out = next
	}
	return out
}

// CheckEvidence enforces the promotion rules on Stable gates against the provider's calibration: enough
// baseline runs, a low CV, and at least one planted regression that breaches the bound.
func CheckEvidence(p *Provider) error {
	var errs []string
	for id, gates := range p.Levels {
		for _, g := range gates {
			if g.Tier != TierStable {
				continue
			}
			e, ok := p.Calibration.evidence(id, g.Metric.Name)
			switch {
			case !ok:
				errs = append(errs, fmt.Sprintf("%s %s is Stable with no calibration evidence", id, g.Metric.Name))
			case e.N < StableMinRuns:
				errs = append(errs, fmt.Sprintf("%s %s is Stable on %d baseline runs; needs %d", id, g.Metric.Name, e.N, StableMinRuns))
			case e.CV > StableMaxCV:
				errs = append(errs, fmt.Sprintf("%s %s is Stable with CV %.0f%%; needs <= %.0f%%", id, g.Metric.Name, e.CV*100, StableMaxCV*100))
			case len(e.catches(g.Metric, g.Bound)) == 0:
				errs = append(errs, fmt.Sprintf("%s %s is Stable but no planted regression breaches %g", id, g.Metric.Name, g.Bound))
			}
		}
	}
	sort.Strings(errs)
	if len(errs) > 0 {
		return fmt.Errorf("evidence: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Inventory renders what a provider runs: every phase's gated and measured metrics with the calibration
// evidence behind each bound, the sweep grids, and what each metric means. It is generated from the same
// values the suite uses.
func Inventory(scenarios []Scenario, providers ...*Provider) (string, error) {
	var b strings.Builder
	for _, p := range providers {
		if err := Validate(p, scenarios); err != nil {
			return "", err
		}
		if err := CheckEvidence(p); err != nil {
			return "", err
		}
		writeLevels(&b, p, scenarios)
		writeQuantiles(&b, p, scenarios)
	}
	writeSweeps(&b, scenarios)
	writeScenarios(&b, providers, scenarios)
	writeMetrics(&b)
	return b.String(), nil
}

func writeLevels(b *strings.Builder, p *Provider, scenarios []Scenario) {
	fmt.Fprintf(b, "## Provider %s\n\n", p.Name)
	if p.Summary != "" {
		fmt.Fprintf(b, "%s\n\n", p.Summary)
	}
	if c := p.Calibration; c != nil && c.Source != "" {
		fmt.Fprintf(b, "Calibration: %s, baseline %s, generated %s.\n\n", c.Source, c.Baseline, c.Generated.Format("2006-01-02"))
	}
	b.WriteString("| phase | kind | metric | level | bound | n | median | CV | headroom | catches |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for i := range scenarios {
		for _, ph := range scenarios[i].Phases {
			id := scenarios[i].Name + "/" + ph.Name
			gates, enabled := p.Levels[id]
			if !enabled {
				fmt.Fprintf(b, "| %s | - | - | off | - | | | | | |\n", id)
				continue
			}
			byMetric := map[string]Gate{}
			for _, g := range gates {
				byMetric[g.Metric.Name] = g
			}
			for _, kind := range []Kind{KindDuration, KindOutcome, KindResource} {
				for _, m := range Metrics {
					if m.Kind == kind {
						b.WriteString(levelRow(p, id, m, byMetric))
					}
				}
			}
		}
	}
	fmt.Fprintf(b, "\nStable needs n >= %d, CV <= %.0f%% and at least one catch. Headroom is how far the median sits from the bound: the smallest regression the gate can see.\n", StableMinRuns, StableMaxCV*100)
}

func levelRow(p *Provider, id string, m Metric, byMetric map[string]Gate) string {
	g, gated := byMetric[m.Name]
	e, measured := p.Calibration.evidence(id, m.Name)
	if !gated && !measured {
		return ""
	}
	level, bound, head, catches, n, med, cv := "measured", "-", "", "", "", "", ""
	if gated {
		op := ">"
		if m.Upper {
			op = "<"
		}
		level, bound = string(g.Tier), fmt.Sprintf("%s %g %s", op, g.Bound, m.Unit)
	}
	if measured {
		n, med, cv = fmt.Sprint(e.N), fmt.Sprintf("%g", e.Median), fmt.Sprintf("%.0f%%", e.CV*100)
	}
	if gated && measured {
		head, catches = fmt.Sprintf("%.2fx", e.headroom(m, g.Bound)), strings.Join(e.catches(m, g.Bound), ", ")
	}
	return fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", id, m.Kind, m.Name, level, bound, n, med, cv, head, catches)
}

func writeQuantiles(b *strings.Builder, p *Provider, scenarios []Scenario) {
	fmt.Fprintf(b, "\n### %s pod latency by quantile (pod_ready_seconds)\n\n| scenario/phase | p50 | p95 | p99 | max (p100) |\n|---|---|---|---|---|\n", p.Name)
	for i := range scenarios {
		for _, ph := range scenarios[i].Phases {
			id := scenarios[i].Name + "/" + ph.Name
			row := []string{}
			for _, m := range []Metric{PodReady.P50, PodReady.P95, PodReady.P99, PodReady.Max} {
				row = append(row, quantileCell(p.Levels[id], m))
			}
			if strings.Join(row, "") != "----" {
				fmt.Fprintf(b, "| %s | %s |\n", id, strings.Join(row, " | "))
			}
		}
	}
}

func quantileCell(gates []Gate, m Metric) string {
	for _, g := range gates {
		if g.Metric.Name == m.Name {
			return fmt.Sprintf("%s < %g %s", g.Tier, g.Bound, m.Unit)
		}
	}
	return "-"
}

func writeSweeps(b *strings.Builder, scenarios []Scenario) {
	b.WriteString("\n## Sweeps (measured only, never gate; run with KARPENTER_PERF_SWEEP=<scenario>)\n\n| scenario | grid | cells |\n|---|---|---|\n")
	for i := range scenarios {
		s := &scenarios[i]
		if len(s.Sweep) == 0 {
			continue
		}
		keys := make([]string, 0, len(s.Sweep))
		for v := range s.Sweep {
			keys = append(keys, string(v))
		}
		sort.Strings(keys)
		axes := make([]string, 0, len(keys))
		for _, k := range keys {
			axes = append(axes, fmt.Sprintf("%s %v", k, s.Sweep[Vector(k)]))
		}
		fmt.Fprintf(b, "| %s | %s | %d |\n", s.Name, strings.Join(axes, " x "), len(cells(s.Sweep)))
	}
}

func writeScenarios(b *strings.Builder, providers []*Provider, scenarios []Scenario) {
	b.WriteString("\n## Scenarios\n\n")
	for i := range scenarios {
		s := &scenarios[i]
		sizes := make([]string, 0, len(providers))
		for _, p := range providers {
			sizes = append(sizes, p.Name+": "+sizesLabel(p.sizes(s), "=", " "))
		}
		fmt.Fprintf(b, "- **%s** (%s): %s\n", s.Name, strings.Join(sizes, "; "), s.Summary)
		for _, ph := range s.Phases {
			fmt.Fprintf(b, "  - `%s/%s`: %s\n", s.Name, ph.Name, ph.Summary)
		}
	}
}

func writeMetrics(b *strings.Builder) {
	b.WriteString("\n## Metrics\n\n| metric | kind | unit | breach when | measures | check on breach |\n|---|---|---|---|---|---|\n")
	for _, m := range Metrics {
		dir := "below bound"
		if m.Upper {
			dir = "above bound"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n", m.Name, m.Kind, m.Unit, dir, m.Measures, m.OnBreach)
	}
}
