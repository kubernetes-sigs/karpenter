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

// Package events defines the NodeClaim-targeted events emitted across the reboot lifecycle. Reboot state
// lives on the NodeClaim (the Rebooting condition), so its events are emitted there too — co-located with
// the condition rather than split across the Node.
package events

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
)

// dedupeTimeout must exceed the reboot observation window so a per-phase event fires once, not once per
// reconcile of the observe loop.
const dedupeTimeout = 30 * time.Minute

func event(nodeClaim *v1.NodeClaim, eventType, reason, message string) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           eventType,
		Reason:         reason,
		Message:        message,
		// Key dedupe on the reboot episode (the Rebooting condition's transition time), not just the
		// NodeClaim UID, so a subsequent reboot on the same node isn't falsely deduped as the previous one.
		DedupeValues:  []string{string(nodeClaim.UID), rebootEpisode(nodeClaim)},
		DedupeTimeout: dedupeTimeout,
	}
}

// rebootEpisode identifies the current reboot episode by the Rebooting condition's transition time.
func rebootEpisode(nodeClaim *v1.NodeClaim) string {
	if cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeRebooting); cond != nil {
		return cond.LastTransitionTime.Format(time.RFC3339Nano)
	}
	return ""
}

func RebootObserved(nodeClaim *v1.NodeClaim) events.Event {
	return event(nodeClaim, corev1.EventTypeNormal, events.RebootObserved, "New boot observed (bootID changed); awaiting node readiness")
}

func RebootFailed(nodeClaim *v1.NodeClaim, message string) events.Event {
	return event(nodeClaim, corev1.EventTypeWarning, events.RebootFailed, message)
}
