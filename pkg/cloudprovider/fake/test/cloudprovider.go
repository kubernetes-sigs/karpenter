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
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/utils/functional"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

type CloudProvider struct {
	InstanceTypes            []*cloudprovider.InstanceType
	InstanceTypesForNodePool map[string][]*cloudprovider.InstanceType
	ErrorsForNodePool        map[string]error

	mu sync.RWMutex
	// CreateCalls contains the arguments for every create call that was made since it was cleared
	CreateCalls        []*v1beta1.NodeClaim
	AllowedCreateCalls int
	NextCreateErr      error
	DeleteCalls        []*v1beta1.NodeClaim

	CreatedNodeClaims map[string]*v1beta1.NodeClaim
	Drifted           cloudprovider.DriftReason
}

func NewCloudProvider() *CloudProvider {
	return &CloudProvider{
		AllowedCreateCalls:       math.MaxInt,
		CreatedNodeClaims:        map[string]*v1beta1.NodeClaim{},
		InstanceTypesForNodePool: map[string][]*cloudprovider.InstanceType{},
		ErrorsForNodePool:        map[string]error{},
	}
}

// Reset is for BeforeEach calls in testing to reset the tracking of CreateCalls
func (c *CloudProvider) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CreateCalls = nil
	c.CreatedNodeClaims = map[string]*v1beta1.NodeClaim{}
	c.InstanceTypes = nil
	c.InstanceTypesForNodePool = map[string][]*cloudprovider.InstanceType{}
	c.ErrorsForNodePool = map[string]error{}
	c.AllowedCreateCalls = math.MaxInt
	c.NextCreateErr = nil
	c.DeleteCalls = []*v1beta1.NodeClaim{}
	c.Drifted = "drifted"
}

func (c *CloudProvider) Create(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (*v1beta1.NodeClaim, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.NextCreateErr != nil {
		temp := c.NextCreateErr
		c.NextCreateErr = nil
		return nil, temp
	}

	c.CreateCalls = append(c.CreateCalls, nodeClaim)
	if len(c.CreateCalls) > c.AllowedCreateCalls {
		return &v1beta1.NodeClaim{}, fmt.Errorf("erroring as number of AllowedCreateCalls has been exceeded")
	}
	reqs := scheduling.NewNodeSelectorRequirements(nodeClaim.Spec.Requirements...)
	np := &v1beta1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: nodeClaim.Labels[v1beta1.NodePoolLabelKey]}}
	instanceTypes := lo.Filter(lo.Must(c.GetInstanceTypes(ctx, np)), func(i *cloudprovider.InstanceType, _ int) bool {
		return reqs.Compatible(i.Requirements, scheduling.AllowUndefinedWellKnownLabels) == nil &&
			len(i.Offerings.Compatible(reqs).Available()) > 0 &&
			resources.Fits(nodeClaim.Spec.Resources.Requests, i.Allocatable())
	})
	// Order instance types so that we get the cheapest instance types of the available offerings
	sort.Slice(instanceTypes, func(i, j int) bool {
		iOfferings := instanceTypes[i].Offerings.Available().Compatible(reqs)
		jOfferings := instanceTypes[j].Offerings.Available().Compatible(reqs)
		return iOfferings.Cheapest().Price < jOfferings.Cheapest().Price
	})
	instanceType := instanceTypes[0]
	// Labels
	labels := map[string]string{}
	for key, requirement := range instanceType.Requirements {
		if requirement.Operator() == v1.NodeSelectorOpIn {
			labels[key] = requirement.Values()[0]
		}
	}
	// Find Offering
	for _, o := range instanceType.Offerings.Available() {
		if reqs.Compatible(scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, o.Zone),
			scheduling.NewRequirement(v1beta1.CapacityTypeLabelKey, v1.NodeSelectorOpIn, o.CapacityType),
		), scheduling.AllowUndefinedWellKnownLabels) == nil {
			labels[v1.LabelTopologyZone] = o.Zone
			labels[v1beta1.CapacityTypeLabelKey] = o.CapacityType
			break
		}
	}
	created := &v1beta1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeClaim.Name,
			Labels:      lo.Assign(labels, nodeClaim.Labels),
			Annotations: nodeClaim.Annotations,
		},
		Spec: *nodeClaim.Spec.DeepCopy(),
		Status: v1beta1.NodeClaimStatus{
			ProviderID:  test.RandomProviderID(),
			Capacity:    functional.FilterMap(instanceType.Capacity, func(_ v1.ResourceName, v resource.Quantity) bool { return !resources.IsZero(v) }),
			Allocatable: functional.FilterMap(instanceType.Allocatable(), func(_ v1.ResourceName, v resource.Quantity) bool { return !resources.IsZero(v) }),
		},
	}
	c.CreatedNodeClaims[created.Status.ProviderID] = created
	return created, nil
}

