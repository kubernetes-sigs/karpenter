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

// Mirrors upstream Cluster Autoscaler constants at
// k8s.io/autoscaler/cluster-autoscaler/capacitybuffer/constants.go
// so that any downstream consumer (e.g. the future provisioner) can import
// the same names instead of hardcoding strings.
const (
	ActiveProvisioningStrategy = "buffer.x-k8s.io/active-capacity"
	CapacityBufferKind         = "CapacityBuffer"
	CapacityBufferApiVersion   = "autoscaling.x-k8s.io/v1alpha1"

	// Condition types.
	ReadyForProvisioningCondition = "ReadyForProvisioning"
	// ProvisioningCondition is set by the provisioner (not this controller) once
	// virtual pods have been injected into the scheduling loop.
	ProvisioningCondition = "Provisioning"
	// LimitedByQuotasCondition is reserved for future ResourceQuota integration.
	LimitedByQuotasCondition = "LimitedByQuotas"

	// Reasons currently emitted by this controller.
	ReasonResolved            = "Resolved"
	ReasonScalableRefNotFound = "ScalableRefNotFound"
	ReasonPodTemplateNotFound = "PodTemplateNotFound"
	ReasonResolutionFailed    = "ResolutionFailed"
)
