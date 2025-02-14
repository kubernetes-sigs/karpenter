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

package scheduling

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// NodeClaim is a set of constraints, compatible pods, and possible instance types that could fulfill these constraints. This
// will be turned into one or more actual node instances within the cluster after bin packing.
type NodeClaim struct {
	NodeClaimTemplate

	Pods                 []*corev1.Pod
	reservedOfferings    map[string]cloudprovider.Offerings
	reservedOfferingMode ReservedOfferingMode
	reservationManager   *ReservationManager
	topology             *Topology
	hostPortUsage        *scheduling.HostPortUsage
	daemonResources      corev1.ResourceList
	hostname             string
}

// ReservedOfferingError indicates a NodeClaim couldn't be created or a pod couldn't be added to an exxisting NodeClaim
// due to
type ReservedOfferingError struct {
	error
}

func NewReservedOfferingError(err error) ReservedOfferingError {
	return ReservedOfferingError{error: err}
}

func IsReservedOfferingError(err error) bool {
	roe := &ReservedOfferingError{}
	return errors.As(err, roe)
}

func (e ReservedOfferingError) Unwrap() error {
	return e.error
}

var nodeID int64

func NewNodeClaim(
	nodeClaimTemplate *NodeClaimTemplate,
	topology *Topology,
	daemonResources corev1.ResourceList,
	instanceTypes []*cloudprovider.InstanceType,
	reservationManager *ReservationManager,
	reservedOfferingMode ReservedOfferingMode,
) *NodeClaim {
	hostname := fmt.Sprintf("hostname-placeholder-%04d", atomic.AddInt64(&nodeID, 1))
	topology.Register(corev1.LabelHostname, hostname)
	template := *nodeClaimTemplate
	template.Requirements = scheduling.NewRequirements()
	template.Requirements.Add(nodeClaimTemplate.Requirements.Values()...)
	template.Requirements.Add(scheduling.NewRequirement(corev1.LabelHostname, corev1.NodeSelectorOpIn, hostname))
	template.InstanceTypeOptions = instanceTypes
	template.Spec.Resources.Requests = daemonResources
	return &NodeClaim{
		NodeClaimTemplate:    template,
		hostPortUsage:        scheduling.NewHostPortUsage(),
		topology:             topology,
		daemonResources:      daemonResources,
		hostname:             hostname,
		reservedOfferings:    map[string]cloudprovider.Offerings{},
		reservationManager:   reservationManager,
		reservedOfferingMode: reservedOfferingMode,
	}
}

func (n *NodeClaim) Add(pod *corev1.Pod, podData *PodData) error {
	// Check Taints
	if err := scheduling.Taints(n.Spec.Taints).ToleratesPod(pod); err != nil {
		return err
	}

	// exposed host ports on the node
	hostPorts := scheduling.GetHostPorts(pod)
	if err := n.hostPortUsage.Conflicts(pod, hostPorts); err != nil {
		return fmt.Errorf("checking host port usage, %w", err)
	}
	nodeClaimRequirements := scheduling.NewRequirements(n.Requirements.Values()...)

	// Check NodeClaim Affinity Requirements
	if err := nodeClaimRequirements.Compatible(podData.Requirements, scheduling.AllowUndefinedWellKnownLabels); err != nil {
		return fmt.Errorf("incompatible requirements, %w", err)
	}
	nodeClaimRequirements.Add(podData.Requirements.Values()...)

	// Check Topology Requirements
	topologyRequirements, err := n.topology.AddRequirements(pod, n.NodeClaimTemplate.Spec.Taints, podData.StrictRequirements, nodeClaimRequirements, scheduling.AllowUndefinedWellKnownLabels)
	if err != nil {
		return err
	}
	if err = nodeClaimRequirements.Compatible(topologyRequirements, scheduling.AllowUndefinedWellKnownLabels); err != nil {
		return err
	}
	nodeClaimRequirements.Add(topologyRequirements.Values()...)

	// Check instance type combinations
	requests := resources.Merge(n.Spec.Resources.Requests, podData.Requests)

	remaining, err := filterInstanceTypesByRequirements(n.InstanceTypeOptions, nodeClaimRequirements, podData.Requests, n.daemonResources, requests)
	if err != nil {
		// We avoid wrapping this err because calling String() on InstanceTypeFilterError is an expensive operation
		// due to calls to resources.Merge and stringifying the nodeClaimRequirements
		return err
	}

	reservedOfferings, offeringsToRelease, err := n.reserveOfferings(remaining, nodeClaimRequirements)
	if err != nil {
		return err
	}

	// Update node
	n.Pods = append(n.Pods, pod)
	n.InstanceTypeOptions = remaining
	n.Spec.Resources.Requests = requests
	n.Requirements = nodeClaimRequirements
	n.topology.Record(pod, n.NodeClaim.Spec.Taints, nodeClaimRequirements, scheduling.AllowUndefinedWellKnownLabels)
	n.hostPortUsage.Add(pod, hostPorts)
	n.reservedOfferings = reservedOfferings
	for _, o := range offeringsToRelease {
		n.reservationManager.Release(n.hostname, o)
	}
	return nil
}

