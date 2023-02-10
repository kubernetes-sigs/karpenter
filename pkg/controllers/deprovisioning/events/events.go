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
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/aws/karpenter-core/pkg/events"
)

func TerminatingNode(node *v1.Node, reason string) events.Event {
	return events.Event{
		InvolvedObject: node,
		Type:           v1.EventTypeNormal,
		Reason:         "DeprovisioningTerminatingNode",
		Message:        fmt.Sprintf("Deprovisioning node via %s", reason),
		DedupeValues:   []string{node.Name, reason},
	}
}

func LaunchingNode(node *v1.Node, reason string) events.Event {
	return events.Event{
		InvolvedObject: node,
		Type:           v1.EventTypeNormal,
		Reason:         "DeprovisioningLaunchingNode",
		Message:        fmt.Sprintf("Launching node for %s", reason),
		DedupeValues:   []string{node.Name, reason},
	}
}

func WaitingOnReadiness(node *v1.Node) events.Event {
	return events.Event{
		InvolvedObject: node,
		Type:           v1.EventTypeNormal,
		Reason:         "DeprovisioningWaitingReadiness",
		Message:        "Waiting on readiness to continue deprovisioning",
		DedupeValues:   []string{node.Name},
	}
}

func WaitingOnDeletion(node *v1.Node) events.Event {
	return events.Event{
		InvolvedObject: node,
		Type:           v1.EventTypeNormal,
		Reason:         "DeprovisioningWaitingDeletion",
		Message:        "Waiting on deletion to continue deprovisioning",
		DedupeValues:   []string{node.Name},
	}
}

func UnconsolidatableReason(node *v1.Node, reason string) events.Event {
	return events.Event{
		InvolvedObject: node,
		Type:           v1.EventTypeNormal,
		Reason:         "Unconsolidatable",
		Message:        reason,
		DedupeValues:   []string{node.Name},
		DedupeTimeout:  time.Minute * 15,
	}
}
