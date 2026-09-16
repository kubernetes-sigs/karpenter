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

package disruption

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

func TestRepairTerminationGracePeriodIntents(t *testing.T) {
	t.Run("filters unannotated NodeClaims", func(t *testing.T) {
		annotated := &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: "annotated",
			Annotations: map[string]string{
				v1.NodeClaimRepairTerminationGracePeriodAnnotationKey: "5m",
			},
		}}
		nodes := []*state.StateNode{
			{NodeClaim: &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "unannotated"}}},
			{NodeClaim: annotated},
			{},
		}

		intents := repairTerminationGracePeriodIntents(nodes)
		if len(intents) != 1 || intents[0] != annotated {
			t.Fatalf("expected only the annotated NodeClaim, got %#v", intents)
		}
	})
	t.Run("bounds cleanup work per pass", func(t *testing.T) {
		nodes := make([]*state.StateNode, intentCleanupBatchSize+1)
		for i := range nodes {
			nodes[i] = &state.StateNode{NodeClaim: &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("annotated-%d", i),
				Annotations: map[string]string{
					v1.NodeClaimRepairTerminationGracePeriodAnnotationKey: "5m",
				},
			}}}
		}

		intents := repairTerminationGracePeriodIntents(nodes)
		if len(intents) != intentCleanupBatchSize {
			t.Fatalf("expected at most %d cleanup intents, got %d", intentCleanupBatchSize, len(intents))
		}
	})
}