// reserveOfferings handles the reservation of `karpenter.sh/capacity-type: reserved` offerings, returning the reserved
// offerings in a map from instance type name to offerings. Additionally, a slice of offerings to release is returned.
// This is based on the previously reserved offerings which are no longer compatible with the nodeclaim. These should
// not be released until we're ready to persist the changes to the nodeclaim.
// nolint:gocyclo
func (n *NodeClaim) reserveOfferings(
	instanceTypes []*cloudprovider.InstanceType,
	nodeClaimRequirements scheduling.Requirements,
) (reservedOfferings map[string]cloudprovider.Offerings, offeringsToRelease []*cloudprovider.Offering, err error) {
	compatibleReservedInstanceTypes := sets.New[string]()
	reservedOfferings = map[string]cloudprovider.Offerings{}
	for _, it := range instanceTypes {
		hasCompatibleOffering := false
		for _, o := range it.Offerings {
			if o.CapacityType() != v1.CapacityTypeReserved || !o.Available {
				continue
			}
			// Track every incompatible reserved offering for release. Since releasing a reservation is a no-op when there is no
			// reservation for the given host, there's no need to check that a reservation actually exists for the offering.
			if !nodeClaimRequirements.IsCompatible(o.Requirements, scheduling.AllowUndefinedWellKnownLabels) {
				offeringsToRelease = append(offeringsToRelease, o)
				continue
			}
			hasCompatibleOffering = true
			// Note that reservation is an idempotent operation - if we have previously successfully reserved an offering for
			// this host, this operation is guaranteed to succeed. As such, reservedOfferings is guaranteed to be a subset of
			// the previous set of reserved offerings.
			if n.reservationManager.Reserve(n.hostname, o) {
				reservedOfferings[it.Name] = append(reservedOfferings[it.Name], o)
			}
		}
		if hasCompatibleOffering {
			compatibleReservedInstanceTypes.Insert(it.Name)
		}
	}

	if n.reservedOfferingMode == ReservedOfferingModeStrict {
		// If an instance type with a compatible reserved offering exists, but we failed to make any reservations, we should
		// fail. We include this check due to the pessimistic nature of instance reservation - we have to reserve each potential
		// offering for every nodeclaim, since we don't know what we'll launch with. Without this check we would fall back to
		// on-demand / spot even when there's sufficient reserved capacity. This check means we may fail to schedule the pod
		// during this scheduling simulation, but with the possibility of success on subsequent simulations.
		// Note: while this can occur both on initial creation and on
		if len(compatibleReservedInstanceTypes) != 0 && len(reservedOfferings) == 0 {
			return nil, nil, NewReservedOfferingError(fmt.Errorf("one or more instance types with compatible reserved offerings are available, but could not be reserved"))
		}
		// If the nodeclaim previously had compatible reserved offerings, but the additional requirements filtered those out,
		// we should fail to add the pod to this nodeclaim.
		if len(n.reservedOfferings) != 0 && len(reservedOfferings) == 0 {
			return nil, nil, NewReservedOfferingError(fmt.Errorf("satisfying updated nodeclaim constraints would remove all compatible reserved offering options"))
		}
	}
	// Ensure we release any offerings for instance types which are no longer compatible with nodeClaimRequirements
	for instanceName, offerings := range n.reservedOfferings {
		if compatibleReservedInstanceTypes.Has(instanceName) {
			continue
		}
		offeringsToRelease = append(offeringsToRelease, offerings...)
	}
	return reservedOfferings, offeringsToRelease, nil
}

func (n *NodeClaim) Destroy() {
	n.topology.Unregister(corev1.LabelHostname, n.hostname)
	for _, ofs := range n.reservedOfferings {
		n.reservationManager.Release(n.hostname, ofs...)
	}
}

