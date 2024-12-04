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

package termination

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
)

func PodDeletedEvent(pod *corev1.Pod, gracePeriodSeconds *int64, nodeGracePeriodTerminationTime *time.Time) events.Event {
	return events.Event{
		InvolvedObject: pod,
		Type:           corev1.EventTypeNormal,
		Reason:         "Disrupted",
		Message:        fmt.Sprintf("Deleting the pod to accommodate the terminationTime %v of the node. The pod was granted %v seconds of grace-period of its %v terminationGracePeriodSeconds. This bypasses the PDB of the pod and the do-not-disrupt annotation.", *nodeGracePeriodTerminationTime, *gracePeriodSeconds, pod.Spec.TerminationGracePeriodSeconds),
		DedupeValues:   []string{pod.Namespace, pod.Name},
	}
}

func NodeDrainFailedEvent(node *corev1.Node, err error) events.Event {
	return events.Event{
		InvolvedObject: node,
		Type:           corev1.EventTypeWarning,
		Reason:         "FailedDraining",
		Message:        fmt.Sprintf("Failed to drain node, %s", err),
		DedupeValues:   []string{node.Name},
	}
}

func NodeAwaitingVolumeDetachmentEvent(node *corev1.Node, volumeAttachments ...*storagev1.VolumeAttachment) events.Event {
	return events.Event{
		InvolvedObject: node,
		Type:           corev1.EventTypeNormal,
		Reason:         "AwaitingVolumeDetachment",
		Message:        fmt.Sprintf("Awaiting deletion of %d VolumeAttachments bound to node", len(volumeAttachments)),
		DedupeValues:   []string{node.Name},
	}
}

func NodeTerminationGracePeriodExpiringEvent(node *corev1.Node, t time.Time) events.Event {
	return events.Event{
		InvolvedObject: node,
		Type:           corev1.EventTypeWarning,
		Reason:         "TerminationGracePeriodExpiring",
		Message:        fmt.Sprintf("All pods will be deleted by %s", t.Format(time.RFC3339)),
		DedupeValues:   []string{node.Name},
	}
}

func NodeClaimTerminationGracePeriodExpiringEvent(nodeClaim *v1.NodeClaim, terminationTime string) events.Event {
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Reason:         "TerminationGracePeriodExpiring",
		Message:        fmt.Sprintf("All pods will be deleted by %s", terminationTime),
		DedupeValues:   []string{nodeClaim.Name},
	}
}
