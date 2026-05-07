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
	"fmt"
	"math"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	autoscalingv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1alpha1"
)

// extractPodSpecFromUnstructured navigates spec.template.spec in an unstructured
// workload object and builds a partial PodSpec with container names, images, and
// resource requirements. This is sufficient for replica/limit calculations; the
// provisioner re-reads the full spec when constructing virtual pods.
func extractPodSpecFromUnstructured(obj *unstructured.Unstructured) (*v1.PodSpec, error) {
	templateSpec, found, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec")
	if err != nil {
		return nil, fmt.Errorf("accessing spec.template.spec: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("spec.template.spec not found in %s %q", obj.GetKind(), obj.GetName())
	}

	podSpec := &v1.PodSpec{}
	podSpec.Containers = extractContainers(templateSpec)
	return podSpec, nil
}

func extractContainers(specMap map[string]interface{}) []v1.Container {
	containersRaw, found := specMap["containers"]
	if !found {
		return nil
	}
	containersList, ok := containersRaw.([]interface{})
	if !ok {
		return nil
	}
	var containers []v1.Container
	for _, c := range containersList {
		cMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		container := v1.Container{}
		if name, ok := cMap["name"].(string); ok {
			container.Name = name
		}
		if image, ok := cMap["image"].(string); ok {
			container.Image = image
		}
		container.Resources = extractResources(cMap)
		containers = append(containers, container)
	}
	return containers
}

func extractResources(containerMap map[string]interface{}) v1.ResourceRequirements {
	reqs := v1.ResourceRequirements{}
	resources, found := containerMap["resources"]
	if !found {
		return reqs
	}
	resMap, ok := resources.(map[string]interface{})
	if !ok {
		return reqs
	}
	if requests, ok := resMap["requests"].(map[string]interface{}); ok {
		reqs.Requests = parseResourceList(requests)
	}
	if limits, ok := resMap["limits"].(map[string]interface{}); ok {
		reqs.Limits = parseResourceList(limits)
	}
	return reqs
}

func parseResourceList(m map[string]interface{}) v1.ResourceList {
	rl := v1.ResourceList{}
	for k, v := range m {
		str, ok := v.(string)
		if !ok {
			continue
		}
		qty, err := resource.ParseQuantity(str)
		if err != nil {
			continue
		}
		rl[v1.ResourceName(k)] = qty
	}
	return rl
}

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
		if int32(n) < minReplicas {
			minReplicas = int32(n)
		}
	}

	if !matched {
		return -1
	}
	return minReplicas
}

// totalPodRequests computes effective resource requests for one pod following
// Kubernetes scheduling semantics: sum all regular containers, then take the
// max of that sum vs each individual init container (since init containers run
// sequentially before regular containers start).
func totalPodRequests(podSpec *v1.PodSpec) v1.ResourceList {
	total := v1.ResourceList{}
	for _, c := range podSpec.Containers {
		for resourceName, qty := range c.Resources.Requests {
			existing := total[resourceName]
			existing.Add(qty)
			total[resourceName] = existing
		}
	}
	for _, c := range podSpec.InitContainers {
		for resourceName, qty := range c.Resources.Requests {
			if existing, ok := total[resourceName]; !ok || qty.Cmp(existing) > 0 {
				total[resourceName] = qty
			}
		}
	}
	return total
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
