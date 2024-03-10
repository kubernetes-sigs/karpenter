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
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

type DriftReason string

// CloudProvider interface is implemented by cloud providers to support provisioning.
type CloudProvider interface {
	// Create launches a NodeClaim with the given resource requests and requirements and returns a hydrated
	// NodeClaim back with resolved NodeClaim labels for the launched NodeClaim
	Create(context.Context, *v1beta1.NodeClaim) (*v1beta1.NodeClaim, error)
	// Delete removes a NodeClaim from the cloudprovider by its provider id
	Delete(context.Context, *v1beta1.NodeClaim) error
	// Get retrieves a NodeClaim from the cloudprovider by its provider id
	Get(context.Context, string) (*v1beta1.NodeClaim, error)
	// List retrieves all NodeClaims from the cloudprovider
	List(context.Context) ([]*v1beta1.NodeClaim, error)
	// GetInstanceTypes returns instance types supported by the cloudprovider.
	// Availability of types or zone may vary by nodepool or over time.  Regardless of
	// availability, the GetInstanceTypes method should always return all instance types,
	// even those with no offerings available.
	GetInstanceTypes(context.Context, *v1beta1.NodePool) ([]*InstanceType, error)
	// IsDrifted returns whether a NodeClaim has drifted from the provisioning requirements
	// it is tied to.
	IsDrifted(context.Context, *v1beta1.NodeClaim) (DriftReason, error)
	// Name returns the CloudProvider implementation name.
	Name() string
	// GetSupportedNodeClass returns the group, version, and kind of the CloudProvider NodeClass
	GetSupportedNodeClass() schema.GroupVersionKind
}

// InstanceType describes the properties of a potential node (either concrete attributes of an instance of this type
// or supported options in the case of arrays)
type InstanceType struct {
	// Name of the instance type, must correspond to v1.LabelInstanceTypeStable
	Name string
	// Requirements returns a flexible set of properties that may be selected
	// for scheduling. Must be defined for every well known label, even if empty.
	Requirements scheduling.Requirements
	// Note that though this is an array it is expected that all the Offerings are unique from one another
	Offerings Offerings
	// Resources are the full resource capacities for this instance type
	Capacity v1.ResourceList
	// Overhead is the amount of resource overhead expected to be used by kubelet and any other system daemons outside
	// of Kubernetes.
	Overhead *InstanceTypeOverhead

	once        sync.Once
	allocatable v1.ResourceList
}

type InstanceTypes []*InstanceType

// precompute is used to ensure we only compute the allocatable resources onces as its called many times
// and the operation is fairly expensive.
func (i *InstanceType) precompute() {
	i.allocatable = resources.Subtract(i.Capacity, i.Overhead.Total())
}

func (i *InstanceType) Allocatable() v1.ResourceList {
	i.once.Do(i.precompute)
	return i.allocatable.DeepCopy()
}

func (its InstanceTypes) OrderByPrice(reqs scheduling.Requirements) InstanceTypes {
	// Order instance types so that we get the cheapest instance types of the available offerings
	sort.Slice(its, func(i, j int) bool {
		iPrice := math.MaxFloat64
		jPrice := math.MaxFloat64
		if len(its[i].Offerings.Available().Compatible(reqs)) > 0 {
			iPrice = its[i].Offerings.Available().Compatible(reqs).Cheapest().Price
		}
		if len(its[j].Offerings.Available().Compatible(reqs)) > 0 {
			jPrice = its[j].Offerings.Available().Compatible(reqs).Cheapest().Price
		}
		if iPrice == jPrice {
			return its[i].Name < its[j].Name
		}
		return iPrice < jPrice
	})
	return its
}

// Compatible returns the list of instanceTypes based on the supported capacityType and zones in the requirements
func (its InstanceTypes) Compatible(requirements scheduling.Requirements) InstanceTypes {
	var filteredInstanceTypes []*InstanceType
	for _, instanceType := range its {
		if len(instanceType.Offerings.Available().Compatible(requirements)) > 0 {
			filteredInstanceTypes = append(filteredInstanceTypes, instanceType)
		}
	}
	return filteredInstanceTypes
}

