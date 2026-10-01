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
	"strconv"
	"strings"

	"github.com/awslabs/operatorpkg/docs"
	"github.com/awslabs/operatorpkg/wellknown"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

// Well known labels and resources
const (
	ArchitectureAmd64    = "amd64"
	ArchitectureArm64    = "arm64"
	CapacityTypeSpot     = "spot"
	CapacityTypeOnDemand = "on-demand"
	CapacityTypeReserved = "reserved"
)

// Karpenter specific domains and labels
const (
	NodePoolLabelKey            = apis.Group + "/nodepool"
	NodeInitializedLabelKey     = apis.Group + "/initialized"
	NodeRegisteredLabelKey      = apis.Group + "/registered"
	NodeDoNotSyncTaintsLabelKey = apis.Group + "/do-not-sync-taints"
	CapacityTypeLabelKey        = apis.Group + "/capacity-type"
)

// Karpenter specific annotations
const (
	DoNotDisruptAnnotationKey                  = apis.Group + "/do-not-disrupt"
	DoNotRepairAnnotationKey                   = apis.Group + "/do-not-repair"
	ProviderCompatibilityAnnotationKey         = apis.CompatibilityGroup + "/provider"
	NodePoolHashAnnotationKey                  = apis.Group + "/nodepool-hash"
	NodePoolHashVersionAnnotationKey           = apis.Group + "/nodepool-hash-version"
	NodeClaimTerminationTimestampAnnotationKey = apis.Group + "/nodeclaim-termination-timestamp"
	NodeClaimMinValuesRelaxedAnnotationKey     = apis.Group + "/nodeclaim-min-values-relaxed"
	RebootPreBootIDAnnotationKey               = apis.Group + "/reboot-pre-boot-id"
	RebootTerminationGracePeriodAnnotationKey  = apis.Group + "/reboot-termination-grace-period"
	DRADriversAnnotationKey                    = apis.Group + "/requested-dra-drivers"
	DisruptionCostAnnotationKey                = apis.Group + "/disruption-cost"
)

var (
	trueValue  = strconv.FormatBool(true)
	falseValue = strconv.FormatBool(false)
)