// FinalizeScheduling is called once all scheduling has completed and allows the node to perform any cleanup
// necessary before its requirements are used for instance launching
func (n *NodeClaim) FinalizeScheduling() {
	// We need nodes to have hostnames for topology purposes, but we don't want to pass that node name on to consumers
	// of the node as it will be displayed in error messages
	delete(n.Requirements, corev1.LabelHostname)
	// If there are any reserved offerings tracked, inject those requirements onto the NodeClaim. This ensures that if
	// there are multiple reserved offerings for an instance type, we don't attempt to overlaunch into a single offering.
	if len(n.reservedOfferings) != 0 {
		n.Requirements.Add(scheduling.NewRequirement(
			cloudprovider.ReservationIDLabel,
			corev1.NodeSelectorOpIn,
			lo.FlatMap(lo.Values(n.reservedOfferings), func(ofs cloudprovider.Offerings, _ int) []string {
				return lo.Map(ofs, func(o *cloudprovider.Offering, _ int) string { return o.ReservationID() })
			})...,
		))
	}
}

func (n *NodeClaim) RemoveInstanceTypeOptionsByPriceAndMinValues(reqs scheduling.Requirements, maxPrice float64) (*NodeClaim, error) {
	n.InstanceTypeOptions = lo.Filter(n.InstanceTypeOptions, func(it *cloudprovider.InstanceType, _ int) bool {
		launchPrice := it.Offerings.Available().WorstLaunchPrice(reqs)
		return launchPrice < maxPrice
	})
	if _, err := n.InstanceTypeOptions.SatisfiesMinValues(reqs); err != nil {
		return nil, err
	}
	return n, nil
}

func InstanceTypeList(instanceTypeOptions []*cloudprovider.InstanceType) string {
	var itSb strings.Builder
	for i, it := range instanceTypeOptions {
		// print the first 5 instance types only (indices 0-4)
		if i > 4 {
			fmt.Fprintf(&itSb, " and %d other(s)", len(instanceTypeOptions)-i)
			break
		} else if i > 0 {
			fmt.Fprint(&itSb, ", ")
		}
		fmt.Fprint(&itSb, it.Name)
	}
	return itSb.String()
}

type InstanceTypeFilterError struct {
	// Each of these three flags indicates if that particular criteria was met by at least one instance type
	requirementsMet bool
	fits            bool
	hasOffering     bool

	// requirementsAndFits indicates if a single instance type met the scheduling requirements and had enough resources
	requirementsAndFits bool
	// requirementsAndOffering indicates if a single instance type met the scheduling requirements and was a required offering
	requirementsAndOffering bool
	// fitsAndOffering indicates if a single instance type had enough resources and was a required offering
	fitsAndOffering          bool
	minValuesIncompatibleErr error

	// We capture requirements so that we can know what the requirements were when evaluating instance type compatibility
	requirements scheduling.Requirements
	// We capture podRequests here since when a pod can't schedule due to requests, it's because the pod
	// was on its own on the simulated Node and exceeded the available resources for any instance type for this NodePool
	podRequests corev1.ResourceList
	// We capture daemonRequests since this contributes to the resources that are required to schedule to this NodePool
	daemonRequests corev1.ResourceList
}

