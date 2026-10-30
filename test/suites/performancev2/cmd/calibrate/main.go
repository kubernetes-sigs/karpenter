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

// calibrate reduces a directory of performancev2 reports into the provider's calibration file: baseline
// statistics per phase and metric, a suggested bound, and the value each planted regression reached.
//
//	go run ./test/suites/performancev2/cmd/calibrate -dir runs/ -source "EKS, tier-xl control plane" > test/suites/performancev2/calibration/kwok.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

func main() {
	dir := flag.String("dir", ".", "directory with base-*/ and <regression>/ subdirectories of report JSONs")
	provider := flag.String("provider", "kwok", "provider name in the reports")
	source := flag.String("source", "", "where the runs came from (cluster, controller size)")
	flag.Parse()
	c, err := perf.Calibrate(*dir, *provider, *source)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(c, "", "  ")
	fmt.Println(string(out))
}
