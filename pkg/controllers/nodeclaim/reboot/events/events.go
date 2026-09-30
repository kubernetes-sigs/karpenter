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

// Package events defines reboot lifecycle events emitted on the `NodeClaim`,
// alongside the `Rebooting` condition.
package events

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
)

// Exceed the reboot observation window so each phase event fires once.
const dedupeTimeout = 30 * time.Minute

func event(nodeClaim *v1.NodeClaim, eventType, reason, message string) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           eventType,
		Reason:         reason,
		Message:        message,
		// Include the reboot episode so a later reboot on the same NodeClaim isn't deduped.
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
