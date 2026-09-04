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

package deletioncost

// The identifiers below are exported for the external deletioncost_test
// package so metric-assertion specs can reference the private collectors.
// Files ending in _test.go are compiled only into the test binary, so
// these names do not leak into production callers.
var (
	NodesRankedMetric           = nodesRanked
	PodsUpdatedTotalMetric      = podsUpdatedTotal
	ReconcileSkippedTotalMetric = reconcileSkippedTotal
)

// Label values used by the pods_updated_total counter, exported for tests.
const (
	ResultLabel            = resultLabel
	ResultUpdated          = "updated"
	ResultSkippedUnchanged = "skipped_unchanged"
	ResultError            = "error"
)
