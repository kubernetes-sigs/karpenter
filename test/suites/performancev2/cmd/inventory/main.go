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

// inventory prints what the core KWOK providers run in performancev2, generated from the same scenarios
// and levels the suite uses. It needs no cluster.
package main

import (
	"fmt"
	"os"

	"sigs.k8s.io/karpenter/test/suites/performancev2"
	"sigs.k8s.io/karpenter/test/suites/performancev2/env"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf/scenarios"
)

func main() {
	out, err := perf.Inventory(scenarios.All, performancev2.KWOK(env.Clients{}), performancev2.KWOKCI(env.Clients{}))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(out)
}
