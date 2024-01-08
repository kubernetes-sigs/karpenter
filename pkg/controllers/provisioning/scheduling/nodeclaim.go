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
	"fmt"
	"strings"
	"sync/atomic"

	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// NodeClaim is a set of constraints, compatible pods, and possible instance types that could fulfill these constraints. This
// will be turned into one or more actual node instances within the cluster after bin packing.
type NodeClaim struct {
	NodeClaimTemplate

	Pods            []*v1.Pod
	topology        *Topology
	hostPortUsage   *scheduling.HostPortUsage
	daemonResources v1.ResourceList
}

var nodeID int64

func NewNodeClaim(nodeClaimTemplate *NodeClaimTemplate, topology *Topology, daemonResources v1.ResourceList, instanceTypes []*cloudprovider.InstanceType) *NodeClaim {
	// Copy the template, and add hostname
	hostname := fmt.Sprintf("hostname-placeholder-%04d", atomic.AddInt64(&nodeID, 1))
	topology.Register(v1.LabelHostname, hostname)
	template := *nodeClaimTemplate
	template.Requirements = scheduling.NewRequirements()
	template.Requirements.Add(nodeClaimTemplate.Requirements.Values()...)
	template.Requirements.Add(scheduling.NewRequirement(v1.LabelHostname, v1.NodeSelectorOpIn, hostname))
	template.InstanceTypeOptions = instanceTypes
	template.Spec.Resources.Requests = daemonResources

	return &NodeClaim{
		NodeClaimTemplate: template,
		hostPortUsage:     scheduling.NewHostPortUsage(),
		topology:          topology,
		daemonResources:   daemonResources,
	}
}

func (n *NodeClaim) Add(pod *v1.Pod) error {
	// Check Taints
	if err := scheduling.Taints(n.Spec.Taints).Tolerates(pod); err != nil {
		return err
	}

	// exposed host ports on the node
	hostPorts := scheduling.GetHostPorts(pod)
	if err := n.hostPortUsage.Conflicts(pod, hostPorts); err != nil {
		return fmt.Errorf("checking host port usage, %w", err)
	}

	nodeClaimRequirements := scheduling.NewRequirements(n.Requirements.Values()...)
	podRequirements := scheduling.NewPodRequirements(pod)

	// Check NodeClaim Affinity Requirements
	if err := nodeClaimRequirements.Compatible(podRequirements, scheduling.AllowUndefinedWellKnownLabels); err != nil {
		return fmt.Errorf("incompatible requirements, %w", err)
	}
	nodeClaimRequirements.Add(podRequirements.Values()...)

	strictPodRequirements := podRequirements
	if scheduling.HasPreferredNodeAffinity(pod) {
		// strictPodRequirements is important as it ensures we don't inadvertently restrict the possible pod domains by a
		// preferred node affinity.  Only required node affinities can actually reduce pod domains.
		strictPodRequirements = scheduling.NewStrictPodRequirements(pod)
	}
	// Check Topology Requirements
	topologyRequirements, err := n.topology.AddRequirements(strictPodRequirements, nodeClaimRequirements, pod, scheduling.AllowUndefinedWellKnownLabels)
	if err != nil {
		return err
	}
	if err = nodeClaimRequirements.Compatible(topologyRequirements, scheduling.AllowUndefinedWellKnownLabels); err != nil {
		return err
	}
	nodeClaimRequirements.Add(topologyRequirements.Values()...)

	// Check instance type combinations
	requests := resources.Merge(n.Spec.Resources.Requests, resources.RequestsForPods(pod))
	filtered := filterInstanceTypesByRequirements(n.InstanceTypeOptions, nodeClaimRequirements, requests)
	if len(filtered.remaining) == 0 {
		return fmt.Errorf(filtered.reason)
	}

	// Update node
	n.Pods = append(n.Pods, pod)
	n.InstanceTypeOptions = filtered.remaining
	n.Spec.Resources.Requests = requests
	n.Requirements = nodeClaimRequirements
	n.topology.Record(pod, nodeClaimRequirements, scheduling.AllowUndefinedWellKnownLabels)
	n.hostPortUsage.Add(pod, hostPorts)
	return nil
}

