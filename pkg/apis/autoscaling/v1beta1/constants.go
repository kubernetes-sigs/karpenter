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

package v1beta1

import "math"

// Constants shared across the CapacityBuffer controller, the provisioner, and
// any downstream consumer (disruption, metrics). Mirrors upstream Cluster
// Autoscaler constants at
// k8s.io/autoscaler/cluster-autoscaler/capacitybuffer/constants.go
const (
	ActiveProvisioningStrategy = "buffer.x-k8s.io/active-capacity"

	// Refill-strategy values for CapacityBufferSpec.RefillStrategy. This axis is orthogonal to
	// ProvisioningStrategy (which describes the kind of capacity). See the ephemeral buffers
	// proposal; field name/values are still under discussion upstream.
	//   RefillStrategyRecreate (default): consumed capacity is recreated to maintain buffer size.
	//   RefillStrategyNone: consumed capacity is NOT recreated (one-shot / ephemeral).
	RefillStrategyRecreate = "recreate"
	RefillStrategyNone     = "none"

	// Condition types written to CapacityBuffer status.
	ReadyForProvisioningCondition = "ReadyForProvisioning"
	ProvisioningCondition         = "Provisioning"
	LimitedByQuotasCondition      = "LimitedByQuotas"

	// FulfilledCondition is set True on a one-shot (RefillStrategyNone) CapacityBuffer once matching
	// bound capacity has covered its intended size — i.e. the buffer was filled *successfully*. It is
	// terminal: the buffer emits zero virtual pods thereafter and its nodes are no longer protected
	// from consolidation. It is NOT set when the fill deadline elapses unfilled — that is reported by
	// ExpiredCondition, so consumers can distinguish success from give-up.
	FulfilledCondition          = "Fulfilled"
	FulfilledReasonBufferFilled = "BufferFilled"

	// ExpiredCondition is set True on a one-shot buffer whose fillDeadlineSeconds elapsed before it
	// was filled. Like Fulfilled it is terminal (provisioning stops), but it signals the buffer was
	// NOT filled. Fulfilled and Expired are mutually exclusive terminal outcomes.
	ExpiredCondition              = "Expired"
	ExpiredReasonDeadlineExceeded = "FillDeadlineExceeded"

	// BufferMatchSelectorAnnotation carries a kubectl-style label selector identifying the pods
	// (in the buffer's namespace) that count toward filling a one-shot buffer. INTERIM: the
	// consumption-tracking / matching surface is being defined by the capped buffers proposal
	// (expected as a spec field, e.g. matchingPodSelector); this annotation is used until it lands.
	BufferMatchSelectorAnnotation = "karpenter.sh/buffer-match-selector"

	// Supported scalableRef kinds.
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindReplicaSet  = "ReplicaSet"

	// FakePodAnnotationKey marks a virtual pod constructed from a CapacityBuffer.
	FakePodAnnotationKey   = "karpenter.sh/capacity-buffer-fake-pod"
	FakePodAnnotationValue = "true"

	// BufferNameAnnotation records which CapacityBuffer a virtual pod belongs to.
	BufferNameAnnotation = "karpenter.sh/capacity-buffer-name"

	// BufferNamespaceAnnotation records the namespace of the CapacityBuffer a virtual pod belongs to.
	BufferNamespaceAnnotation = "karpenter.sh/capacity-buffer-namespace"

	// VirtualPodPriority is the priority stamped onto virtual buffer pods so that
	// future preemption / disruption logic can identify them as low-value.
	// NOTE: Karpenter's scheduler currently sorts the queue by resource size,
	// not priority, so this value does not affect scheduling order today.
	VirtualPodPriority int32 = math.MinInt32
)
