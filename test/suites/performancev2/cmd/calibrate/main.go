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
	"sigs.k8s.io/karpenter/test/suites/performancev2"
	"sigs.k8s.io/karpenter/test/suites/performancev2/env"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

func main() {
	perf.CalibrateMain(performancev2.KWOK(env.Clients{}), performancev2.KWOKCI(env.Clients{}))
}
