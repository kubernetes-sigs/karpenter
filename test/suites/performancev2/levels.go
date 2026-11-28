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

package performancev2

import "sigs.k8s.io/karpenter/test/suites/performancev2/perf"

// scenarioLevels are one scenario's levels for both core providers, kept in levels_<scenario>.go.
type scenarioLevels struct{ KWOK, KWOKCI perf.Levels }

var migrated = []scenarioLevels{
	replacementLevels,
	podsWideLevels,
	podsBalancedLevels,
	podsBalancedChurnLevels,
	podsBalancedHeterogeneousLevels,
	nodepoolsDiverseLevels,
	nodepoolsTargetedLevels,
	topologyHostnameLevels,
}

func withMigrated(base perf.Levels, pick func(scenarioLevels) perf.Levels) perf.Levels {
	for _, l := range migrated {
		base = base.With(pick(l))
	}
	return base
}
