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

// UpdateFailedEvent creates a Warning event indicating that a pod's
// pod-deletion-cost annotation failed to update. The event is keyed by pod UID
// so the same pod's failures dedupe rather than spamming.
func UpdateFailedEvent(pod *corev1.Pod, err error) events.Event {
	return events.Event{
		InvolvedObject: pod,
		Type:           corev1.EventTypeWarning,
		Reason:         events.PodDeletionCostUpdateFailed,
		Message:        fmt.Sprintf("Failed to update pod deletion cost: %s", err.Error()),
		DedupeValues:   []string{string(pod.UID)},
	}
}
