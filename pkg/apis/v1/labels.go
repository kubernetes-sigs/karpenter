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

package v1

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/apis"
)

// Well known labels and resources
const (
	// ArchitectureAmd64 is a value of the kubernetes.io/arch label. Use it in a NodePool requirement or a pod
	// nodeSelector to launch or schedule onto x86-64 instance types.
	ArchitectureAmd64 = "amd64"
	// ArchitectureArm64 is a value of the kubernetes.io/arch label. Use it in a NodePool requirement or a pod
	// nodeSelector to launch or schedule onto arm64 instance types.
	ArchitectureArm64 = "arm64"
	// CapacityTypeSpot is a value of the karpenter.sh/capacity-type label for spare capacity that is cheaper
	// than on-demand but can be reclaimed by the cloud provider at any time. Karpenter handles the resulting
	// interruption by draining the node and launching a replacement.
	CapacityTypeSpot = "spot"
	// CapacityTypeOnDemand is a value of the karpenter.sh/capacity-type label for regular pay as you go
	// capacity that is not reclaimed by the cloud provider.
	CapacityTypeOnDemand = "on-demand"
	// CapacityTypeReserved is a value of the karpenter.sh/capacity-type label for capacity the cloud provider
	// has reserved ahead of time (for example a capacity reservation). When a NodePool allows it, Karpenter
	// prefers reserved capacity over spot and on-demand while the reservation still has room.
	CapacityTypeReserved = "reserved"
)

// Karpenter specific domains and labels
const (
	// NodePoolLabelKey is the name of the NodePool that launched the NodeClaim. Karpenter sets it on the
	// NodeClaim when it is created and copies it to the Node on registration. Pods can use it in a
	// nodeSelector or node affinity to target a specific NodePool, and Karpenter ignores Nodes without it
	// when considering disruption.
	//
	// <nodepool name> ; the NodePool that owns this NodeClaim and Node
	NodePoolLabelKey = apis.Group + "/nodepool"
	// NodeInitializedLabelKey marks a Node that Karpenter has finished initializing. It is set once the Node
	// is Ready, every startup taint from the NodeClaim has been removed, every extended resource the NodeClaim
	// expected is allocatable, and (when DRA is enabled) every expected DRA driver has published its
	// ResourceSlices. Until then Karpenter keeps counting the Node as in flight and does not consider it for
	// consolidation.
	//
	// "true" ; the Node is initialized
	// unset  ; the Node has not finished initializing
	NodeInitializedLabelKey = apis.Group + "/initialized"
	// NodeRegisteredLabelKey marks a Node that has joined the cluster and been matched to its NodeClaim. At
	// that point Karpenter syncs the NodeClaim's labels, annotations, taints and the termination finalizer
	// onto the Node and removes the karpenter.sh/unregistered taint.
	//
	// "true" ; the Node is registered and linked to its NodeClaim
	// unset  ; the Node has not been registered by Karpenter yet
	NodeRegisteredLabelKey = apis.Group + "/registered"
	// NodeDoNotSyncTaintsLabelKey is set on a Node, usually by the cloud provider through its bootstrap
	// config, to take ownership of the Node's taints. Karpenter still removes the karpenter.sh/unregistered
	// taint on registration so the rest of the lifecycle works as normal.
	//
	// "true" ; Karpenter does not copy the NodeClaim's taints and startup taints onto the Node
	// unset or any other value ; Karpenter merges the NodeClaim's taints and startup taints into the Node
	NodeDoNotSyncTaintsLabelKey = apis.Group + "/do-not-sync-taints"
	// CapacityTypeLabelKey is the purchase option the node was launched with. It can be used in NodePool
	// requirements to choose which capacity types Karpenter may launch, and in pod scheduling constraints to
	// keep workloads on or off a capacity type.
	//
	// "spot"      ; spare capacity that can be reclaimed by the cloud provider
	// "on-demand" ; regular capacity that is not reclaimed
	// "reserved"  ; capacity from a reservation the cloud provider holds for you
	CapacityTypeLabelKey = apis.Group + "/capacity-type"
)

