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

// plot renders a heat map per phase and metric from a directory of performancev2 report JSONs:
// one vector on x, another on y, cell shade green (low) to red (high) on a log scale.
//
//	go run ./test/suites/performancev2/cmd/plot -dir $OUTPUT_DIR -out perf.html
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type report struct {
	Provider  string         `json:"provider"`
	Phase     string         `json:"phase"`
	Commit    string         `json:"commit"`
	Sizes     map[string]int `json:"sizes"`
	Total     float64        `json:"total_time"`
	Startup   float64        `json:"startup_seconds"`
	Resources struct {
		P95HeapMB   float64 `json:"p95_heap_mb"`
		AvgCPUCores float64 `json:"avg_cpu_cores"`
	} `json:"resources"`
}

type metric struct {
	name, unit string
	value      func(report) float64
}

var metrics = []metric{
	{"total_time", "s", func(r report) float64 { return r.Total / 1e9 }},
	{"heap_mb", "MB", func(r report) float64 { return r.Resources.P95HeapMB }},
	{"cpu_cores", "cores", func(r report) float64 { return r.Resources.AvgCPUCores }},
	{"startup_seconds", "s", func(r report) float64 { return r.Startup }},
}

var low, mid, high = [3]float64{0x1f, 0x8f, 0x6a}, [3]float64{0xf0, 0xef, 0xec}, [3]float64{0xd7, 0x26, 0x3d}

const cell = 64

func main() {
	dir := flag.String("dir", ".", "directory of *_performance_report.json files (searched recursively)")
	out := flag.String("out", "perf.html", "output HTML file")
	xv := flag.String("x", "pods", "vector on the x axis")
	yv := flag.String("y", "nodepools", "vector on the y axis")
	flag.Parse()
	byPhase, err := load(*dir, *xv, *yv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	page := render2D(byPhase, *dir, *xv, *yv)
	if err := os.WriteFile(*out, []byte(page), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d phases)\n", *out, len(byPhase))
}

func load(dir, xv, yv string) (map[string][]report, error) {
	var paths []string
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, "_performance_report.json") {
			paths = append(paths, p)
		}
		return err
	}); err != nil {
		return nil, err
	}
	byPhase := map[string][]report{}
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			return nil, err
		}
		var r report
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		_, hasX := r.Sizes[xv]
		_, hasY := r.Sizes[yv]
		if hasX && hasY {
			byPhase[r.Phase] = append(byPhase[r.Phase], r)
		}
	}
	return byPhase, nil
}

