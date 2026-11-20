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
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Provider is the plugin a KWOK-based Karpenter distribution implements to run the shared scenarios. A scenario
// runs if Levels has an entry for at least one of its phases; its other phases are measured but not gated.
type Provider struct {
	Name       string
	Summary    string
	Client     client.Client
	KubeClient kubernetes.Interface
	Controller Controller
	// NodeClass and NodePool build each run's starting objects. They get the scenario, so a provider can vary
	// them, for example fewer zones for topology.
	NodeClass func(s *Scenario) client.Object
	NodePool  func(s *Scenario, nodeClass client.Object) *v1.NodePool
	Sizes     map[string]map[Vector]int
	// Shapes overrides the requests of a scenario's deployments, keyed by "<scenario>/<deployment>", for
	// providers whose instance types don't fit the KWOK shapes.
	Shapes map[string]Shape
	// TimeoutScale multiplies every wait in the scenarios, since real launches are slower than KWOK. 0 means 1.
	TimeoutScale float64
	// Metrics are the provider's own metrics. They gate, take overrides, calibrate and show in the inventory
	// like the core ones. Their Value reads what Capture keeps, through Result.Histogram, Counter and Gauge.
	Metrics []Metric
	// Capture adds metric family prefixes to keep in reports, on top of DefaultCapture.
	Capture []string
	// Sinks get every phase's Result after it's written, for example to push it to a metrics backend.
	Sinks []func(context.Context, *Result) error
	// ReproEnv names the provider's own environment variables, such as CLUSTER_NAME, to include in repro lines.
	ReproEnv    []string
	Levels      Levels
	Calibration *Calibration
}

// Shape is a deployment's per-pod requests.
type Shape struct {
	CPU, Memory string
}

// Controller locates the Karpenter pod whose /metrics the suite scrapes through the API server proxy.
type Controller struct {
	Namespace   string
	Selector    labels.Selector
	MetricsPort string
}

// Levels sets, per "<scenario>/<phase>", the gates a provider holds that phase to.
type Levels map[string][]Gate

// Off turns a phase off. Later phases of the scenario are skipped too, since they start from its end state.
var Off = []Gate{{Tier: TierOff}}

func isOff(gates []Gate) bool {
	return len(gates) == 1 && gates[0].Tier == TierOff && gates[0].Metric.Name == ""
}

// With returns a copy of l with overrides layered on, so a provider can start from another's levels and declare
// only the differences. Within a phase, a gate replaces the gate on the same metric and Metric.Off() removes it.
// Off, or a phase l doesn't have, replaces the phase.
func (l Levels) With(overrides Levels) Levels {
	out := Levels{}
	for id, gates := range l {
		out[id] = append([]Gate(nil), gates...)
	}
	for id, gates := range overrides {
		base, ok := out[id]
		if !ok || isOff(gates) || isOff(base) {
			out[id] = withoutOff(gates)
			continue
		}
		for _, g := range gates {
			base = lo.Reject(base, func(b Gate, _ int) bool { return b.Metric.Name == g.Metric.Name })
			if g.Tier != TierOff {
				base = append(base, g)
			}
		}
		out[id] = base
	}
	return out
}

func withoutOff(gates []Gate) []Gate {
	if isOff(gates) {
		return gates
	}
	return lo.Reject(gates, func(g Gate, _ int) bool { return g.Tier == TierOff })
}

type Vector string

const (
	VectorPods      Vector = "pods"
	VectorNodePools Vector = "nodepools"
)

func sizesLabel(sizes map[Vector]int, kv, sep string) string {
	keys := make([]string, 0, len(sizes))
	for v := range sizes {
		keys = append(keys, string(v))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s%s%d", k, kv, sizes[Vector(k)]))
	}
	return strings.Join(parts, sep)
}

// sizeOverrides parses KARPENTER_PERF_SIZES: {"pods":5000} sizes a vector in every scenario, and
// {"topology":{"pods":5000}} sizes one scenario. A scenario's own entry wins.
func sizeOverrides() (all map[Vector]int, byScenario map[string]map[Vector]int, err error) {
	all, byScenario = map[Vector]int{}, map[string]map[Vector]int{}
	s := os.Getenv("KARPENTER_PERF_SIZES")
	if s == "" {
		return all, byScenario, nil
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, nil, fmt.Errorf("parsing KARPENTER_PERF_SIZES: %w", err)
	}
	for k, v := range raw {
		var n int
		if json.Unmarshal(v, &n) == nil {
			all[Vector(k)] = n
			continue
		}
		m := map[Vector]int{}
		if err := json.Unmarshal(v, &m); err != nil {
			return nil, nil, fmt.Errorf("parsing KARPENTER_PERF_SIZES %q: want a size or a map of vector to size: %w", k, err)
		}
		byScenario[k] = m
	}
	return all, byScenario, nil
}

func (p *Provider) size(scenario string, v Vector, def int) (int, error) {
	all, byScenario, err := sizeOverrides()
	if err != nil {
		return 0, err
	}
	if n, ok := byScenario[scenario][v]; ok {
		return n, nil
	}
	if n, ok := all[v]; ok {
		return n, nil
	}
	if n, ok := p.Sizes[scenario][v]; ok {
		return n, nil
	}
	return def, nil
}

func (p *Provider) timeoutScale() float64 {
	if p.TimeoutScale <= 0 {
		return 1
	}
	return p.TimeoutScale
}

func (p *Provider) metrics() []Metric {
	return append(append([]Metric(nil), Metrics...), p.Metrics...)
}

func (p *Provider) metric(name string) (Metric, bool) {
	return lo.Find(p.metrics(), func(m Metric) bool { return m.Name == name })
}