// Karpenter specific annotations
const (
	// DoNotDisruptAnnotationKey blocks voluntary disruption (consolidation and drift) by Karpenter. On a Node
	// or NodeClaim it keeps that node from being picked as a disruption candidate. On a Pod it keeps the node
	// the pod runs on from being voluntarily disrupted, and during a forced termination (expiration,
	// interruption, repair) the pod is not evicted until the NodeClaim's terminationGracePeriod runs out.
	// Terminating, terminal and DaemonSet pods are ignored.
	//
	// "true"     ; disruption is blocked for as long as the annotation is present
	// <duration> ; on a Pod only, a Go duration such as "30m" or "1h". Disruption is blocked until the pod
	//              has been running that long. An unparsable value is ignored and an event is emitted.
	DoNotDisruptAnnotationKey = apis.Group + "/do-not-disrupt"
	// ProviderCompatibilityAnnotationKey is reserved for cloud providers to store provider specific data that
	// has to survive conversion between Karpenter API versions. The core controllers do not read or write it,
	// and its value format is defined by the provider.
	ProviderCompatibilityAnnotationKey = apis.CompatibilityGroup + "/provider"
	// NodePoolHashAnnotationKey is a hash of the static (non requirement) fields of a NodePool's template.
	// Karpenter keeps it up to date on the NodePool and stamps it on every NodeClaim at launch. When the value
	// on a NodeClaim no longer matches its NodePool, and both carry the same karpenter.sh/nodepool-hash-version,
	// the NodeClaim is marked Drifted.
	//
	// <hash> ; opaque hash of the NodePool template, compared as a string
	NodePoolHashAnnotationKey = apis.Group + "/nodepool-hash"
	// NodePoolHashVersionAnnotationKey is the version of the hashing scheme behind karpenter.sh/nodepool-hash.
	// When Karpenter changes which fields are hashed it bumps the version and rewrites the hash on existing
	// NodePools and NodeClaims instead of treating every node as drifted.
	//
	// <version> ; hashing scheme version, for example "v3". Static drift is only checked when the NodePool
	//             and NodeClaim versions match.
	NodePoolHashVersionAnnotationKey = apis.Group + "/nodepool-hash-version"
	// NodeClaimTerminationTimestampAnnotationKey is the time after which Karpenter stops waiting for a graceful
	// drain of a deleting NodeClaim. From then on pods are deleted regardless of PDBs and
	// karpenter.sh/do-not-disrupt, and the instance is terminated. It is set to the deletion timestamp plus
	// spec.terminationGracePeriod, or to the current time when node repair force terminates an unhealthy
	// node.
	//
	// <RFC3339 timestamp> ; for example "2025-01-02T15:04:05Z"
	NodeClaimTerminationTimestampAnnotationKey = apis.Group + "/nodeclaim-termination-timestamp"
	// NodeClaimMinValuesRelaxedAnnotationKey records whether the scheduler had to lower a requirement's
	// minValues to launch this NodeClaim. That only happens when the minValuesPolicy is BestEffort. It is also
	// reported on the nodeclaims created metric.
	//
	// "true"  ; at least one minValues was relaxed to fit the available instance types
	// "false" ; every minValues was satisfied as written
	NodeClaimMinValuesRelaxedAnnotationKey = apis.Group + "/nodeclaim-min-values-relaxed"
	// DRADriversAnnotationKey records the comma-separated set of DRA driver names whose devices were allocated to pods
	// scheduled to this NodeClaim. The initialization controller can gate on these drivers having published their
	// ResourceSlices before marking the node initialized.
	DRADriversAnnotationKey = apis.Group + "/requested-dra-drivers"
)

// Karpenter specific finalizers
const (
	// TerminationFinalizer is added to every NodeClaim when it is launched and to its Node on registration.
	// While it is present, deleting either object makes Karpenter taint the Node with
	// karpenter.sh/disrupted:NoSchedule, drain it (respecting PDBs and karpenter.sh/do-not-disrupt until the
	// termination timestamp), wait for volume detachment, and delete the cloud provider instance before the
	// finalizer is removed.
	TerminationFinalizer = apis.Group + "/termination"
)