//nolint:gocyclo
func (e InstanceTypeFilterError) Error() string {
	// minValues is specified in the requirements and is not met
	if e.minValuesIncompatibleErr != nil {
		return fmt.Sprintf("%s, requirements=%s, resources=%s", e.minValuesIncompatibleErr.Error(), e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
	}
	// no instance type met any of the three criteria, meaning each criteria was enough to completely prevent
	// this pod from scheduling
	if !e.requirementsMet && !e.fits && !e.hasOffering {
		return fmt.Sprintf("no instance type met the scheduling requirements or had enough resources or had a required offering, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
	}
	// check the other pairwise criteria
	if !e.requirementsMet && !e.fits {
		return fmt.Sprintf("no instance type met the scheduling requirements or had enough resources, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
	}
	if !e.requirementsMet && !e.hasOffering {
		return fmt.Sprintf("no instance type met the scheduling requirements or had a required offering, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
	}
	if !e.fits && !e.hasOffering {
		return fmt.Sprintf("no instance type had enough resources or had a required offering, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
	}
	// and then each individual criteria. These are sort of the same as above in that each one indicates that no
	// instance type matched that criteria at all, so it was enough to exclude all instance types.  I think it's
	// helpful to have these separate, since we can report the multiple excluding criteria above.
	if !e.requirementsMet {
		return fmt.Sprintf("no instance type met all requirements, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
	}
	if !e.fits {
		msg := fmt.Sprintf("no instance type has enough resources, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
		// special case for a user typo I saw reported once
		if e.podRequests.Cpu().Cmp(resource.MustParse("1M")) >= 0 {
			msg += " (CPU request >= 1 Million, m vs M typo?)"
		}
		return msg
	}
	if !e.hasOffering {
		return fmt.Sprintf("no instance type has the required offering, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
	}
	// see if any pair of criteria was enough to exclude all instances
	if e.requirementsAndFits {
		return fmt.Sprintf("no instance type which met the scheduling requirements and had enough resources, had a required offering, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
	}
	if e.fitsAndOffering {
		return fmt.Sprintf("no instance type which had enough resources and the required offering met the scheduling requirements, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
	}
	if e.requirementsAndOffering {
		return fmt.Sprintf("no instance type which met the scheduling requirements and the required offering had the required resources, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
	}
	// finally all instances were filtered out, but we had at least one instance that met each criteria, and met each
	// pairwise set of criteria, so the only thing that remains is no instance which met all three criteria simultaneously
	return fmt.Sprintf("no instance type met the requirements/resources/offering tuple, requirements=%s, resources=%s", e.requirements, resources.String(resources.Merge(e.daemonRequests, e.podRequests)))
}

//nolint:gocyclo
func filterInstanceTypesByRequirements(instanceTypes []*cloudprovider.InstanceType, requirements scheduling.Requirements, podRequests, daemonRequests, totalRequests corev1.ResourceList) (cloudprovider.InstanceTypes, error) {
	// We hold the results of our scheduling simulation inside of this InstanceTypeFilterError struct
	// to reduce the CPU load of having to generate the error string for a failed scheduling simulation
	err := InstanceTypeFilterError{
		requirementsMet: false,
		fits:            false,
		hasOffering:     false,

		requirementsAndFits:     false,
		requirementsAndOffering: false,
		fitsAndOffering:         false,

		podRequests:    podRequests,
		daemonRequests: daemonRequests,
	}
	remaining := cloudprovider.InstanceTypes{}

	for _, it := range instanceTypes {
		// the tradeoff to not short-circuiting on the filtering is that we can report much better error messages
		// about why scheduling failed
		itCompat := compatible(it, requirements)
		itFits := fits(it, totalRequests)

		// By using this iterative approach vs. the Available() function it prevents allocations
		// which have to be garbage collected and slow down Karpenter's scheduling algorithm
		itHasOffering := false
		for _, of := range it.Offerings {
			if of.Available && requirements.IsCompatible(of.Requirements, scheduling.AllowUndefinedWellKnownLabels) {
				itHasOffering = true
				break
			}
		}

		// track if any single instance type met a single criteria
		err.requirementsMet = err.requirementsMet || itCompat
		err.fits = err.fits || itFits
		err.hasOffering = err.hasOffering || itHasOffering

		// track if any single instance type met the three pairs of criteria
		err.requirementsAndFits = err.requirementsAndFits || (itCompat && itFits && !itHasOffering)
		err.requirementsAndOffering = err.requirementsAndOffering || (itCompat && itHasOffering && !itFits)
		err.fitsAndOffering = err.fitsAndOffering || (itFits && itHasOffering && !itCompat)

		// and if it met all criteria, we keep the instance type and continue filtering.  We now won't be reporting
		// any errors.
		if itCompat && itFits && itHasOffering {
			remaining = append(remaining, it)
		}
	}

	if requirements.HasMinValues() {
		// We don't care about the minimum number of instance types that meet our requirements here, we only care if they meet our requirements.
		_, err.minValuesIncompatibleErr = remaining.SatisfiesMinValues(requirements)
		if err.minValuesIncompatibleErr != nil {
			// If minValues is NOT met for any of the requirement across InstanceTypes, then return empty InstanceTypeOptions as we cannot launch with the remaining InstanceTypes.
			remaining = nil
		}
	}
	if len(remaining) == 0 {
		return nil, err
	}
	return remaining, nil
}

func compatible(instanceType *cloudprovider.InstanceType, requirements scheduling.Requirements) bool {
	return instanceType.Requirements.Intersects(requirements) == nil
}

func fits(instanceType *cloudprovider.InstanceType, requests corev1.ResourceList) bool {
	return resources.Fits(requests, instanceType.Allocatable())
}
