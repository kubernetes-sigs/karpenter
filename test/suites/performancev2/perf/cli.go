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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samber/lo"
)

// InventoryMain is the inventory command for a provider's own module: it prints the providers' inventory and the
// repeats table, and exits non-zero if levels or evidence don't validate.
func InventoryMain(scenarios []Scenario, providers ...*Provider) {
	mde := flag.Float64("mde", 0.10, "regression size for the repeats table, as a fraction")
	fpr := flag.Float64("fpr", 0.01, "false positive rate for the repeats table")
	power := flag.Float64("power", 0.80, "power for the repeats table")
	list := flag.String("list", "", "print the scenarios this provider enables as a JSON array (for a workflow matrix) and exit")
	flag.Parse()
	if *list != "" {
		p, ok := lo.Find(providers, func(p *Provider) bool { return p.Name == *list })
		if !ok {
			fail(fmt.Errorf("unknown provider %q", *list))
		}
		b, _ := json.Marshal(EnabledScenarios(p, scenarios))
		fmt.Println(string(b))
		return
	}
	out, err := Inventory(scenarios, providers...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(out + Repeats(*mde, *fpr, *power, providers...))
}

// CalibrateMain is the calibrate command for a provider's own module: the calibration as JSON on stdout, as one
// <scenario>.json per scenario with -out, or as Go levels with -levels.
func CalibrateMain(providers ...*Provider) {
	dir := flag.String("dir", ".", "directory of runs: base-*/ for baselines, <patch>/ or reg-<patch>/ for planted regressions; may nest")
	name := flag.String("provider", providers[0].Name, "provider name in the reports")
	source := flag.String("source", "", "where the runs came from (cluster, controller size)")
	out := flag.String("out", "", "directory to write <scenario>.json files into, instead of stdout")
	controls := flag.String("controls", strings.Join(DefaultControls, ","), "comma-separated negative-control patches")
	levels := flag.Bool("levels", false, "print the calibration rule's levels as Go instead of the calibration")
	flag.Parse()
	p, ok := lo.Find(providers, func(p *Provider) bool { return p.Name == *name })
	if !ok {
		fail(fmt.Errorf("unknown provider %q", *name))
	}
	c, err := Calibrate(*dir, p, *source, lo.Compact(strings.Split(*controls, ","))...)
	if err != nil {
		fail(err)
	}
	switch {
	case *levels:
		fmt.Print(FormatLevels(SuggestLevels(c)))
	case *out != "":
		for scenario, sc := range c.ByScenario() {
			b, _ := json.MarshalIndent(sc, "", "  ")
			if err := os.WriteFile(filepath.Join(*out, scenario+".json"), append(b, '\n'), 0o600); err != nil {
				fail(err)
			}
			fmt.Fprintf(os.Stderr, "wrote %s\n", filepath.Join(*out, scenario+".json"))
		}
	default:
		b, _ := json.MarshalIndent(c, "", "  ")
		fmt.Println(string(b))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
