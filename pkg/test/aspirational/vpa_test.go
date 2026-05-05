//go:build test_aspirational

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

package aspirational

import (
	"testing"
)

// TestInPlacePodResize_SchedulerAwareness documents the scenario from
// https://github.com/kubernetes-sigs/karpenter/issues/829
//
// When a pod is resized in place (KEP-1287 InPlacePodVerticalScaling), the
// scheduler and consolidation logic should account for the new resource
// requests. Currently Karpenter does not react to in-place resize events,
// which can lead to overcommitment or missed consolidation opportunities.
//
// This test will pass once Karpenter handles pod resize status changes.
func TestInPlacePodResize_SchedulerAwareness(t *testing.T) {
	t.Skip("aspirational: blocked on in-place pod resize support (#829)")
}
