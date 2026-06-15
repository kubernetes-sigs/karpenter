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

package capacitybuffer

import (
	"math"

	v1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1alpha1"
)

// calculateLimitReplicas determines how many buffer pods fit within the given
// resource budget. For each resource present in both limits and pod requests,
// it floors(limit/request). Returns the minimum across all matched resources,
// or -1 if no resources overlap (meaning limits don't constrain anything).
func calculateLimitReplicas(limits v1.ResourceList, podSpec *v1.PodSpec) int32 {
	totalRequests := totalPodRequests(podSpec)
	if len(totalRequests) == 0 {
		return -1
	}

	minReplicas := int32(math.MaxInt32)
	matched := false
	for resourceName, limit := range limits {
		request, ok := totalRequests[resourceName]
		if !ok || request.IsZero() {
			continue
		}
		matched = true
		n := int64(math.Floor(float64(limit.MilliValue()) / float64(request.MilliValue())))
		clamped := int32(min(n, math.MaxInt32)) //nolint:gosec // clamped to MaxInt32
		if clamped < minReplicas {
			minReplicas = clamped
		}
	}

	if !matched {
		return -1
	}
	return minReplicas
}

// totalPodRequests computes effective resource requests for one pod following
// Kubernetes scheduling semantics (KEP-753 sidecar containers):
//
//	reqs = sum(regular containers) + sum(sidecar init containers)
//	initUse(i) = resources(init[i]) + sum(sidecars started before i)
//	effective = max(reqs, max(initUse...))
//
// Sidecar init containers have restartPolicy=Always and run concurrently with
// regular containers, so their resources are summed rather than max'd.
func totalPodRequests(podSpec *v1.PodSpec) v1.ResourceList {
	reqs := v1.ResourceList{}
	for _, c := range podSpec.Containers {
		addResourceList(reqs, c.Resources.Requests)
	}

	restartableInitReqs := v1.ResourceList{}
	initContainerReqs := v1.ResourceList{}

	for _, c := range podSpec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == v1.ContainerRestartPolicyAlways {
			// Sidecar: runs for the lifetime of the pod, sum with regular containers
			addResourceList(reqs, c.Resources.Requests)
			addResourceList(restartableInitReqs, c.Resources.Requests)
		} else {
			// Regular init: initUse(i) = own resources + all sidecars started before it
			initUse := v1.ResourceList{}
			addResourceList(initUse, c.Resources.Requests)
			addResourceList(initUse, restartableInitReqs)
			maxResourceList(initContainerReqs, initUse)
		}
	}

	maxResourceList(reqs, initContainerReqs)
	return reqs
}

func addResourceList(dest v1.ResourceList, src v1.ResourceList) {
	for name, qty := range src {
		existing := dest[name]
		existing.Add(qty)
		dest[name] = existing
	}
}

func maxResourceList(dest v1.ResourceList, src v1.ResourceList) {
	for name, qty := range src {
		if existing, ok := dest[name]; !ok || qty.Cmp(existing) > 0 {
			dest[name] = qty.DeepCopy()
		}
	}
}

// calculatePercentageReplicas computes ceil(scalableReplicas * percentage / 100)
// with a floor of 1 when both inputs are positive (the API guarantees at least
// one buffer chunk if the user asks for any percentage of a running workload).
func calculatePercentageReplicas(scalableReplicas int32, percentage int32) int32 {
	pctReplicas := int32(math.Ceil(float64(scalableReplicas) * float64(percentage) / 100.0))
	if pctReplicas < 1 && percentage > 0 && scalableReplicas > 0 {
		pctReplicas = 1
	}
	return pctReplicas
}

func setCondition(cb *autoscalingv1alpha1.CapacityBuffer, condType string, condStatus metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&cb.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		ObservedGeneration: cb.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})
}