var (
	// RestrictedLabelDomains are reserved by karpenter.
	RestrictedLabelDomains = sets.New(
		apis.Group,
	)

	// WellKnownLabels are labels that Karpenter is aware of and can be used to
	// further narrow down the range of the corresponding values by either nodepool or pods.
	WellKnownLabels = sets.New(
		NodePoolLabelKey,
		v1.LabelTopologyZone,
		v1.LabelTopologyRegion,
		v1.LabelInstanceTypeStable,
		v1.LabelArchStable,
		v1.LabelOSStable,
		CapacityTypeLabelKey,
		v1.LabelWindowsBuild,
	)

	// WellKnownResources are resources that are expected from the instance types
	// provided by cloud providers.
	WellKnownResources = sets.New[v1.ResourceName](
		v1.ResourceCPU,
		v1.ResourceMemory,
		v1.ResourceEphemeralStorage,
		v1.ResourcePods,
	)

	// WellKnownValuesForRequirements are for requirements where a known set of values
	// is expected to be used for that requirement. For example, in the AWS provider,
	// only on-demand, spot, and reserved make sense as values for the capacity type requirement
	WellKnownValuesForRequirements = map[string]sets.Set[string]{
		CapacityTypeLabelKey: sets.New(
			CapacityTypeOnDemand,
			CapacityTypeSpot,
			CapacityTypeReserved,
		),
	}

	// WellKnownLabelsForOfferings are for requirements where a known labels that will be used in the
	// offerings passed back by the provider
	WellKnownLabelsForOfferings = sets.New(
		v1.LabelTopologyZone,
		CapacityTypeLabelKey,
	)

	// RestrictedLabels are labels that should not be used
	// because they may interfere with the internal provisioning logic.
	RestrictedLabels = sets.New(
		v1.LabelHostname,
	)

	// NormalizedLabels translate aliased concepts into the controller's
	// WellKnownLabels. Pod requirements are translated for compatibility.
	NormalizedLabels = map[string]string{
		v1.LabelFailureDomainBetaZone:   v1.LabelTopologyZone,
		"beta.kubernetes.io/arch":       v1.LabelArchStable,
		"beta.kubernetes.io/os":         v1.LabelOSStable,
		v1.LabelInstanceType:            v1.LabelInstanceTypeStable,
		v1.LabelFailureDomainBetaRegion: v1.LabelTopologyRegion,
	}

	// NormalizedLabelValues translates label values when a label key is normalized
	// via NormalizedLabels. The map key is the normalized (target) label key, and
	// the value is a map from original values to their normalized equivalents.
	// For example, a CSI driver may use "" for non-zonal topology while the cloud
	// provider uses "0" — this mapping bridges that gap.
	NormalizedLabelValues = map[string]map[string]string{}
)

// IsRestrictedLabel returns an error if the label is restricted.
func IsRestrictedLabel(key string) error {
	if WellKnownLabels.Has(key) {
		return nil
	}
	labelDomain := GetLabelDomain(key)
	for restrictedLabelDomain := range RestrictedLabelDomains {
		if labelDomain == restrictedLabelDomain || strings.HasSuffix(labelDomain, "."+restrictedLabelDomain) {
			return fmt.Errorf("using label %s is not allowed as it might interfere with the internal provisioning logic; specify a well known label: %v, or a custom label that does not use a restricted domain: %v", key, sets.List(WellKnownLabels), sets.List(RestrictedLabelDomains))
		}
	}

	if RestrictedLabels.Has(key) {
		return fmt.Errorf("using label %s is not allowed as it might interfere with the internal provisioning logic; specify a well known label: %v, or a custom label that does not use a restricted domain: %v", key, sets.List(WellKnownLabels), sets.List(RestrictedLabelDomains))
	}
	return nil
}

// HasKnownValues returns an error if the requirement has well known values and is only presented with unknown values.
func HasKnownValues(requirement NodeSelectorRequirementWithMinValues) error {
	if !WellKnownLabels.Has(requirement.Key) {
		return nil
	}
	if !WellKnownValuesForRequirements[requirement.Key].HasAny(requirement.Values...) {
		return fmt.Errorf("invalid values: %v for key: %s, expected one of: %v", requirement.Values, requirement.Key, WellKnownValuesForRequirements[requirement.Key].UnsortedList())
	}
	return nil
}

func GetLabelDomain(key string) string {
	if parts := strings.SplitN(key, "/", 2); len(parts) == 2 {
		return parts[0]
	}
	return ""
}

func NodeClassLabelKey(gk schema.GroupKind) string {
	return fmt.Sprintf("%s/%s", gk.Group, strings.ToLower(gk.Kind))
}