var (
	DoNotDisruptAnnotation = wellknown.Annotation{
		Name:    DoNotDisruptAnnotationKey,
		Example: trueValue,
		UsedOn:  []runtime.Object{&v1.Pod{}, &v1.Node{}, &NodeClaim{}},
		Help: "Users set this to block voluntary disruption. On a Node or NodeClaim, it blocks consolidation and " +
			"drift. On a Pod, it blocks consolidation of the Pod's node, and blocks drift unless the NodeClaim sets " +
			"a terminationGracePeriod. On a Pod, the value may also be a duration (e.g. `5m`) after the Pod's start " +
			"time at which the protection ends. It does not block expiration, repair, or forceful termination once " +
			"the terminationGracePeriod elapses.",
		Values: []docs.Value{{Name: trueValue, Help: "Disruption is blocked."}},
		Stage:  docs.GA,
	}
	DoNotRepairAnnotation = wellknown.Annotation{
		Name:    DoNotRepairAnnotationKey,
		Example: trueValue,
		UsedOn:  []runtime.Object{&v1.Node{}, &NodeClaim{}},
		Help:    "Users set this to block node repair, independently of karpenter.sh/do-not-disrupt.",
		Values:  []docs.Value{{Name: trueValue, Help: "Repair is blocked."}},
		Stage:   options.NodeRepairStage,
	}
	NodePoolHashAnnotation = wellknown.Annotation{
		Name:    NodePoolHashAnnotationKey,
		Example: "6821555240594823858",
		UsedOn:  []runtime.Object{&NodePool{}, &NodeClaim{}},
		Help: "Karpenter sets this to a hash of the NodePool's template, on the NodePool and on each NodeClaim it " +
			"launches. A NodeClaim whose hash differs from its NodePool's is drifted.",
		Stage:        docs.Alpha,
		InternalOnly: true,
	}
	NodePoolHashVersionAnnotation = wellknown.Annotation{
		Name:    NodePoolHashVersionAnnotationKey,
		Example: NodePoolHashVersion,
		UsedOn:  []runtime.Object{&NodePool{}, &NodeClaim{}},
		Help: "Karpenter sets this to the version of the karpenter.sh/nodepool-hash algorithm. Hashes are only " +
			"compared when versions match; when the version changes, Karpenter rehashes NodeClaims instead of " +
			"drifting them.",
		Values:       []docs.Value{{Name: NodePoolHashVersion, Help: "The current hash version."}},
		Stage:        docs.Alpha,
		InternalOnly: true,
	}
	NodeClaimTerminationTimestampAnnotation = wellknown.Annotation{
		Name:    NodeClaimTerminationTimestampAnnotationKey,
		Example: "2026-10-01T22:00:00Z",
		UsedOn:  []runtime.Object{&NodeClaim{}},
		Help: "Karpenter sets this to the RFC3339 time by which the node must finish draining, from the " +
			"NodeClaim's terminationGracePeriod when it is deleted. Pods are deleted early enough to complete " +
			"their own terminationGracePeriodSeconds by then, bypassing PDBs and karpenter.sh/do-not-disrupt.",
		Stage:        docs.Alpha,
		InternalOnly: true,
	}
	NodeClaimMinValuesRelaxedAnnotation = wellknown.Annotation{
		Name:    NodeClaimMinValuesRelaxedAnnotationKey,
		Example: falseValue,
		UsedOn:  []runtime.Object{&NodeClaim{}},
		Help: "Karpenter sets this to whether scheduling relaxed the NodePool's minValues requirements to launch " +
			"the NodeClaim.",
		Values: []docs.Value{
			{Name: trueValue, Help: "minValues was relaxed."},
			{Name: falseValue, Help: "minValues was satisfied."},
		},
		Stage:        docs.Alpha,
		InternalOnly: true,
	}
	RebootPreBootIDAnnotation = wellknown.Annotation{
		Name:    RebootPreBootIDAnnotationKey,
		Example: "2b6c2f9e-3a8d-4f4e-9c1a-7d5e8b0f6a12",
		UsedOn:  []runtime.Object{&NodeClaim{}},
		Help: "Karpenter sets this to the node's boot ID before it issues a reboot. A changed boot ID means the " +
			"reboot happened, so it is not issued again. Removed when the reboot completes.",
		Stage:        docs.Alpha,
		InternalOnly: true,
	}
	RebootTerminationGracePeriodAnnotation = wellknown.Annotation{
		Name:    RebootTerminationGracePeriodAnnotationKey,
		Example: "10m",
		UsedOn:  []runtime.Object{&NodeClaim{}},
		Help: "Karpenter sets this to a duration bounding the drain before a reboot. When unset, the drain is " +
			"unbounded; `0` drains forcefully.",
		Stage:        docs.Alpha,
		InternalOnly: true,
	}
	DRADriversAnnotation = wellknown.Annotation{
		Name:    DRADriversAnnotationKey,
		Example: "gpu.nvidia.com",
		UsedOn:  []runtime.Object{&NodeClaim{}},
		Help: "Karpenter sets this to a comma-separated list of the DRA drivers whose devices were allocated to " +
			"pods scheduled to the NodeClaim. The node is not initialized until each driver has published its " +
			"ResourceSlices.",
		Stage:        docs.Alpha,
		InternalOnly: true,
	}
	DisruptionCostAnnotation = wellknown.Annotation{
		Name:    DisruptionCostAnnotationKey,
		Example: "100",
		UsedOn:  []runtime.Object{&v1.Pod{}},
		Help: "Users set this to an int32 cost of evicting the pod. Consolidation prefers to disrupt nodes whose " +
			"pods cost less to evict, so a higher cost makes the pod's node less likely to be consolidated. When " +
			"unset and the PodDeletionCostManagement feature gate is disabled, Karpenter reads " +
			"controller.kubernetes.io/pod-deletion-cost instead; when enabled, Karpenter writes " +
			"controller.kubernetes.io/pod-deletion-cost itself.",
		Stage: docs.Alpha,
	}
)

// KarpenterAnnotations are the well known annotations Karpenter reads or writes.
var KarpenterAnnotations = []wellknown.Annotation{
	DoNotDisruptAnnotation,
	DoNotRepairAnnotation,
	NodePoolHashAnnotation,
	NodePoolHashVersionAnnotation,
	NodeClaimTerminationTimestampAnnotation,
	NodeClaimMinValuesRelaxedAnnotation,
	RebootPreBootIDAnnotation,
	RebootTerminationGracePeriodAnnotation,
	DRADriversAnnotation,
	DisruptionCostAnnotation,
}

// Karpenter specific finalizers
const (
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
