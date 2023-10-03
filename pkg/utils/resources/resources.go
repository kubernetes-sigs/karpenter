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

package resources

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/aws/karpenter-core/pkg/utils/pretty"
)

// RequestsForPods returns the total resources of a variadic list of podspecs.
func RequestsForPods(pods ...*v1.Pod) v1.ResourceList {
	var resources []v1.ResourceList
	for _, pod := range pods {
		resources = append(resources, Ceiling(pod).Requests)
	}
	merged := Merge(resources...)
	merged[v1.ResourcePods] = *resource.NewQuantity(int64(len(pods)), resource.DecimalExponent)
	return merged
}

// LimitsForPods returns the total resources of a variadic list of podspecs
func LimitsForPods(pods ...*v1.Pod) v1.ResourceList {
	var resources []v1.ResourceList
	for _, pod := range pods {
		resources = append(resources, Ceiling(pod).Limits)
	}
	merged := Merge(resources...)
	merged[v1.ResourcePods] = *resource.NewQuantity(int64(len(pods)), resource.DecimalExponent)
	return merged
}

// Merge the resources from the variadic into a single v1.ResourceList
func Merge(resources ...v1.ResourceList) v1.ResourceList {
	if len(resources) == 0 {
		return v1.ResourceList{}
	}
	result := make(v1.ResourceList, len(resources[0]))
	for _, resourceList := range resources {
		for resourceName, quantity := range resourceList {
			current := result[resourceName]
			current.Add(quantity)
			result[resourceName] = current
		}
	}
	return result
}

// MergeInto sums the resources from src into dest, modifying dest. If you need to repeatedly sum
// multiple resource lists, it allocates less to continually sum into an existing list as opposed to
// constructing a new one for each sum like Merge
func MergeInto(dest v1.ResourceList, src v1.ResourceList) v1.ResourceList {
	if dest == nil {
		sz := len(dest)
		if len(src) > sz {
			sz = len(src)
		}
		dest = make(v1.ResourceList, sz)
	}
	for resourceName, quantity := range src {
		current := dest[resourceName]
		current.Add(quantity)
		dest[resourceName] = current
	}
	return dest
}

func Subtract(lhs, rhs v1.ResourceList) v1.ResourceList {
	result := make(v1.ResourceList, len(lhs))
	for k, v := range lhs {
		result[k] = v.DeepCopy()
	}
	for resourceName := range lhs {
		current := lhs[resourceName]
		if rhsValue, ok := rhs[resourceName]; ok {
			current.Sub(rhsValue)
		}
		result[resourceName] = current
	}
	return result
}

func PodRequests(pod *v1.Pod) v1.ResourceList {
	reqs := v1.ResourceList{}

	for _, container := range pod.Spec.Containers {
		containerReqs := container.Resources.Requests

		MergeInto(reqs, containerReqs)

	}

	restartableInitContainerReqs := v1.ResourceList{}
	initContainerReqs := v1.ResourceList{}

	for _, container := range pod.Spec.InitContainers {
		containerReqs := container.Resources.Requests

		if container.RestartPolicy != nil && *container.RestartPolicy == v1.ContainerRestartPolicyAlways {
			MergeInto(reqs, containerReqs)

			MergeInto(restartableInitContainerReqs, containerReqs)
			containerReqs = restartableInitContainerReqs
		} else {
			tmp := v1.ResourceList{}
			MergeInto(tmp, containerReqs)
			MergeInto(tmp, restartableInitContainerReqs)
			containerReqs = tmp
		}

		initContainerReqs = MaxResources(initContainerReqs, containerReqs)
	}

	reqs = MaxResources(reqs, initContainerReqs)

	if pod.Spec.Overhead != nil {
		MergeInto(reqs, pod.Spec.Overhead)
	}

	return reqs
}

