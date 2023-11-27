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

package scheduling

import (
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/flowcontrol"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/events"
)

// PodNominationRateLimiter is a pointer so it rate-limits across events
var PodNominationRateLimiter = flowcontrol.NewTokenBucketRateLimiter(5, 10)

func NominatePodEvent(pod *v1.Pod, node *v1.Node, nodeClaim *v1beta1.NodeClaim) events.Event {
	var info []string
	if nodeClaim != nil {
		info = append(info, fmt.Sprintf("nodeclaim/%s", nodeClaim.GetName()))
	}
	if node != nil {
		info = append(info, fmt.Sprintf("node/%s", node.Name))
	}
	return events.Event{
		InvolvedObject: pod,
		Type:           v1.EventTypeNormal,
		Reason:         "Nominated",
		Message:        fmt.Sprintf("Pod should schedule on: %s", strings.Join(info, ", ")),
		DedupeValues:   []string{string(pod.UID)},
		RateLimiter:    PodNominationRateLimiter,
	}
}

func PodFailedToScheduleEvent(pod *v1.Pod, err error) events.Event {
	return events.Event{
		InvolvedObject: pod,
		Type:           v1.EventTypeWarning,
		Reason:         "FailedScheduling",
		Message:        fmt.Sprintf("Failed to schedule pod, %s", err),
		DedupeValues:   []string{string(pod.UID)},
		DedupeTimeout:  5 * time.Minute,
	}
}