type InstanceTypeOverhead struct {
	// KubeReserved returns the default resources allocated to kubernetes system daemons by default
	KubeReserved v1.ResourceList
	// SystemReserved returns the default resources allocated to the OS system daemons by default
	SystemReserved v1.ResourceList
	// EvictionThreshold returns the resources used to maintain a hard eviction threshold
	EvictionThreshold v1.ResourceList
}

func (i InstanceTypeOverhead) Total() v1.ResourceList {
	return resources.Merge(i.KubeReserved, i.SystemReserved, i.EvictionThreshold)
}

// An Offering describes where an InstanceType is available to be used, with the expectation that its properties
// may be tightly coupled (e.g. the availability of an instance type in some zone is scoped to a capacity type)
type Offering struct {
	CapacityType string
	Zone         string
	Price        float64
	// Available is added so that Offerings can return all offerings that have ever existed for an instance type,
	// so we can get historical pricing data for calculating savings in consolidation
	Available bool
}

type Offerings []Offering

// Get gets the offering from an offering slice that matches the
// passed zone and capacity type
func (ofs Offerings) Get(ct, zone string) (Offering, bool) {
	return lo.Find(ofs, func(of Offering) bool {
		return of.CapacityType == ct && of.Zone == zone
	})
}

// Available filters the available offerings from the returned offerings
func (ofs Offerings) Available() Offerings {
	return lo.Filter(ofs, func(o Offering, _ int) bool {
		return o.Available
	})
}

// Compatible returns the offerings based on the passed requirements
func (ofs Offerings) Compatible(reqs scheduling.Requirements) Offerings {
	return lo.Filter(ofs, func(offering Offering, _ int) bool {
		return (!reqs.Has(v1.LabelTopologyZone) || reqs.Get(v1.LabelTopologyZone).Has(offering.Zone)) &&
			(!reqs.Has(v1beta1.CapacityTypeLabelKey) || reqs.Get(v1beta1.CapacityTypeLabelKey).Has(offering.CapacityType))
	})
}

// Cheapest returns the cheapest offering from the returned offerings
func (ofs Offerings) Cheapest() Offering {
	return lo.MinBy(ofs, func(a, b Offering) bool {
		return a.Price < b.Price
	})
}

// NodeClaimNotFoundError is an error type returned by CloudProviders when the reason for failure is NotFound
type NodeClaimNotFoundError struct {
	error
}

func NewNodeClaimNotFoundError(err error) *NodeClaimNotFoundError {
	return &NodeClaimNotFoundError{
		error: err,
	}
}

func (e *NodeClaimNotFoundError) Error() string {
	return fmt.Sprintf("nodeclaim not found, %s", e.error)
}

func IsNodeClaimNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var mnfErr *NodeClaimNotFoundError
	return errors.As(err, &mnfErr)
}

func IgnoreNodeClaimNotFoundError(err error) error {
	if IsNodeClaimNotFoundError(err) {
		return nil
	}
	return err
}

// InsufficientCapacityError is an error type returned by CloudProviders when a launch fails due to a lack of capacity from NodeClaim requirements
type InsufficientCapacityError struct {
	error
}

func NewInsufficientCapacityError(err error) *InsufficientCapacityError {
	return &InsufficientCapacityError{
		error: err,
	}
}

func (e *InsufficientCapacityError) Error() string {
	return fmt.Sprintf("insufficient capacity, %s", e.error)
}

func IsInsufficientCapacityError(err error) bool {
	if err == nil {
		return false
	}
	var icErr *InsufficientCapacityError
	return errors.As(err, &icErr)
}

func IgnoreInsufficientCapacityError(err error) error {
	if IsInsufficientCapacityError(err) {
		return nil
	}
	return err
}

// NodeClassNotReadyError is an error type returned by CloudProviders when a NodeClass that is used by the launch process doesn't have all its resolved fields
type NodeClassNotReadyError struct {
	error
}

func NewNodeClassNotReadyError(err error) *NodeClassNotReadyError {
	return &NodeClassNotReadyError{
		error: err,
	}
}

func (e *NodeClassNotReadyError) Error() string {
	return fmt.Sprintf("NodeClassRef not ready, %s", e.error)
}

func IsNodeClassNotReadyError(err error) bool {
	if err == nil {
		return false
	}
	var nrError *NodeClassNotReadyError
	return errors.As(err, &nrError)
}

func IgnoreNodeClassNotReadyError(err error) error {
	if IsNodeClassNotReadyError(err) {
		return nil
	}
	return err
}
