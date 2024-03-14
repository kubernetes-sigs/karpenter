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

package events

import (
	"fmt"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/eventrecorder"
)

func Launching(nodeClaim *v1beta1.NodeClaim, reason string) eventrecorder.Event {
	return eventrecorder.Event{
		InvolvedObject: nodeClaim,
		Type:           v1.EventTypeNormal,
		Reason:         "DisruptionLaunching",
		Message:        fmt.Sprintf("Launching NodeClaim: %s", cases.Title(language.Und, cases.NoLower).String(reason)),
		DedupeValues:   []string{string(nodeClaim.UID), reason},
	}
}

func WaitingOnReadiness(nodeClaim *v1beta1.NodeClaim) eventrecorder.Event {
	return eventrecorder.Event{
		InvolvedObject: nodeClaim,
		Type:           v1.EventTypeNormal,
		Reason:         "DisruptionWaitingReadiness",
		Message:        "Waiting on readiness to continue disruption",
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}

func WaitingOnDeletion(nodeClaim *v1beta1.NodeClaim) eventrecorder.Event {
	return eventrecorder.Event{
		InvolvedObject: nodeClaim,
		Type:           v1.EventTypeNormal,
		Reason:         "DisruptionWaitingDeletion",
		Message:        "Waiting on deletion to continue disruption",
		DedupeValues:   []string{string(nodeClaim.UID)},
	}
}

func Terminating(node *v1.Node, nodeClaim *v1beta1.NodeClaim, reason string) []eventrecorder.Event {
	return []eventrecorder.Event{
		{
			InvolvedObject: node,
			Type:           v1.EventTypeNormal,
			Reason:         "DisruptionTerminating",
			Message:        fmt.Sprintf("Disrupting Node: %s", cases.Title(language.Und, cases.NoLower).String(reason)),
			DedupeValues:   []string{string(node.UID), reason},
		},
		{
			InvolvedObject: nodeClaim,
			Type:           v1.EventTypeNormal,
			Reason:         "DisruptionTerminating",
			Message:        fmt.Sprintf("Disrupting NodeClaim: %s", cases.Title(language.Und, cases.NoLower).String(reason)),
			DedupeValues:   []string{string(nodeClaim.UID), reason},
		},
	}
}

// Unconsolidatable is an event that informs the user that a NodeClaim/Node combination cannot be consolidated
// due to the state of the NodeClaim/Node or due to some state of the pods that are scheduled to the NodeClaim/Node
func Unconsolidatable(node *v1.Node, nodeClaim *v1beta1.NodeClaim, reason string) []eventrecorder.Event {
	return []eventrecorder.Event{
		{
			InvolvedObject: node,
			Type:           v1.EventTypeNormal,
			Reason:         "Unconsolidatable",
			Message:        reason,
			DedupeValues:   []string{string(node.UID)},
			DedupeTimeout:  time.Minute * 15,
		},
		{
			InvolvedObject: nodeClaim,
			Type:           v1.EventTypeNormal,
			Reason:         "Unconsolidatable",
			Message:        reason,
			DedupeValues:   []string{string(nodeClaim.UID)},
			DedupeTimeout:  time.Minute * 15,
		},
	}
}

// Blocked is an event that informs the user that a NodeClaim/Node combination is blocked on deprovisioning
// due to the state of the NodeClaim/Node or due to some state of the pods that are scheduled to the NodeClaim/Node
func Blocked(node *v1.Node, nodeClaim *v1beta1.NodeClaim, reason string) []eventrecorder.Event {
	return []eventrecorder.Event{
		{
			InvolvedObject: node,
			Type:           v1.EventTypeNormal,
			Reason:         "DisruptionBlocked",
			Message:        fmt.Sprintf("Cannot disrupt Node: %s", reason),
			DedupeValues:   []string{string(node.UID)},
		},
		{
			InvolvedObject: nodeClaim,
			Type:           v1.EventTypeNormal,
			Reason:         "DisruptionBlocked",
			Message:        fmt.Sprintf("Cannot disrupt NodeClaim: %s", reason),
			DedupeValues:   []string{string(nodeClaim.UID)},
		},
	}
}

func NodePoolBlocked(nodePool *v1beta1.NodePool) eventrecorder.Event {
	return eventrecorder.Event{
		InvolvedObject: nodePool,
		Type:           v1.EventTypeNormal,
		Reason:         "DisruptionBlocked",
		Message:        "No allowed disruptions due to blocking budget",
		DedupeValues:   []string{string(nodePool.UID)},
		// Set a small timeout as a NodePool's disruption budget can change every minute.
		DedupeTimeout: 1 * time.Minute,
	}
}
