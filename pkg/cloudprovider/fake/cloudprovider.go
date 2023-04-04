/*
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

package fake

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

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/test"
	"github.com/aws/karpenter-core/pkg/utils/functional"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

type CloudProvider struct {
	InstanceTypes []*cloudprovider.InstanceType

	mu sync.RWMutex
	// CreateCalls contains the arguments for every create call that was made since it was cleared
	CreateCalls        []*v1alpha5.Machine
	AllowedCreateCalls int
	NextCreateErr      error
	DeleteCalls        []*v1alpha5.Machine

	CreatedMachines map[string]*v1alpha5.Machine
	Drifted         bool
}

func NewCloudProvider() *CloudProvider {
	return &CloudProvider{
		AllowedCreateCalls: math.MaxInt,
		CreatedMachines:    map[string]*v1alpha5.Machine{},
	}
}

// Reset is for BeforeEach calls in testing to reset the tracking of CreateCalls
func (c *CloudProvider) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CreateCalls = []*v1alpha5.Machine{}
	c.CreatedMachines = map[string]*v1alpha5.Machine{}
	c.AllowedCreateCalls = math.MaxInt
	c.NextCreateErr = nil
	c.DeleteCalls = []*v1alpha5.Machine{}
	c.Drifted = false
}

func (c *CloudProvider) Create(ctx context.Context, machine *v1alpha5.Machine) (*v1alpha5.Machine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.NextCreateErr != nil {
		temp := c.NextCreateErr
		c.NextCreateErr = nil
		return nil, temp
	}

	c.CreateCalls = append(c.CreateCalls, machine)
	if len(c.CreateCalls) > c.AllowedCreateCalls {
		return &v1alpha5.Machine{}, fmt.Errorf("erroring as number of AllowedCreateCalls has been exceeded")
	}
	reqs := scheduling.NewNodeSelectorRequirements(machine.Spec.Requirements...)
	instanceTypes := lo.Filter(lo.Must(c.GetInstanceTypes(ctx, nil)), func(i *cloudprovider.InstanceType, _ int) bool {
		return reqs.Compatible(i.Requirements) == nil &&
			len(i.Offerings.Requirements(reqs).Available()) > 0 &&
			resources.Fits(machine.Spec.Resources.Requests, i.Allocatable())
	})
	// Order instance types so that we get the cheapest instance types of the available offerings
	sort.Slice(instanceTypes, func(i, j int) bool {
		iOfferings := instanceTypes[i].Offerings.Available().Requirements(reqs)
		jOfferings := instanceTypes[j].Offerings.Available().Requirements(reqs)
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
			scheduling.NewRequirement(v1alpha5.LabelCapacityType, v1.NodeSelectorOpIn, o.CapacityType),
		)) == nil {
			labels[v1.LabelTopologyZone] = o.Zone
			labels[v1alpha5.LabelCapacityType] = o.CapacityType
			break
		}
	}
	name := test.RandomName()
	created := &v1alpha5.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      lo.Assign(labels, machine.Labels),
			Annotations: machine.Annotations,
		},
		Spec: *machine.Spec.DeepCopy(),
		Status: v1alpha5.MachineStatus{
			ProviderID:  test.RandomProviderID(),
			Capacity:    functional.FilterMap(instanceType.Capacity, func(_ v1.ResourceName, v resource.Quantity) bool { return !resources.IsZero(v) }),
			Allocatable: functional.FilterMap(instanceType.Allocatable(), func(_ v1.ResourceName, v resource.Quantity) bool { return !resources.IsZero(v) }),
		},
	}
	c.CreatedMachines[created.Status.ProviderID] = created
	return created, nil
}

func (c *CloudProvider) Get(_ context.Context, id string) (*v1alpha5.Machine, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if machine, ok := c.CreatedMachines[id]; ok {
		return machine.DeepCopy(), nil
	}
	return nil, cloudprovider.NewMachineNotFoundError(fmt.Errorf("no machine exists with id '%s'", id))
}

func (c *CloudProvider) List(_ context.Context) ([]*v1alpha5.Machine, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return lo.Map(lo.Values(c.CreatedMachines), func(m *v1alpha5.Machine, _ int) *v1alpha5.Machine {
		return m.DeepCopy()
	}), nil
}

func (c *CloudProvider) GetInstanceTypes(_ context.Context, _ *v1alpha5.Provisioner) ([]*cloudprovider.InstanceType, error) {
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
			OperatingSystems: sets.NewString("ios", string(v1.Linux), string(v1.Windows), "darwin"),
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

func (c *CloudProvider) Delete(_ context.Context, m *v1alpha5.Machine) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.DeleteCalls = append(c.DeleteCalls, m)
	if _, ok := c.CreatedMachines[m.Status.ProviderID]; ok {
		delete(c.CreatedMachines, m.Status.ProviderID)
		return nil
	}
	return cloudprovider.NewMachineNotFoundError(fmt.Errorf("no machine exists with provider id '%s'", m.Status.ProviderID))
}

func (c *CloudProvider) IsMachineDrifted(context.Context, *v1alpha5.Machine) (bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.Drifted, nil
}

// Name returns the CloudProvider implementation name.
func (c *CloudProvider) Name() string {
	return "fake"
}
