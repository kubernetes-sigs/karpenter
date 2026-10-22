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
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Suite registers each scenario as a Ginkgo spec for the provider. Specs for scenarios the provider has
// no levels for are skipped, so a provider opts in phase by phase. With KARPENTER_PERF_SWEEP=<scenario>
// (or "all"), each cell of the scenario's Sweep grid becomes its own spec; sweep cells are measured, never gated.
func Suite(provider func() *Provider, scenarios ...Scenario) bool {
	sweep := os.Getenv("KARPENTER_PERF_SWEEP")
	return Describe("Performance", func() {
		for i := range scenarios {
			s := &scenarios[i]
			labels := Label("perf")
			for v := range s.Vectors {
				labels = append(labels, "vector:"+string(v))
			}
			Context(s.Name, labels, func() {
				if sweep == "" || (sweep != "all" && sweep != s.Name) || len(s.Sweep) == 0 {
					if sweep != "" {
						return
					}
					It("should hold its performance levels", func(ctx context.Context) { run(ctx, provider(), scenarios, s, nil) })
					return
				}
				grid := s.Sweep
				if g := os.Getenv("KARPENTER_PERF_SWEEP_GRID"); g != "" {
					grid = map[Vector][]int{}
					Expect(json.Unmarshal([]byte(g), &grid)).To(Succeed(), "parsing KARPENTER_PERF_SWEEP_GRID")
				}
				for _, cell := range cells(grid) {
					It("sweep "+sizesLabel(cell, "=", " "), Label("sweep"), func(ctx context.Context) { run(ctx, provider(), scenarios, s, cell) })
				}
			})
		}
	})
}

func run(ctx context.Context, p *Provider, all []Scenario, s *Scenario, cell map[Vector]int) {
	Expect(Validate(p, all)).To(Succeed())
	if !p.enabled(s) {
		Skip("scenario " + s.Name + " is off for provider " + p.Name)
	}
	r := newRun(ctx, p, s, cell)
	r.expectClean()
	DeferCleanup(func(ctx SpecContext) { r.cleanup(ctx) }, NodeTimeout(40*time.Minute))
	s.Run(r)
}

func (p *Provider) enabled(s *Scenario) bool {
	for id := range p.Levels {
		if strings.HasPrefix(id, s.Name+"/") {
			return true
		}
	}
	return false
}
