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

package main

import (
	"slices"
	"testing"

	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/test/suites/performancev2"
	"sigs.k8s.io/karpenter/test/suites/performancev2/env"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf/scenarios"
)

// Every scenario runs on both core providers, so perf-periodic (cmd/inventory -list kwok-ci) covers it.
func TestCoreProvidersEnableEveryScenario(t *testing.T) {
	all := lo.Map(scenarios.All, func(s perf.Scenario, _ int) string { return s.Name })
	for _, p := range []*perf.Provider{performancev2.KWOK(env.Clients{}), performancev2.KWOKCI(env.Clients{})} {
		if got := perf.EnabledScenarios(p, scenarios.All); !slices.Equal(got, all) {
			t.Errorf("provider %s enables %v, want every scenario %v", p.Name, got, all)
		}
		if err := perf.Validate(p, scenarios.All); err != nil {
			t.Errorf("provider %s: %v", p.Name, err)
		}
		if err := perf.CheckEvidence(p); err != nil {
			t.Errorf("provider %s: %v", p.Name, err)
		}
	}
}