func PodLimits(pod *v1.Pod) v1.ResourceList {
	limits := v1.ResourceList{}

	for _, container := range pod.Spec.Containers {

		MergeInto(limits, container.Resources.Limits)
	}

	restartableInitContainerLimits := v1.ResourceList{}
	initContainerLimits := v1.ResourceList{}

	for _, container := range pod.Spec.InitContainers {
		containerLimits := container.Resources.Limits
		if container.RestartPolicy != nil && *container.RestartPolicy == v1.ContainerRestartPolicyAlways {
			// fmt.Println("sidecar at con",container.Resources.Limits.Cpu())
			MergeInto(limits, containerLimits)

			MergeInto(restartableInitContainerLimits, containerLimits)
			containerLimits = restartableInitContainerLimits
		} else {
			tmp := v1.ResourceList{}
			MergeInto(tmp, containerLimits)
			MergeInto(tmp, restartableInitContainerLimits)
			containerLimits = tmp
		}

		initContainerLimits = MaxResources(initContainerLimits, containerLimits)
	}

	limits = MaxResources(limits, initContainerLimits)

	if pod.Spec.Overhead != nil {
		for name, quantity := range pod.Spec.Overhead {
			if value, ok := limits[name]; ok && !value.IsZero() {
				value.Add(quantity)
				limits[name] = value
			}
		}
	}

	return limits
}

// Ceiling calculates the max between the sum of container resources and max of initContainers along with sidecar feature consideration
func Ceiling(pod *v1.Pod) v1.ResourceRequirements {
	var resources v1.ResourceRequirements

	resources.Requests = PodRequests(pod)
	resources.Limits = PodLimits(pod)
	return resources
}

// MaxResources returns the maximum quantities for a given list of resources
func MaxResources(resources ...v1.ResourceList) v1.ResourceList {
	resourceList := v1.ResourceList{}
	for _, resource := range resources {
		for resourceName, quantity := range resource {
			if value, ok := resourceList[resourceName]; !ok || quantity.Cmp(value) > 0 {
				resourceList[resourceName] = quantity
			}
		}
	}
	return resourceList
}

// MergeResourceLimitsIntoRequests merges resource limits into requests if no request exists for the given resource
func MergeResourceLimitsIntoRequests(container v1.Container) v1.ResourceList {
	resources := container.Resources.DeepCopy()
	if resources.Requests == nil {
		resources.Requests = v1.ResourceList{}
	}

	if resources.Limits != nil {
		for resourceName, quantity := range resources.Limits {
			if _, ok := resources.Requests[resourceName]; !ok {
				resources.Requests[resourceName] = quantity
			}
		}
	}
	return resources.Requests
}

// Quantity parses the string value into a *Quantity
func Quantity(value string) *resource.Quantity {
	r := resource.MustParse(value)
	return &r
}

// IsZero implements r.IsZero(). This method is provided to make some code a bit cleaner as the Quantity.IsZero() takes
// a pointer receiver and map index expressions aren't addressable, so it can't be called directly.
func IsZero(r resource.Quantity) bool {
	return r.IsZero()
}

func Cmp(lhs resource.Quantity, rhs resource.Quantity) int {
	return lhs.Cmp(rhs)
}

// Fits returns true if the candidate set of resources is less than or equal to the total set of resources.
func Fits(candidate, total v1.ResourceList) bool {
	// If any of the total resource values are negative then the resource will never fit
	for _, quantity := range total {
		if Cmp(resource.MustParse("0"), quantity) > 0 {
			return false
		}
	}
	for resourceName, quantity := range candidate {
		if Cmp(quantity, total[resourceName]) > 0 {
			return false
		}
	}
	return true
}

// String returns a string version of the resource list suitable for presenting in a log
func String(list v1.ResourceList) string {
	if len(list) == 0 {
		return "{}"
	}
	return pretty.Concise(list)
}

// StringMap returns the string map representation of the resource list
func StringMap(list v1.ResourceList) map[string]string {
	if list == nil {
		return nil
	}
	m := make(map[string]string)
	for k, v := range list {
		m[k.String()] = v.String()
	}
	return m
}
