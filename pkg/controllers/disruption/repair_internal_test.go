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
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
)

func TestRepairPolicyDecisionLogsOnlyOnTransitions(t *testing.T) {
	repair := &Repair{decisionLogs: make(map[types.UID]repairDecisionLogState)}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: types.UID("node-uid")}}
	now := time.Unix(1, 0)

	if !repair.recordRepairPolicyDecisionLog(node, "waiting", now) {
		t.Fatal("expected the first waiting decision to be logged")
	}
	if repair.recordRepairPolicyDecisionLog(node, "waiting", now.Add(time.Minute)) {
		t.Fatal("expected an unchanged waiting decision to be deduplicated")
	}
	if !repair.recordRepairPolicyDecisionLog(node, "eligible", now.Add(2*time.Minute)) {
		t.Fatal("expected the transition to eligible to be logged")
	}

	repair.clearRepairPolicyDecisionLog(node)
	if !repair.recordRepairPolicyDecisionLog(node, "waiting", now.Add(3*time.Minute)) {
		t.Fatal("expected a decision to be logged again after the condition clears")
	}
}

func TestRepairPolicyDecisionLogsPruneDeletedNodes(t *testing.T) {
	repair := &Repair{
		decisionLogs: map[types.UID]repairDecisionLogState{
			"deleted": {fingerprint: "waiting", lastSeen: time.Unix(1, 0)},
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: types.UID("node-uid")}}
	now := time.Unix(1, 0).Add(repairDecisionLogRetention + time.Second)

	repair.recordRepairPolicyDecisionLog(node, "waiting", now)
	if _, ok := repair.decisionLogs["deleted"]; ok {
		t.Fatal("expected stale decision-log state for a deleted node to be pruned")
	}
}

func TestRepairPolicyDecisionLoggingDoesNoWorkWhenDisabled(t *testing.T) {
	matcher, err := health.NewRepairPolicyMatcher(
		[]cloudprovider.RepairPolicy{{
			ConditionType:   "BadNode",
			ConditionStatus: corev1.ConditionFalse,
			Action:          cloudprovider.ReplaceNode,
		}},
		sets.New(cloudprovider.ReplaceNode),
	)
	if err != nil {
		t.Fatalf("creating repair policy matcher, %v", err)
	}
	repair := &Repair{
		policyMatcher: matcher,
		decisionLogs:  make(map[types.UID]repairDecisionLogState),
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node", UID: types.UID("node-uid")},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:               "BadNode",
			Status:             corev1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(time.Unix(1, 0)),
		}}},
	}
	ctx := log.IntoContext(context.Background(), logr.Discard())

	repair.logRepairPolicyDecisions(ctx, node, time.Unix(2, 0))

	if len(repair.decisionLogs) != 0 {
		t.Fatalf("expected disabled decision logging to avoid tracking state, got %d entries", len(repair.decisionLogs))
	}
}
