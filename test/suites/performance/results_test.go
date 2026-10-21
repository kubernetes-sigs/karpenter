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

package performance

import "testing"

func TestRenderSummary(t *testing.T) {
	got := renderSummary([]thresholdCheck{
		{key: "basic/scaleOut", metric: "heap_mb", comparator: "<", coreThreshold: 120, threshold: 250, measured: 210.628, pass: true},
		{key: "basic/consolidation", metric: "cpu_util", comparator: ">", coreThreshold: 0.5, threshold: 0.5, measured: 0.41234, pass: false},
	})
	want := `## Karpenter performance: FAIL (1/2 threshold checks passed)

| Phase | Metric | Measured | Threshold | Default | Result |
| --- | --- | ---: | ---: | ---: | --- |
| basic/scaleOut | heap_mb | 210.6 | < 250 | 120 | pass |
| basic/consolidation | cpu_util | 0.4123 | > 0.5 | 0.5 | **FAIL** |
`
	if got != want {
		t.Errorf("renderSummary() =\n%s\nwant\n%s", got, want)
	}
}
