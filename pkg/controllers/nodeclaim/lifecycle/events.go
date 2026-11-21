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

package lifecycle

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/flowcontrol"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
)

// InsufficientCapacityErrorPodRateLimiter is a pointer so it rate-limits across events
var InsufficientCapacityErrorPodRateLimiter = flowcontrol.NewTokenBucketRateLimiter(5, 10)

func InsufficientCapacityErrorEvent(nodeClaim *v1.NodeClaim, err error) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Reason:         events.InsufficientCapacityError,
		Message:        truncateMessage(fmt.Sprintf("NodeClaim %s event: %s", nodeClaim.Name, err.Error())),
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}

func InsufficientCapacityErrorPodEvent(pod *corev1.Pod, nodeClaim *v1.NodeClaim, err error) events.Event {
	return events.Event{
		InvolvedObject: pod,
		Type:           corev1.EventTypeWarning,
		Reason:         events.InsufficientCapacityError,
		Message:        truncateMessage(fmt.Sprintf("NodeClaim %s nominated for this pod failed to launch: %s", nodeClaim.Name, err.Error())),
		DedupeValues:   []string{string(pod.UID)},
		RateLimiter:    InsufficientCapacityErrorPodRateLimiter,
	}
}

// Deprecated: unused by Karpenter and will be removed in v1.18.
func NodeClassNotReadyEvent(nodeClaim *v1.NodeClaim, err error) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Reason:         events.NodeClassNotReady,
		Message:        truncateMessage(fmt.Sprintf("NodeClaim %s event: %s", nodeClaim.Name, err.Error())),
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}

func RebootDepartingPodsTerminatingEvent(nodeClaim *v1.NodeClaim, msg string) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Reason:         events.RebootDepartingPodsTerminating,
		Message:        truncateMessage(fmt.Sprintf("Waiting to re-initialize after reboot: %s", msg)),
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}

// RebootDepartingPodTerminatingEvent is published on a pod holding its node back from re-initializing after a reboot,
// where the pod's owner, who can clear what's holding it, will look.
func RebootDepartingPodTerminatingEvent(pod *corev1.Pod, node *corev1.Node, now time.Time) events.Event {
	var why string
	switch {
	case len(pod.Finalizers) > 0:
		why = fmt.Sprintf(", pending finalizers %s", strings.Join(pod.Finalizers, ", "))
	case pod.DeletionTimestamp != nil && now.After(pod.DeletionTimestamp.Time):
		why = ", past its termination grace period"
	}
	return events.Event{
		InvolvedObject: pod,
		Type:           corev1.EventTypeWarning,
		Reason:         events.RebootDepartingPodsTerminating,
		Message:        truncateMessage(fmt.Sprintf("Pod deleted before node %s rebooted is still terminating%s, blocking the node from re-initializing", node.Name, why)),
		DedupeValues:   []string{string(pod.UID)},
	}
}

func UnregisteredTaintMissingEvent(nodeClaim *v1.NodeClaim) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Reason:         events.UnregisteredTaintMissing,
		Message:        fmt.Sprintf("Missing %s taint which prevents registration related race conditions on Karpenter-managed nodes", v1.UnregisteredTaintKey),
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}
