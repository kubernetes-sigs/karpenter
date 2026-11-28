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
	"math"
	"sort"
	"strings"

	"github.com/samber/lo"
)

// The calibration rule:
//   - Bounds: max(BoundMaxFactor x the highest sample, median + BoundZ sd) from above; min(lowest / BoundMinFactor,
//     median - BoundZ sd) from below. Negative controls count as samples.
//   - Pod-ready bounds are whole seconds, at least PodReadyStableFactor x the highest sample, non-decreasing.
//   - Stable needs Validate's evidence bar and a regression breaching the bound by StableCatchMargin, and never
//     applies to ExperimentalOnly metrics or settle-phase wall time.
const (
	BoundMaxFactor       = 1.2
	BoundMinFactor       = 1.15
	BoundZ               = 2.33
	PodReadyStableFactor = 1.5
	StableCatchMargin    = 0.10
)

// gatedByKind are the metrics SuggestLevels gates for each Result kind.
var gatedByKind = map[string][]Metric{
	"scale-out":     {PodReady.P95, PodReady.P99, PodReady.Max, CPUUtil, MemoryUtil, Nodes, HeapMB, CPUCores},
	"consolidation": {TotalTimeSeconds, CPUUtil, MemoryUtil, Nodes, PodsDisrupted, HeapMB, CPUCores},
	"replacement":   {TotalTimeSeconds, Replacement.P50, Replacement.P95, Replacement.Max, UnavailablePodSeconds, PeakUnavailablePods, CPUUtil, MemoryUtil, Nodes, PodsDisrupted, HeapMB, CPUCores},
	"startup":       {StartupSeconds, HeapMB},
}

func (e MetricEvidence) samples() (lowest, highest float64) {
	lowest, highest = e.Min, e.Max
	for _, v := range e.Controls {
		lowest, highest = math.Min(lowest, v), math.Max(highest, v)
	}
	return lowest, highest
}

// SuggestBound is the calibration rule's bound for m.
func SuggestBound(m Metric, e MetricEvidence) float64 {
	lowest, highest := e.samples()
	sd := e.CV * e.Median
	if !m.Upper {
		return roundBound(m, math.Min(lowest/BoundMinFactor, e.Median-BoundZ*sd))
	}
	b := math.Max(BoundMaxFactor*highest, e.Median+BoundZ*sd)
	if isPodReady(m) && m.Name != PodReady.Max.Name {
		b = math.Max(b, PodReadyStableFactor*highest)
	}
	return roundBound(m, b)
}

func isPodReady(m Metric) bool { return strings.HasPrefix(m.Name, "pod_ready_seconds_") }

// roundBound rounds a bound to the metric's resolution.
func roundBound(m Metric, b float64) float64 {
	switch {
	case isPodReady(m) || m.Unit == "nodes" || m.Unit == "pods":
		if m.Upper {
			return math.Ceil(b)
		}
		return math.Floor(b)
	case b >= 100:
		return math.Round(b)
	case b >= 10:
		return math.Round(b*10) / 10
	default:
		return round3(b)
	}
}

// Suggestion is a suggested gate and why it got its tier.
type Suggestion struct {
	Gate    Gate
	Catches []string
	Why     string
}

// SuggestLevels applies the rule to the given scenarios, or to every phase when none are given.
func SuggestLevels(c *Calibration, scenarios ...string) map[string][]Suggestion {
	out := map[string][]Suggestion{}
	for id, byMetric := range c.Phases {
		name, _, _ := strings.Cut(id, "/")
		if len(scenarios) > 0 && !lo.Contains(scenarios, name) {
			continue
		}
		kind := c.Kinds[id]
		last := 0.0
		for _, m := range gatedByKind[kind] {
			e, ok := byMetric[m.Name]
			if !ok || e.Median == 0 {
				continue
			}
			b := SuggestBound(m, e)
			if isPodReady(m) {
				b, last = math.Max(b, last), math.Max(b, last)
			}
			s := Suggestion{Gate: m.Experimental(b), Catches: catchesBy(m, e, b, StableCatchMargin)}
			switch {
			case m.ExperimentalOnly:
				s.Why = m.Name + " is Experimental only"
			case m.Name == TotalTimeSeconds.Name && kind == "consolidation":
				s.Why = "wall time of a " + kind + " phase"
			case e.N < StableMinRuns || e.CV > StableMaxCV || e.FPR(m, b) > StableMaxFPR:
				s.Why = fmt.Sprintf("evidence: n=%d, CV %.0f%%, FPR %.2f%%", e.N, e.CV*100, e.FPR(m, b)*100)
			case len(s.Catches) == 0:
				s.Why = fmt.Sprintf("no planted regression breaches it by %.0f%%", StableCatchMargin*100)
			default:
				s.Gate.Tier = TierStable
			}
			out[id] = append(out[id], s)
		}
	}
	return out
}

func catchesBy(m Metric, e MetricEvidence, bound, margin float64) []string {
	var out []string
	for name, v := range e.Variants {
		if (m.Upper && v >= bound*(1+margin)) || (!m.Upper && v <= bound/(1+margin)) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// FormatLevels prints suggestions as a perf.Levels body.
func FormatLevels(s map[string][]Suggestion) string {
	ids := lo.Keys(s)
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "\t%q: {\n\t\t", id)
		gates := lo.Map(s[id], func(x Suggestion, _ int) string {
			return fmt.Sprintf("perf.%s.%s(%s)", goName(x.Gate.Metric), lo.Ternary(x.Gate.Tier == TierStable, "Stable", "Experimental"), fmt.Sprint(x.Gate.Bound))
		})
		b.WriteString(strings.Join(gates, ", ") + ",\n\t},\n")
	}
	return b.String()
}

func goName(m Metric) string {
	for expr, c := range map[string]Metric{"TotalTimeSeconds": TotalTimeSeconds, "PodReady.P50": PodReady.P50, "PodReady.P95": PodReady.P95,
		"PodReady.P99": PodReady.P99, "PodReady.Max": PodReady.Max, "StartupSeconds": StartupSeconds, "CPUUtil": CPUUtil,
		"MemoryUtil": MemoryUtil, "Nodes": Nodes, "PodsDisrupted": PodsDisrupted, "HeapMB": HeapMB, "CPUCores": CPUCores} {
		if c.Name == m.Name {
			return expr
		}
	}
	return m.Name
}