func (c *CloudProvider) Get(_ context.Context, id string) (*v1beta1.NodeClaim, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if nodeClaim, ok := c.CreatedNodeClaims[id]; ok {
		return nodeClaim.DeepCopy(), nil
	}
	return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("no nodeclaim exists with id '%s'", id))
}

func (c *CloudProvider) List(_ context.Context) ([]*v1beta1.NodeClaim, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return lo.Map(lo.Values(c.CreatedNodeClaims), func(nc *v1beta1.NodeClaim, _ int) *v1beta1.NodeClaim {
		return nc.DeepCopy()
	}), nil
}

func (c *CloudProvider) GetInstanceTypes(_ context.Context, np *v1beta1.NodePool) ([]*cloudprovider.InstanceType, error) {
	if np != nil {
		if err, ok := c.ErrorsForNodePool[np.Name]; ok {
			return nil, err
		}

		if v, ok := c.InstanceTypesForNodePool[np.Name]; ok {
			return v, nil
		}
	}
	if c.InstanceTypes != nil {
		return c.InstanceTypes, nil
	}
	return []*cloudprovider.InstanceType{
		NewInstanceType(InstanceTypeOptions{
			Name: "default-instance-type",
		}),
		NewInstanceType(InstanceTypeOptions{
			Name: "small-instance-type",
			Resources: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
		}),
		NewInstanceType(InstanceTypeOptions{
			Name: "gpu-vendor-instance-type",
			Resources: map[v1.ResourceName]resource.Quantity{
				ResourceGPUVendorA: resource.MustParse("2"),
			}}),
		NewInstanceType(InstanceTypeOptions{
			Name: "gpu-vendor-b-instance-type",
			Resources: map[v1.ResourceName]resource.Quantity{
				ResourceGPUVendorB: resource.MustParse("2"),
			},
		}),
		NewInstanceType(InstanceTypeOptions{
			Name:             "arm-instance-type",
			Architecture:     "arm64",
			OperatingSystems: sets.New("ios", string(v1.Linux), string(v1.Windows), "darwin"),
			Resources: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("16"),
				v1.ResourceMemory: resource.MustParse("128Gi"),
			},
		}),
		NewInstanceType(InstanceTypeOptions{
			Name: "single-pod-instance-type",
			Resources: map[v1.ResourceName]resource.Quantity{
				v1.ResourcePods: resource.MustParse("1"),
			},
		}),
	}, nil
}

func (c *CloudProvider) Delete(_ context.Context, nc *v1beta1.NodeClaim) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.DeleteCalls = append(c.DeleteCalls, nc)
	if _, ok := c.CreatedNodeClaims[nc.Status.ProviderID]; ok {
		delete(c.CreatedNodeClaims, nc.Status.ProviderID)
		return nil
	}
	return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("no nodeclaim exists with provider id '%s'", nc.Status.ProviderID))
}

func (c *CloudProvider) IsDrifted(context.Context, *v1beta1.NodeClaim) (cloudprovider.DriftReason, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.Drifted, nil
}

// Name returns the CloudProvider implementation name.
func (c *CloudProvider) Name() string {
	return "fake"
}
