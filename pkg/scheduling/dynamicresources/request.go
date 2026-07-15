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

package dynamicresources

import (
	"context"
	"fmt"
	"math"

	"github.com/samber/lo"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	dracel "k8s.io/dynamic-resource-allocation/cel"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// deviceRequestAccessor abstracts the common fields between each type of DeviceRequest.
type deviceRequestAccessor interface {
	DeviceClassName() string
	Selectors() []resourcev1.DeviceSelector
	AllocationMode() resourcev1.DeviceAllocationMode
	Count() int64
	Capacity() *resourcev1.CapacityRequirements
}

type exactRequestAccessor struct {
	req *resourcev1.ExactDeviceRequest
}

func (a exactRequestAccessor) DeviceClassName() string {
	return a.req.DeviceClassName
}
func (a exactRequestAccessor) Selectors() []resourcev1.DeviceSelector {
	return a.req.Selectors
}
func (a exactRequestAccessor) AllocationMode() resourcev1.DeviceAllocationMode {
	return a.req.AllocationMode
}
func (a exactRequestAccessor) Count() int64 {
	return a.req.Count
}
func (a exactRequestAccessor) Capacity() *resourcev1.CapacityRequirements {
	return a.req.Capacity
}

type subRequestAccessor struct {
	sub *resourcev1.DeviceSubRequest
}

func (a subRequestAccessor) DeviceClassName() string {
	return a.sub.DeviceClassName
}
func (a subRequestAccessor) Selectors() []resourcev1.DeviceSelector {
	return a.sub.Selectors
}
func (a subRequestAccessor) AllocationMode() resourcev1.DeviceAllocationMode {
	return a.sub.AllocationMode
}
func (a subRequestAccessor) Count() int64 {
	return a.sub.Count
}
func (a subRequestAccessor) Capacity() *resourcev1.CapacityRequirements {
	return a.sub.Capacity
}

// RequestData holds the parsed and validated metadata for a single device request.
type RequestData struct {
	// Name identifies this request. For Exactly requests, Parent is set and Sub is empty.
	// For FirstAvailable sub-requests, Parent is the top-level request name and Sub is
	// the sub-request's own name.
	Name RequestName

	// SubRequests holds the ordered alternatives for a FirstAvailable request.
	// nil for Exactly requests and for sub-request entries.
	SubRequests []RequestData

	// Class is the resolved DeviceClass for this request.
	Class *resourcev1.DeviceClass
	// NumDevices is the number of devices to allocate (for ExactCount mode).
	// For All mode, this is 0 — the actual count is determined per instance type
	// from AllDevices and AllTemplateDevicesByIT during allocation.
	NumDevices int
	// AllocationMode is ExactCount or All.
	AllocationMode resourcev1.DeviceAllocationMode
	// AllDevices holds the pre-determined eligible in-cluster devices for All mode.
	// nil when AllocationMode is not All.
	AllDevices []DeviceWithID
	// AllTemplateDevicesByIT holds the pre-determined eligible template devices per
	// instance type for All mode. Each entry contains only that instance type's
	// template devices that match the request's selectors. nil when AllocationMode
	// is not All or when there are no template devices.
	AllTemplateDevicesByIT map[InstanceTypeID][]DeviceWithID
	// Selectors is the combined set of selectors from the class and request.
	Selectors []resourcev1.DeviceSelector
	// CapacityRequests contains the per-dimension capacity requirements from
	// ExactDeviceRequest.Capacity.Requests. nil when no capacity is requested.
	CapacityRequests map[resourcev1.QualifiedName]resource.Quantity
}

// ClaimData holds the parsed constraints and requests for a single ResourceClaim.
type ClaimData struct {
	ID          ResourceClaimID
	Requests    []RequestData
	Constraints []Constraint
}

// ValidateClaimRequest parses a ResourceClaim, resolves its DeviceClass references, and builds
// the constraint and request metadata needed for allocation. Returns an error if any request
// is invalid (missing class, unsupported selector/constraint type, etc.).
//
//nolint:gocyclo
func ValidateClaimRequest(
	ctx context.Context,
	kubeClient client.Client,
	claim *resourcev1.ResourceClaim,
	pools []*Pool,
	templateDevicesByIT map[InstanceTypeID][]DeviceWithID,
	celCache *dracel.Cache,
	bindingFallback *AttributeBindingFallback,
) (*ClaimData, error) {
	data := &ClaimData{
		ID: resourceClaimID(claim),
	}

	// Build constraints.
	for _, c := range claim.Spec.Devices.Constraints {
		switch {
		case c.MatchAttribute != nil:
			mac := &MatchAttributeConstraint{
				RequestNames:             sets.New(c.Requests...),
				AttributeName:            resourcev1.QualifiedName(*c.MatchAttribute),
				AttributeBindingFallback: bindingFallback,
			}
			data.Constraints = append(data.Constraints, mac)
		case c.DistinctAttribute != nil:
			return nil, fmt.Errorf("claim %q: DistinctAttribute constraints not done yet", claim.Name)
		default:
			return nil, fmt.Errorf("claim %q: unsupported constraint type", claim.Name)
		}
	}

	// Validate requests.
	for i := range claim.Spec.Devices.Requests {
		req := &claim.Spec.Devices.Requests[i]

		var rd *RequestData
		var err error
		switch {
		case req.Exactly != nil:
			rd, err = validateExactRequest(ctx, kubeClient, claim.Name, req.Name, req.Exactly, pools, templateDevicesByIT, celCache)

		case len(req.FirstAvailable) > 0:
			rd, err = validateFirstAvailableRequest(ctx, kubeClient, claim.Name, req.Name, req.FirstAvailable, pools, templateDevicesByIT, celCache)

		default:
			return nil, fmt.Errorf("claim %q request %q: only Exactly and FirstAvailable requests are supported", claim.Name, req.Name)
		}
		if err != nil {
			return nil, err
		}
		data.Requests = append(data.Requests, *rd)
	}

	// Compute the base device total: ExactCount requests contribute NumDevices,
	// All-mode requests contribute their in-cluster device count (len(AllDevices)).
	// This base total is constant regardless of instance type.
	// For FirstAvailable, we use the min across sub-requests. Because this can pass while
	// no combination of sub-request selections actually fits, the per-claim check runs
	// during the DFS.
	var baseTotalDevices int
	for _, req := range data.Requests {
		if len(req.SubRequests) > 0 {
			minDevices := math.MaxInt
			for _, sub := range req.SubRequests {
				minDevices = min(minDevices, sub.NumDevices+len(sub.AllDevices))
			}
			baseTotalDevices += minDevices
		} else {
			baseTotalDevices += req.NumDevices
			baseTotalDevices += len(req.AllDevices)
		}
	}
	maxDevices := int(resourcev1.AllocationResultsMaxSize)
	if baseTotalDevices > maxDevices {
		return nil, fmt.Errorf("claim %q requests %d total devices, exceeding maximum of %d",
			claim.Name, baseTotalDevices, maxDevices)
	}

	// Prune instance types whose template devices would push the total over the limit.
	// Collect all unique instance type IDs across All-mode requests.
	allITs := sets.New[InstanceTypeID]()
	for _, req := range data.Requests {
		if len(req.SubRequests) > 0 {
			for _, sub := range req.SubRequests {
				for itID := range sub.AllTemplateDevicesByIT {
					allITs.Insert(itID)
				}
			}
		} else {
			for itID := range req.AllTemplateDevicesByIT {
				allITs.Insert(itID)
			}
		}
	}
	var prunedCount int
	for itID := range allITs {
		var templateCount int
		for _, req := range data.Requests {
			if len(req.SubRequests) > 0 {
				minForReq := math.MaxInt
				for _, sub := range req.SubRequests {
					minForReq = min(minForReq, len(sub.AllTemplateDevicesByIT[itID]))
				}
				templateCount += minForReq
			} else {
				templateCount += len(req.AllTemplateDevicesByIT[itID])
			}
		}
		if baseTotalDevices+templateCount > maxDevices {
			prunedCount += 1
			for i := range data.Requests {
				if len(data.Requests[i].SubRequests) > 0 {
					for j := range data.Requests[i].SubRequests {
						delete(data.Requests[i].SubRequests[j].AllTemplateDevicesByIT, itID)
					}
				} else {
					delete(data.Requests[i].AllTemplateDevicesByIT, itID)
				}
			}
		}
	}

	// If template devices were available but all instance types were pruned,
	// no instance type can satisfy this claim without exceeding the limit.
	if allITs.Len() > 0 && prunedCount == allITs.Len() {
		return nil, fmt.Errorf("claim %q: all instance types pruned, no instance type can satisfy this claim without exceeding the maximum of %d devices",
			claim.Name, maxDevices)
	}

	return data, nil
}

func validateExactRequest(
	ctx context.Context,
	kubeClient client.Client,
	claimName string,
	requestName string,
	req *resourcev1.ExactDeviceRequest,
	pools []*Pool,
	templateDevicesByIT map[InstanceTypeID][]DeviceWithID,
	celCache *dracel.Cache,
) (*RequestData, error) {
	rd, err := buildRequestData(ctx, kubeClient, claimName, requestName, exactRequestAccessor{req}, pools, templateDevicesByIT, celCache)
	if err != nil {
		return nil, err
	}
	return rd, nil
}

func validateFirstAvailableRequest(
	ctx context.Context,
	kubeClient client.Client,
	claimName string,
	parentName string,
	subRequests []resourcev1.DeviceSubRequest,
	pools []*Pool,
	templateDevicesByIT map[InstanceTypeID][]DeviceWithID,
	celCache *dracel.Cache,
) (*RequestData, error) {
	parent := &RequestData{Name: RequestName{Parent: parentName}, SubRequests: make([]RequestData, 0, len(subRequests))}
	for i := range subRequests {
		rd, err := buildRequestData(ctx, kubeClient, claimName, subRequests[i].Name, subRequestAccessor{&subRequests[i]}, pools, templateDevicesByIT, celCache)
		if err != nil {
			return nil, err
		}
		rd.Name = RequestName{Parent: parentName, Sub: rd.Name.Parent}

		parent.SubRequests = append(parent.SubRequests, *rd)
	}

	return parent, nil
}

func buildRequestData(
	ctx context.Context,
	kubeClient client.Client,
	claimName string,
	requestName string,
	req deviceRequestAccessor,
	pools []*Pool,
	templateDevicesByIT map[InstanceTypeID][]DeviceWithID,
	celCache *dracel.Cache,
) (*RequestData, error) {
	class := &resourcev1.DeviceClass{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: req.DeviceClassName()}, class); err != nil {
		return nil, fmt.Errorf("claim %q request %q: DeviceClass %q not found: %w", claimName, requestName, req.DeviceClassName(), err)
	}

	// Combine selectors from class and request.
	var selectors []resourcev1.DeviceSelector
	for _, s := range class.Spec.Selectors {
		if s.CEL == nil {
			return nil, fmt.Errorf("claim %q request %q: DeviceClass %q has unsupported selector type (only CEL is supported)", claimName, requestName, class.Name)
		}
		selectors = append(selectors, s)
	}
	for _, s := range req.Selectors() {
		if s.CEL == nil {
			return nil, fmt.Errorf("claim %q request %q: unsupported selector type (only CEL is supported)", claimName, requestName)
		}
		selectors = append(selectors, s)
	}

	// Validate CEL expressions compile.
	for _, s := range selectors {
		result := celCache.GetOrCompile(s.CEL.Expression)
		if result.Error != nil {
			return nil, fmt.Errorf("claim %q request %q: CEL expression %q failed to compile: %w", claimName, requestName, s.CEL.Expression, result.Error)
		}
	}

	rd := &RequestData{
		Name:           RequestName{Parent: requestName},
		Class:          class,
		Selectors:      selectors,
		NumDevices:     int(req.Count()),
		AllocationMode: resourcev1.DeviceAllocationModeExactCount,
	}

	if req.Capacity() != nil {
		rd.CapacityRequests = req.Capacity().Requests
	}

	if req.AllocationMode() == resourcev1.DeviceAllocationModeAll {
		rd.AllocationMode = resourcev1.DeviceAllocationModeAll
		rd.NumDevices = 0
		// Pre-compute eligible in-cluster devices.
		inCluster, err := collectAllModePoolDevices(ctx, selectors, pools, celCache)
		if err != nil {
			return nil, fmt.Errorf("claim %q request %q: %w", claimName, requestName, err)
		}
		rd.AllDevices = inCluster
		// Pre-compute eligible template devices per instance type.
		if len(templateDevicesByIT) > 0 {
			rd.AllTemplateDevicesByIT = make(map[InstanceTypeID][]DeviceWithID, len(templateDevicesByIT))
			for itID, templateDevices := range templateDevicesByIT {
				matched, err := collectAllModeFromDevices(ctx, selectors, templateDevices, celCache)
				if err != nil {
					return nil, fmt.Errorf("claim %q request %q: %w", claimName, requestName, err)
				}
				// Keep the IT in the map if it has matching devices OR if there are
				// in-cluster devices (the IT is still viable with zero template devices).
				// Only prune when both in-cluster and template matches are empty.
				if len(matched) > 0 || len(inCluster) > 0 {
					rd.AllTemplateDevicesByIT[itID] = matched
				}
			}
		}
	}

	return rd, nil
}