// FinalizeScheduling is called once all scheduling has completed and allows the node to perform any cleanup
// necessary before its requirements are used for instance launching
func (n *NodeClaim) FinalizeScheduling() {
	// We need nodes to have hostnames for topology purposes, but we don't want to pass that node name on to consumers
	// of the node as it will be displayed in error messages
	delete(n.Requirements, v1.LabelHostname)
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

type filterResults struct {
	remaining []*cloudprovider.InstanceType
	// Each of these three flags indicates if that particular criteria was met by at least one instance type
	requirementsMet bool
	fits            bool
	hasOffering     bool

	// requirementsAndFits indicates if a single instance type met the scheduling requirements and had enough resources
	requirementsAndFits bool
	// requirementsAndOffering indicates if a single instance type met the scheduling requirements and was a required offering
	requirementsAndOffering bool
	// fitsAndOffering indicates if a single instance type had enough resources and was a required offering
	fitsAndOffering bool

	requests v1.ResourceList
	reason   string
}

//nolint:gocyclo
func filterInstanceTypesByRequirements(instanceTypes []*cloudprovider.InstanceType, requirements scheduling.Requirements, requests v1.ResourceList) filterResults {
	results := filterResults{
		requests:                requests,
		requirementsMet:         false,
		fits:                    false,
		hasOffering:             false,
		reason:                  "",
		requirementsAndFits:     false,
		requirementsAndOffering: false,
		fitsAndOffering:         false,
	}
	for _, it := range instanceTypes {
		// the tradeoff to not short circuiting on the filtering is that we can report much better error messages
		// about why scheduling failed
		itCompat := compatible(it, requirements)
		itFits, itUnfitReason := fits(it, requests)
		itHasOffering, offeringReason := hasOffering(it, requirements)

		// track if any single instance type met a single criteria
		results.requirementsMet = results.requirementsMet || itCompat
		results.fits = results.fits || itFits
		results.hasOffering = results.hasOffering || itHasOffering

		// track if any single instance type met the three pairs of criteria
		results.requirementsAndFits = results.requirementsAndFits || (itCompat && itFits && !itHasOffering)
		results.requirementsAndOffering = results.requirementsAndOffering || (itCompat && itHasOffering && !itFits)
		results.fitsAndOffering = results.fitsAndOffering || (itFits && itHasOffering && !itCompat)

		// and if it met all criteria, we keep the instance type and continue filtering.  We now won't be reporting
		// any errors.
		if itCompat && itFits && itHasOffering {
			results.remaining = append(results.remaining, it)
		} else {
			if !itFits && !itHasOffering && !results.requirementsMet {
				results.reason = fmt.Sprintf("%s, %s, %s, %s", it.Name, itUnfitReason, offeringReason, it.Requirements.Intersects(requirements))
			} else if !itFits && !itHasOffering {
				results.reason = fmt.Sprintf("%s, %s, %s", it.Name, itUnfitReason, offeringReason)
			} else if !itCompat && !itHasOffering {
				results.reason = fmt.Sprintf("%s, %s, %s", it.Name, it.Requirements.Intersects(requirements), offeringReason)
			} else if !itCompat && !itFits {
				results.reason = fmt.Sprintf("%s, %s, %s", it.Name, it.Requirements.Intersects(requirements), itUnfitReason)
			} else if !itCompat {
				results.reason = fmt.Sprintf("%s, %s", it.Name, it.Requirements.Intersects(requirements))
			} else if !itFits {
				results.reason = fmt.Sprintf("%s, %s", it.Name, itUnfitReason)
			} else if !itHasOffering {
				results.reason = fmt.Sprintf("%s, %s", it.Name, offeringReason)
			}
		}
	}
	return results
}

func compatible(instanceType *cloudprovider.InstanceType, requirements scheduling.Requirements) bool {
	return instanceType.Requirements.Intersects(requirements) == nil
}

func fits(instanceType *cloudprovider.InstanceType, requests v1.ResourceList) (bool, string) {
	return resources.Fits(requests, instanceType.Allocatable())
}

func hasOffering(instanceType *cloudprovider.InstanceType, requirements scheduling.Requirements) (bool, string) {
	for _, offering := range instanceType.Offerings.Available() {
		if (!requirements.Has(v1.LabelTopologyZone) || requirements.Get(v1.LabelTopologyZone).Has(offering.Zone)) &&
			(!requirements.Has(v1beta1.CapacityTypeLabelKey) || requirements.Get(v1beta1.CapacityTypeLabelKey).Has(offering.CapacityType)) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("no offering found for requirements %s", requirements)
}
