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

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/events"
)

// RankingCompletedEvent creates an event indicating that node ranking has completed successfully
func RankingCompletedEvent(nodeCount int) events.Event {
	return events.Event{
		InvolvedObject: &corev1.Namespace{},
		Type:           corev1.EventTypeNormal,
		Reason:         events.PodDeletionCostRankingCompleted,
		Message:        fmt.Sprintf("Ranked %d nodes using PodCount strategy", nodeCount),
		DedupeValues:   []string{"PodCount"},
	}
}

// UpdateFailedEvent creates an event indicating that updating pod deletion cost annotation failed
func UpdateFailedEvent(pod *corev1.Pod, err error) events.Event {
	return events.Event{
		InvolvedObject: pod,
		Type:           corev1.EventTypeWarning,
		Reason:         events.PodDeletionCostUpdateFailed,
		Message:        fmt.Sprintf("Failed to update pod deletion cost: %s", err.Error()),
		DedupeValues:   []string{string(pod.UID)},
	}
}

// DisabledEvent creates an event indicating that the pod deletion cost feature has been disabled due to an error
func DisabledEvent(reason string) events.Event {
	return events.Event{
		InvolvedObject: &corev1.Namespace{},
		Type:           corev1.EventTypeWarning,
		Reason:         events.PodDeletionCostDisabled,
		Message:        fmt.Sprintf("Pod deletion cost management disabled: %s", reason),
		DedupeValues:   []string{reason},
	}
}

// ThirdPartyConflictEvent creates a warning event when a third party modified a Karpenter-managed deletion cost
func ThirdPartyConflictEvent(pod *corev1.Pod) events.Event {
	return events.Event{
		InvolvedObject: pod,
		Type:           corev1.EventTypeWarning,
		Reason:         events.PodDeletionCostThirdPartyConflict,
		Message:        "Pod deletion cost annotation was externally modified; Karpenter is releasing management of this pod",
		DedupeValues:   []string{string(pod.UID)},
	}
}