// collectAllModePoolDevices finds all devices across all pools that match the selectors.
// Returns an error if any pool is invalid or incomplete, since All mode requires a
// complete and consistent view of available devices.
func collectAllModePoolDevices(
	ctx context.Context,
	selectors []resourcev1.DeviceSelector,
	pools []*Pool,
	celCache *dracel.Cache,
) ([]DeviceWithID, error) {
	var devices []DeviceWithID
	for _, pool := range pools {
		if pool.Invalid {
			return nil, fmt.Errorf("pool %s/%s is invalid (duplicate device names)",
				pool.Key.Driver.Value(), pool.Key.Pool.Value())
		}
		if pool.Incomplete {
			return nil, fmt.Errorf("pool %s/%s is incomplete (missing slices)",
				pool.Key.Driver.Value(), pool.Key.Pool.Value())
		}
		matched, err := collectAllModeFromDevices(ctx, selectors, pool.Devices, celCache)
		if err != nil {
			return nil, err
		}
		devices = append(devices, matched...)
	}
	return devices, nil
}

// collectAllModeFromDevices filters a device list to those matching the selectors.
func collectAllModeFromDevices(
	ctx context.Context,
	selectors []resourcev1.DeviceSelector,
	devices []DeviceWithID,
	celCache *dracel.Cache,
) ([]DeviceWithID, error) {
	var matched []DeviceWithID
	for _, d := range devices {
		ok, err := DeviceMatchesSelectors(ctx, d.Device, d.ID, selectors, celCache)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, d)
		}
	}
	return matched, nil
}

// DeviceMatchesSelectors evaluates whether a device matches all the given selectors.
// All selectors must match (AND semantics).
func DeviceMatchesSelectors(
	ctx context.Context,
	device cloudprovider.Device,
	deviceID DeviceID,
	selectors []resourcev1.DeviceSelector,
	celCache *dracel.Cache,
) (bool, error) {
	for _, s := range selectors {
		if s.CEL == nil {
			continue
		}
		result := celCache.GetOrCompile(s.CEL.Expression)
		if result.Error != nil {
			return false, fmt.Errorf("CEL expression %q failed to compile: %w", s.CEL.Expression, result.Error)
		}

		match, _, err := result.DeviceMatches(ctx, dracel.Device{
			Driver:                   deviceID.Driver.Value(),
			Attributes:               device.Attributes,
			Capacity:                 device.Capacity,
			AllowMultipleAllocations: lo.ToPtr(device.AllowMultipleAllocations),
		})
		if err != nil {
			return false, fmt.Errorf("CEL expression %q evaluation failed: %w", s.CEL.Expression, err)
		}
		if !match {
			return false, nil
		}
	}
	return true, nil
}
