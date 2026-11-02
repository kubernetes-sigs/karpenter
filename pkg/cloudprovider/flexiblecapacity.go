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

package cloudprovider

import (
	"encoding/json"
	"strings"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// FlexibleCapacityFromNodeClaim returns the flexible resource amounts that Karpenter chose for the NodeClaim, from the
// karpenter.sh/flexible-capacity annotation. It returns nil when the annotation is absent.
func FlexibleCapacityFromNodeClaim(nodeClaim *v1.NodeClaim) (corev1.ResourceList, error) {
	value, ok := nodeClaim.Annotations[v1.FlexibleCapacityAnnotationKey]
	if !ok {
		return nil, nil
	}
	flexible := corev1.ResourceList{}
	if err := json.Unmarshal([]byte(value), &flexible); err != nil {
		return nil, serrors.Wrap(err, "annotation", v1.FlexibleCapacityAnnotationKey, "NodeClaim", klog.KObj(nodeClaim))
	}
	return flexible, nil
}

// FlexibleCapacityAnnotationValue returns the karpenter.sh/flexible-capacity annotation value for the given amounts.
func FlexibleCapacityAnnotationValue(flexible corev1.ResourceList) (string, error) {
	value, err := json.Marshal(flexible)
	if err != nil {
		return "", serrors.Wrap(err, "annotation", v1.FlexibleCapacityAnnotationKey)
	}
	return string(value), nil
}

// ApplyFlexibleCapacity returns copies of capacity and allocatable with each flexible resource set to its chosen
// amount. Allocatable memory is reduced by the chosen hugepages amounts, and never goes below zero. Cloud providers
// use this to build the node status they report for a NodeClaim.
func ApplyFlexibleCapacity(capacity, allocatable, flexible corev1.ResourceList) (corev1.ResourceList, corev1.ResourceList) {
	capacity = lo.Assign(capacity, flexible)
	allocatable = lo.Assign(allocatable, flexible)
	for name, quantity := range flexible {
		if !strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix) {
			continue
		}
		memory, ok := allocatable[corev1.ResourceMemory]
		if !ok {
			continue
		}
		memory = memory.DeepCopy()
		memory.Sub(quantity)
		if memory.Sign() == -1 {
			memory.Set(0)
		}
		allocatable[corev1.ResourceMemory] = memory
	}
	return capacity, allocatable
}

// FlexibleCapacityStatus returns copies of the capacity and allocatable that a cloud provider reported for a NodeClaim
// that launched as the instance type. Each resource in flexible is set to its chosen amount. Each other flexible
// resource of the instance type is removed. Allocatable memory becomes the lower of the reported value and the
// instance type's allocatable memory less the chosen hugepages. The result is therefore the same whether the cloud
// provider reported the ceiling and full memory, or already applied the chosen amounts.
//
// The cloud provider gives the node the amounts in flexible, even when the NodeOverlays changed after Karpenter chose
// them. So a resource in flexible is set to its chosen amount even when the instance type no longer has it as
// flexible. If the instance type now has a fixed amount of that hugepages resource, that amount is not taken out of
// the expected memory, because the node does not have it.
func FlexibleCapacityStatus(instanceType *InstanceType, capacity, allocatable, flexible corev1.ResourceList) (corev1.ResourceList, corev1.ResourceList) {
	names := lo.Union(lo.Keys(instanceType.FlexibleCapacity()), lo.Keys(flexible))
	capacity = lo.Assign(lo.OmitByKeys(capacity, names), flexible)
	allocatable = lo.Assign(lo.OmitByKeys(allocatable, names), flexible)
	reported, ok := allocatable[corev1.ResourceMemory]
	if !ok {
		return capacity, allocatable
	}
	// The expected memory is the instance type's memory, less its overhead and less the hugepages that the node has:
	// the fixed amounts of hugepages resources that are not in flexible, and the chosen amounts.
	expected := instanceType.Capacity.Memory().DeepCopy()
	overhead := lo.FromPtr(instanceType.Overhead).Total()
	expected.Sub(*overhead.Memory())
	for name, quantity := range instanceType.Capacity {
		_, chosen := flexible[name]
		if strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix) && !instanceType.IsFlexible(name) && !chosen {
			expected.Sub(quantity)
		}
	}
	for name, quantity := range flexible {
		if strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix) {
			expected.Sub(quantity)
		}
	}
	if expected.Sign() == -1 {
		expected.Set(0)
	}
	if expected.Cmp(reported) < 0 {
		allocatable[corev1.ResourceMemory] = expected
	}
	return capacity, allocatable
}