func render2D(byPhase map[string][]report, dir, xv, yv string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><meta charset="utf-8"><title>Karpenter perf: %s x %s</title>
<style>body{font:14px system-ui,sans-serif;background:#fcfcfb;color:#0b0b0b;margin:24px}h2{margin:28px 0 4px}
.panels{display:flex;flex-wrap:wrap;gap:24px}figure{margin:0}figcaption{color:#52514e;margin:4px 0}
text{font:12px system-ui,sans-serif}.muted{fill:#52514e}table{border-collapse:collapse;margin-top:8px}td,th{padding:2px 8px;border-bottom:1px solid #e6e5e1;text-align:right}</style>
<h1>Karpenter perf: %s (x) by %s (y)</h1><p class="muted">Shade is log-scaled per panel, green low and red high, at full strength only when the panel spans 2x or more (so noise stays pale). Every cell prints its value; hover for details.</p>`, xv, yv, xv, yv)
	phases := make([]string, 0, len(byPhase))
	for p := range byPhase {
		phases = append(phases, p)
	}
	sort.Strings(phases)
	if len(phases) == 0 {
		fmt.Fprintf(&b, "<p>No reports in %s carry both %s and %s sizes.</p>", html.EscapeString(dir), xv, yv)
	}
	for _, ph := range phases {
		b.WriteString(renderPhase(ph, byPhase[ph], xv, yv))
	}
	return b.String()
}

func renderPhase(ph string, rs []report, xv, yv string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<h2>%s</h2><p class=\"muted\">provider %s, %d reports, commit %s</p><div class=\"panels\">", html.EscapeString(ph), html.EscapeString(rs[0].Provider), len(rs), html.EscapeString(commits(rs)))
	hasStartup := false
	for _, r := range rs {
		hasStartup = hasStartup || r.Startup > 0
	}
	for _, m := range metrics {
		if m.name != "total_time" || !hasStartup {
			b.WriteString(render(rs, m, xv, yv))
		}
	}
	b.WriteString("</div>")
	return b.String()
}

func medians(rs []report, m metric, xv, yv string) (map[[2]int]float64, map[[2]int]int, []int, []int) {
	vals := map[[2]int][]float64{}
	xs, ys := map[int]bool{}, map[int]bool{}
	for _, r := range rs {
		if v := m.value(r); v > 0 {
			k := [2]int{r.Sizes[xv], r.Sizes[yv]}
			vals[k] = append(vals[k], v)
			xs[k[0]], ys[k[1]] = true, true
		}
	}
	med, runs := map[[2]int]float64{}, map[[2]int]int{}
	for k, v := range vals {
		sort.Float64s(v)
		med[k], runs[k] = v[len(v)/2], len(v)
	}
	return med, runs, sorted(xs), sorted(ys)
}

func position(v, lo, hi float64) float64 {
	if hi <= lo {
		return 0.5
	}
	return 0.5 + ((math.Log(v)-math.Log(lo))/(math.Log(hi)-math.Log(lo))-0.5)*math.Min(1, math.Log(hi/lo)/math.Log(2))
}

func render(rs []report, m metric, xv, yv string) string {
	med, runs, xk, yk := medians(rs, m, xv, yv)
	if len(med) == 0 {
		return ""
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range med {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	left, top := 70, 10
	w, h := left+cell*len(xk)+10, top+cell*len(yk)+72
	var s strings.Builder
	fmt.Fprintf(&s, `<figure><figcaption><b>%s</b> (%s), median per cell, range x%.2f</figcaption><svg width="%d" height="%d" role="img" aria-label="%s heat map">`, m.name, m.unit, hi/lo, w, h, m.name)
	for j, y := range yk {
		row := len(yk) - 1 - j
		fmt.Fprintf(&s, `<text x="%d" y="%d" text-anchor="end" class="muted">%d</text>`, left-6, top+row*cell+cell/2+4, y)
		for i, x := range xk {
			v, ok := med[[2]int{x, y}]
			px, py := left+i*cell, top+row*cell
			if !ok {
				fmt.Fprintf(&s, `<rect x="%d" y="%d" width="%d" height="%d" fill="none" stroke="#e6e5e1"/>`, px+1, py+1, cell-2, cell-2)
				continue
			}
			t := position(v, lo, hi)
			fill := shade(t)
			ink := "#0b0b0b"
			if t < 0.15 || t > 0.85 {
				ink = "#ffffff"
			}
			fmt.Fprintf(&s, `<g><title>%s=%d %s=%d: %s %s (%d runs)</title><rect x="%d" y="%d" width="%d" height="%d" rx="4" fill="%s"/><text x="%d" y="%d" text-anchor="middle" fill="%s">%s</text></g>`,
				xv, x, yv, y, format(v), m.unit, runs[[2]int{x, y}], px+1, py+1, cell-2, cell-2, fill, px+cell/2, py+cell/2+4, ink, format(v))
		}
	}
	for i, x := range xk {
		fmt.Fprintf(&s, `<text x="%d" y="%d" text-anchor="middle" class="muted">%d</text>`, left+i*cell+cell/2, top+len(yk)*cell+16, x)
	}
	fmt.Fprintf(&s, `<text x="%d" y="%d" text-anchor="middle" class="muted">%s</text><text x="14" y="%d" transform="rotate(-90 14 %d)" text-anchor="middle" class="muted">%s</text>`,
		left+cell*len(xk)/2, top+len(yk)*cell+34, xv, top+cell*len(yk)/2, top+cell*len(yk)/2, yv)
	gy := top + len(yk)*cell + 44
	fmt.Fprintf(&s, `<defs><linearGradient id="g-%s"><stop offset="0" stop-color="%s"/><stop offset=".5" stop-color="%s"/><stop offset="1" stop-color="%s"/></linearGradient></defs><rect x="%d" y="%d" width="%d" height="8" rx="4" fill="url(#g-%s)"/><text x="%d" y="%d" class="muted">%s</text><text x="%d" y="%d" text-anchor="end" class="muted">%s</text>`,
		m.name, shade(0), shade(0.5), shade(1), left, gy, cell*len(xk), m.name, left, gy+20, format(lo), left+cell*len(xk), gy+20, format(hi))
	s.WriteString("</svg>")
	s.WriteString(table(med, xk, yk, xv, yv))
	s.WriteString("</figure>")
	return s.String()
}

func shade(t float64) string {
	from, to := mid, high
	if t < 0.5 {
		from, to, t = low, mid, t*2
	} else {
		t = (t - 0.5) * 2
	}
	c := func(i int) int { return int(math.Round(from[i] + (to[i]-from[i])*t)) }
	return fmt.Sprintf("#%02x%02x%02x", c(0), c(1), c(2))
}

func format(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func sorted(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func commits(rs []report) string {
	set := map[string]bool{}
	for _, r := range rs {
		if r.Commit != "" {
			set[r.Commit] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "unknown"
	}
	return strings.Join(out, ", ")
}

func table(med map[[2]int]float64, xk, yk []int, xv, yv string) string {
	var s strings.Builder
	s.WriteString("<details><summary>table</summary><table><tr><th>" + yv + " \\ " + xv + "</th>")
	for _, x := range xk {
		fmt.Fprintf(&s, "<th>%d</th>", x)
	}
	s.WriteString("</tr>")
	for j := len(yk) - 1; j >= 0; j-- {
		fmt.Fprintf(&s, "<tr><th>%d</th>", yk[j])
		for _, x := range xk {
			if v, ok := med[[2]int{x, yk[j]}]; ok {
				fmt.Fprintf(&s, "<td>%s</td>", format(v))
			} else {
				s.WriteString("<td>-</td>")
			}
		}
		s.WriteString("</tr>")
	}
	s.WriteString("</table></details>")
	return s.String()
}
