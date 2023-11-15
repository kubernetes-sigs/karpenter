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

package pod

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

func IsProvisionable(pod *v1.Pod) bool {
	return !IsScheduled(pod) &&
		!IsPreempting(pod) &&
		FailedToSchedule(pod) &&
		!IsOwnedByDaemonSet(pod) &&
		!IsOwnedByNode(pod)
}

func FailedToSchedule(pod *v1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodScheduled && condition.Reason == v1.PodReasonUnschedulable {
			return true
		}
	}
	return false
}

func IsScheduled(pod *v1.Pod) bool {
	return pod.Spec.NodeName != ""
}

func IsPreempting(pod *v1.Pod) bool {
	return pod.Status.NominatedNodeName != ""
}

func IsTerminal(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded
}

func IsTerminating(pod *v1.Pod) bool {
	return pod.DeletionTimestamp != nil
}

func IsOwnedByDaemonSet(pod *v1.Pod) bool {
	return IsOwnedBy(pod, []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "DaemonSet"},
	})
}

// IsOwnedByNode returns true if the pod is a static pod owned by a specific node
func IsOwnedByNode(pod *v1.Pod) bool {
	return IsOwnedBy(pod, []schema.GroupVersionKind{
		{Version: "v1", Kind: "Node"},
	})
}

func IsPodReady(pod *v1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return true
		}
	}
	return false
}

func IsOwnedBy(pod *v1.Pod, gvks []schema.GroupVersionKind) bool {
	for _, ignoredOwner := range gvks {
		for _, owner := range pod.ObjectMeta.OwnerReferences {
			if owner.APIVersion == ignoredOwner.GroupVersion().String() && owner.Kind == ignoredOwner.Kind {
				return true
			}
		}
	}
	return false
}

func HasDoNotDisrupt(pod *v1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}
	// TODO Remove checking do-not-evict as part of v1
	return pod.Annotations[v1alpha5.DoNotEvictPodAnnotationKey] == "true" ||
		pod.Annotations[v1beta1.DoNotDisruptAnnotationKey] == "true"
}

// ToleratesUnschedulableTaint returns true if the pod tolerates node.kubernetes.io/unschedulable taint
func ToleratesUnschedulableTaint(pod *v1.Pod) bool {
	return (scheduling.Taints{{Key: v1.TaintNodeUnschedulable, Effect: v1.TaintEffectNoSchedule}}).Tolerates(pod) == nil
}

// ToleratesDisruptionNoScheduleTaint returns true if the pod tolerates karpenter.sh/disruption:NoSchedule=Disrupting taint
func ToleratesDisruptionNoScheduleTaint(pod *v1.Pod) bool {
	return scheduling.Taints([]v1.Taint{v1beta1.DisruptionNoScheduleTaint}).Tolerates(pod) == nil
}

// HasRequiredPodAntiAffinity returns true if a non-empty PodAntiAffinity/RequiredDuringSchedulingIgnoredDuringExecution
// is defined in the pod spec
func HasRequiredPodAntiAffinity(pod *v1.Pod) bool {
	return HasPodAntiAffinity(pod) &&
		len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0
}

// HasPodAntiAffinity returns true if a non-empty PodAntiAffinity is defined in the pod spec
func HasPodAntiAffinity(pod *v1.Pod) bool {
	return pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAntiAffinity != nil &&
		(len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 ||
			len(pod.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 0)
}
