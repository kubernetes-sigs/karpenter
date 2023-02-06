/*
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

package events

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/events"
)

// PodNominationRateLimiter is a pointer so it rate-limits across events
var PodNominationRateLimiter = flowcontrol.NewTokenBucketRateLimiter(5, 10)

// PodNominationRateLimiterForMachine is a pointer so it rate-limits across events
var PodNominationRateLimiterForMachine = flowcontrol.NewTokenBucketRateLimiter(5, 10)

func NominatePod(pod *v1.Pod, node *v1.Node) events.Event {
	return events.Event{
		InvolvedObject: pod,
		Type:           v1.EventTypeNormal,
		Reason:         "Nominated",
		Message:        fmt.Sprintf("Pod should schedule on: %s", node.Name),
		DedupeValues:   []string{string(pod.UID), node.Name},
		RateLimiter:    PodNominationRateLimiter,
	}
}

func NominatePodForMachine(pod *v1.Pod, machine *v1alpha5.Machine) events.Event {
	return events.Event{
		InvolvedObject: pod,
		Type:           v1.EventTypeNormal,
		Reason:         "Nominated",
		Message:        fmt.Sprintf("Pod should schedule on node associated with machine: %s", machine.Name),
		DedupeValues:   []string{string(pod.UID), machine.Name},
		RateLimiter:    PodNominationRateLimiterForMachine,
	}
}

func PodFailedToSchedule(pod *v1.Pod, err error) events.Event {
	return events.Event{
		InvolvedObject: pod,
		Type:           v1.EventTypeWarning,
		Reason:         "FailedScheduling",
		Message:        fmt.Sprintf("Failed to schedule pod, %s", err),
		DedupeValues:   []string{string(pod.UID), err.Error()},
	}
}
