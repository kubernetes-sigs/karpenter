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

import autoscalingv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1alpha1"

// Re-exports of shared API-level constants so existing call sites in this
// package keep working. New code should import from the API package directly.
const (
	ActiveProvisioningStrategy    = autoscalingv1alpha1.ActiveProvisioningStrategy
	ReadyForProvisioningCondition = autoscalingv1alpha1.ReadyForProvisioningCondition
	ProvisioningCondition         = autoscalingv1alpha1.ProvisioningCondition
	LimitedByQuotasCondition      = autoscalingv1alpha1.LimitedByQuotasCondition

	CapacityBufferKind       = "CapacityBuffer"
	CapacityBufferApiVersion = "autoscaling.x-k8s.io/v1alpha1"

	// Reasons emitted by this controller on ReadyForProvisioning.
	ReasonResolved            = "Resolved"
	ReasonScalableRefNotFound = "ScalableRefNotFound"
	ReasonPodTemplateNotFound = "PodTemplateNotFound"
	ReasonResolutionFailed    = "ResolutionFailed"

	// Reasons emitted by the provisioner on the Provisioning condition.
	ReasonFitsExistingCapacity    = "FitsExistingCapacity"
	ReasonRequiresNewCapacity     = "RequiresNewCapacity"
	ReasonNotReadyForProvisioning = "NotReadyForProvisioning"
	ReasonBufferEmpty             = "BufferEmpty"
)
