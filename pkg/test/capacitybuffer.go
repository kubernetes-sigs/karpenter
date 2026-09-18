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

package test

import (
	"fmt"

	"github.com/imdario/mergo"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
)

func CapacityBuffer(overrides ...autoscalingv1beta1.CapacityBuffer) *autoscalingv1beta1.CapacityBuffer {
	override := autoscalingv1beta1.CapacityBuffer{}
	for _, opts := range overrides {
		if err := mergo.Merge(&override, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("failed to merge: %v", err))
		}
	}
	if override.Name == "" {
		override.Name = RandomName()
	}
	if override.Namespace == "" {
		override.Namespace = "default"
	}
	return &autoscalingv1beta1.CapacityBuffer{
		ObjectMeta: NamespacedObjectMeta(override.ObjectMeta),
		Spec:       override.Spec,
		Status:     override.Status,
	}
}

// ReadyBuffer returns a podTemplateRef CapacityBuffer in the "default" namespace
// that is fully resolved and ready for provisioning, with a deterministic UID of
// "uid-<name>".
func ReadyBuffer(name string, replicas int32) *autoscalingv1beta1.CapacityBuffer {
	return &autoscalingv1beta1.CapacityBuffer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: autoscalingv1beta1.CapacityBufferSpec{
			PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: name + "-template"},
			Replicas:       lo.ToPtr(replicas),
		},
		Status: autoscalingv1beta1.CapacityBufferStatus{
			Replicas:       lo.ToPtr(replicas),
			PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: name + "-template"},
			Conditions: []metav1.Condition{{
				Type:   autoscalingv1beta1.ReadyForProvisioningCondition,
				Status: metav1.ConditionTrue,
				Reason: "Resolved",
			}},
		},
	}
}

// ReadyBufferInNamespace is like ReadyBuffer but places the buffer in the given
// namespace, with a deterministic UID of "uid-<namespace>-<name>" so that
// same-named buffers in different namespaces stay distinct.
func ReadyBufferInNamespace(name, namespace string, replicas int32) *autoscalingv1beta1.CapacityBuffer {
	return &autoscalingv1beta1.CapacityBuffer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("uid-" + namespace + "-" + name),
		},
		Spec: autoscalingv1beta1.CapacityBufferSpec{
			PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: name + "-template"},
			Replicas:       lo.ToPtr(replicas),
		},
		Status: autoscalingv1beta1.CapacityBufferStatus{
			Replicas:       lo.ToPtr(replicas),
			PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: name + "-template"},
			Conditions: []metav1.Condition{{
				Type:   autoscalingv1beta1.ReadyForProvisioningCondition,
				Status: metav1.ConditionTrue,
				Reason: "Resolved",
			}},
		},
	}
}

// ReadyScalableRefBuffer returns a scalableRef CapacityBuffer in the "default"
// namespace that is ready for provisioning, with a deterministic UID of
// "uid-<name>".
func ReadyScalableRefBuffer(name string, replicas int32) *autoscalingv1beta1.CapacityBuffer {
	return &autoscalingv1beta1.CapacityBuffer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: autoscalingv1beta1.CapacityBufferSpec{
			ScalableRef: &autoscalingv1beta1.ScalableRef{
				APIGroup: "apps",
				Kind:     "Deployment",
				Name:     name + "-deploy",
			},
			Percentage: lo.ToPtr(int32(20)),
		},
		Status: autoscalingv1beta1.CapacityBufferStatus{
			Replicas: lo.ToPtr(replicas),
			Conditions: []metav1.Condition{{
				Type:   autoscalingv1beta1.ReadyForProvisioningCondition,
				Status: metav1.ConditionTrue,
				Reason: "Resolved",
			}},
		},
	}
}
