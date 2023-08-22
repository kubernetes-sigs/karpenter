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

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	v1 "k8s.io/api/core/v1"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/events"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
)

func Launching(nodeClaim *v1beta1.NodeClaim, reason string) events.Event {
	if nodeClaim.IsMachine {
		machine := machineutil.NewFromNodeClaim(nodeClaim)
		return events.Event{
			InvolvedObject: machine,
			Type:           v1.EventTypeNormal,
			Reason:         "DeprovisioningLaunching",
			Message:        fmt.Sprintf("Launching Machine: %s", cases.Title(language.Und, cases.NoLower).String(reason)),
			DedupeValues:   []string{string(machine.UID), reason},
		}
	}
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           v1.EventTypeNormal,
		Reason:         "DeprovisioningLaunching",
		Message:        fmt.Sprintf("Launching NodeClaim: %s", cases.Title(language.Und, cases.NoLower).String(reason)),
		DedupeValues:   []string{string(nodeClaim.UID), reason},
	}
}

func WaitingOnReadiness(nodeClaim *v1beta1.NodeClaim) events.Event {
	if nodeClaim.IsMachine {
		machine := machineutil.NewFromNodeClaim(nodeClaim)
		return events.Event{
			InvolvedObject: machine,
			Type:           v1.EventTypeNormal,
			Reason:         "DeprovisioningWaitingReadiness",
			Message:        "Waiting on readiness to continue deprovisioning",
			DedupeValues:   []string{string(machine.UID)},
		}
	}
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           v1.EventTypeNormal,
		Reason:         "DeprovisioningWaitingReadiness",
		Message:        "Waiting on readiness to continue deprovisioning",
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}

func WaitingOnDeletion(nodeClaim *v1beta1.NodeClaim) events.Event {
	if nodeClaim.IsMachine {
		machine := machineutil.NewFromNodeClaim(nodeClaim)
		return events.Event{
			InvolvedObject: machine,
			Type:           v1.EventTypeNormal,
			Reason:         "DeprovisioningWaitingDeletion",
			Message:        "Waiting on deletion to continue deprovisioning",
			DedupeValues:   []string{string(machine.UID)},
		}
	}
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           v1.EventTypeNormal,
		Reason:         "DeprovisioningWaitingDeletion",
		Message:        "Waiting on deletion to continue deprovisioning",
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}

func Terminating(node *v1.Node, nodeClaim *v1beta1.NodeClaim, reason string) []events.Event {
	evts := []events.Event{
		{
			InvolvedObject: node,
			Type:           v1.EventTypeNormal,
			Reason:         "DeprovisioningTerminating",
			Message:        fmt.Sprintf("Deprovisioning Node: %s", cases.Title(language.Und, cases.NoLower).String(reason)),
			DedupeValues:   []string{string(node.UID), reason},
		},
	}
	if nodeClaim.IsMachine {
		machine := machineutil.NewFromNodeClaim(nodeClaim)
		evts = append(evts, events.Event{
			InvolvedObject: machine,
			Type:           v1.EventTypeNormal,
			Reason:         "DeprovisioningTerminating",
			Message:        fmt.Sprintf("Deprovisioning Machine: %s", cases.Title(language.Und, cases.NoLower).String(reason)),
			DedupeValues:   []string{string(machine.UID), reason},
		})
	} else {
		evts = append(evts, events.Event{
			InvolvedObject: nodeClaim,
			Type:           v1.EventTypeNormal,
			Reason:         "DeprovisioningTerminating",
			Message:        fmt.Sprintf("Deprovisioning NodeClaim: %s", cases.Title(language.Und, cases.NoLower).String(reason)),
			DedupeValues:   []string{string(nodeClaim.UID), reason},
		})
	}
	return evts
}

// Unconsolidatable is an event that informs the user that a Machine/Node combination cannot be consolidated
// due to the state of the Machine/Node or due to some state of the pods that are scheduled to the Machine/Node
func Unconsolidatable(node *v1.Node, nodeClaim *v1beta1.NodeClaim, reason string) []events.Event {
	evts := []events.Event{
		{
			InvolvedObject: node,
			Type:           v1.EventTypeNormal,
			Reason:         "Unconsolidatable",
			Message:        reason,
			DedupeValues:   []string{string(node.UID)},
			DedupeTimeout:  time.Minute * 15,
		},
	}
	if nodeClaim.IsMachine {
		machine := machineutil.NewFromNodeClaim(nodeClaim)
		evts = append(evts, events.Event{
			InvolvedObject: machine,
			Type:           v1.EventTypeNormal,
			Reason:         "Unconsolidatable",
			Message:        reason,
			DedupeValues:   []string{string(machine.UID)},
			DedupeTimeout:  time.Minute * 15,
		})
	} else {
		evts = append(evts, events.Event{
			InvolvedObject: nodeClaim,
			Type:           v1.EventTypeNormal,
			Reason:         "Unconsolidatable",
			Message:        reason,
			DedupeValues:   []string{string(nodeClaim.UID)},
			DedupeTimeout:  time.Minute * 15,
		})
	}
	return evts
}

// Blocked is an event that informs the user that a Machine/Node combination is blocked on deprovisioning
// due to the state of the Machine/Node or due to some state of the pods that are scheduled to the Machine/Node
func Blocked(node *v1.Node, nodeClaim *v1beta1.NodeClaim, reason string) []events.Event {
	evts := []events.Event{
		{
			InvolvedObject: node,
			Type:           v1.EventTypeNormal,
			Reason:         "DeprovisioningBlocked",
			Message:        fmt.Sprintf("Cannot deprovision Node: %s", reason),
			DedupeValues:   []string{string(node.UID)},
		},
	}
	if nodeClaim.IsMachine {
		machine := machineutil.NewFromNodeClaim(nodeClaim)
		evts = append(evts, events.Event{
			InvolvedObject: machine,
			Type:           v1.EventTypeNormal,
			Reason:         "DeprovisioningBlocked",
			Message:        fmt.Sprintf("Cannot deprovision Machine: %s", reason),
			DedupeValues:   []string{string(machine.UID)},
		})
	} else {
		evts = append(evts, events.Event{
			InvolvedObject: nodeClaim,
			Type:           v1.EventTypeNormal,
			Reason:         "DeprovisioningBlocked",
			Message:        fmt.Sprintf("Cannot deprovision NodeClaim: %s", reason),
			DedupeValues:   []string{string(nodeClaim.UID)},
		})
	}
	return evts
}
