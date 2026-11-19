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
)

// InventoryMain is the inventory command for a provider's own module: it prints the providers' inventory and the
// repeats table, and exits non-zero if levels or evidence don't validate.
func InventoryMain(scenarios []Scenario, providers ...*Provider) {
	mde := flag.Float64("mde", 0.10, "regression size for the repeats table, as a fraction")
	fpr := flag.Float64("fpr", 0.01, "false positive rate for the repeats table")
	power := flag.Float64("power", 0.80, "power for the repeats table")
	flag.Parse()
	out, err := Inventory(scenarios, providers...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(out + Repeats(*mde, *fpr, *power, providers...))
}

// CalibrateMain is the calibrate command for a provider's own module: it reduces a directory of reports into the
// named provider's calibration JSON on stdout.
func CalibrateMain(providers ...*Provider) {
	dir := flag.String("dir", ".", "directory with base-*/ and <regression>/ subdirectories of report JSONs")
	name := flag.String("provider", providers[0].Name, "provider name in the reports")
	source := flag.String("source", "", "where the runs came from (cluster, controller size)")
	flag.Parse()
	for _, p := range providers {
		if p.Name != *name {
			continue
		}
		c, err := Calibrate(*dir, p, *source)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		out, _ := json.MarshalIndent(c, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Fprintf(os.Stderr, "unknown provider %q\n", *name)
	os.Exit(1)
}
