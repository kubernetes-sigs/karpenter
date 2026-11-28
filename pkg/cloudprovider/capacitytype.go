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
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/labels"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// CapacityTypeClass is the behavior Karpenter applies to a karpenter.sh/capacity-type value. A cloud provider may
// define more capacity types than there are classes.
type CapacityTypeClass string

const (
	CapacityTypeClassReserved CapacityTypeClass = "reserved"
	CapacityTypeClassSpot     CapacityTypeClass = "spot"
	CapacityTypeClassOnDemand CapacityTypeClass = "on-demand"
)

// CapacityTypeClassPrecedence is the order a launch resolves its class in when a NodeClaim allows more than one.
var CapacityTypeClassPrecedence = []CapacityTypeClass{CapacityTypeClassReserved, CapacityTypeClassSpot, CapacityTypeClassOnDemand}

// CapacityTypeClassResolver translates karpenter.sh/capacity-type values into their class.
type CapacityTypeClassResolver interface {
	// CapacityTypes returns every capacity type the resolver knows.
	CapacityTypes() []string
	// Class returns the class of a capacity type, and false if the capacity type is unknown.
	Class(capacityType string) (CapacityTypeClass, bool)
}

// CapacityTypeClasses is a CapacityTypeClassResolver backed by a map from capacity type to class.
type CapacityTypeClasses map[string]CapacityTypeClass

// DefaultCapacityTypeClasses maps each built-in capacity type to its own class.
var DefaultCapacityTypeClasses = CapacityTypeClasses{
	v1.CapacityTypeReserved: CapacityTypeClassReserved,
	v1.CapacityTypeSpot:     CapacityTypeClassSpot,
	v1.CapacityTypeOnDemand: CapacityTypeClassOnDemand,
}

func (c CapacityTypeClasses) CapacityTypes() []string {
	capacityTypes := lo.Keys(c)
	sort.Strings(capacityTypes)
	return capacityTypes
}

func (c CapacityTypeClasses) Class(capacityType string) (CapacityTypeClass, bool) {
	class, ok := c[capacityType]
	return class, ok
}

// With returns a copy of c that also contains additional.
func (c CapacityTypeClasses) With(additional CapacityTypeClasses) CapacityTypeClasses {
	return lo.Assign(c, additional)
}

type capacityTypeClassState struct {
	resolver     CapacityTypeClassResolver
	byClass      map[CapacityTypeClass][]string
	requirements map[CapacityTypeClass]scheduling.Requirements
}

var capacityTypeClasses atomic.Pointer[capacityTypeClassState]

// SetCapacityTypeClassResolver registers the capacity types a cloud provider supports and their classes. It must be
// called at startup, after labels.Register and before controllers run. The built-in capacity types must keep their own
// class, every additional capacity type must resolve to the on-demand class, and the resolver's capacity types must be
// the karpenter.sh/capacity-type known values registered with labels.Register. A nil resolver, like never setting
// one, selects DefaultCapacityTypeClasses, which is checked against the known values when first used.
func SetCapacityTypeClassResolver(resolver CapacityTypeClassResolver) error {
	if resolver == nil {
		capacityTypeClasses.Store(nil)
		return nil
	}
	state, err := newCapacityTypeClassState(resolver)
	if err != nil {
		return err
	}
	capacityTypeClasses.Store(state)
	return nil
}

func newCapacityTypeClassState(resolver CapacityTypeClassResolver) (*capacityTypeClassState, error) {
	state := &capacityTypeClassState{
		resolver:     resolver,
		byClass:      map[CapacityTypeClass][]string{},
		requirements: map[CapacityTypeClass]scheduling.Requirements{},
	}
	for capacityType, class := range DefaultCapacityTypeClasses {
		if got, ok := resolver.Class(capacityType); !ok || got != class {
			return nil, fmt.Errorf("capacity type %q must resolve to class %q", capacityType, class)
		}
	}
	for _, capacityType := range resolver.CapacityTypes() {
		class, ok := resolver.Class(capacityType)
		if !ok {
			return nil, fmt.Errorf("capacity type %q has no class", capacityType)
		}
		if _, builtIn := DefaultCapacityTypeClasses[capacityType]; !builtIn && class != CapacityTypeClassOnDemand {
			return nil, fmt.Errorf("capacity type %q must resolve to class %q, got %q", capacityType, CapacityTypeClassOnDemand, class)
		}
		state.byClass[class] = append(state.byClass[class], capacityType)
	}
	if known, _ := labels.KnownValues(v1.CapacityTypeLabelKey); !known.Equal(sets.New(resolver.CapacityTypes()...)) {
		return nil, fmt.Errorf("capacity types %v must be the registered %s known values %v", resolver.CapacityTypes(), v1.CapacityTypeLabelKey, sets.List(known))
	}
	for class, capacityTypes := range state.byClass {
		state.requirements[class] = scheduling.NewRequirements(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, capacityTypes...))
	}
	return state, nil
}

// currentCapacityTypeClasses returns the registered resolver's state, or DefaultCapacityTypeClasses' if none is set.
// The default state is built on first use rather than at init, since it reads the label registry, and panics if the
// registered known values aren't the built-in capacity types. The default is checked against the registry once, on
// first use, and not again if the registry changes.
func currentCapacityTypeClasses() *capacityTypeClassState {
	if state := capacityTypeClasses.Load(); state != nil {
		return state
	}
	capacityTypeClasses.CompareAndSwap(nil, lo.Must(newCapacityTypeClassState(DefaultCapacityTypeClasses)))
	return capacityTypeClasses.Load()
}

// CapacityTypeClassOf returns the class of a capacity type, and false if the capacity type isn't registered.
func CapacityTypeClassOf(capacityType string) (CapacityTypeClass, bool) {
	return currentCapacityTypeClasses().resolver.Class(capacityType)
}

// CapacityTypesOf returns the registered capacity types of a class.
func CapacityTypesOf(class CapacityTypeClass) []string {
	return currentCapacityTypeClasses().byClass[class]
}

// CapacityTypeClassRequirements returns a capacity type requirement allowing every registered capacity type of a class.
func CapacityTypeClassRequirements(class CapacityTypeClass) scheduling.Requirements {
	if reqs, ok := currentCapacityTypeClasses().requirements[class]; ok {
		return reqs
	}
	return scheduling.NewRequirements(scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpDoesNotExist))
}

// HasCapacityTypeClass returns whether a capacity type requirement allows any capacity type of the class.
func HasCapacityTypeClass(requirement *scheduling.Requirement, class CapacityTypeClass) bool {
	return lo.SomeBy(CapacityTypesOf(class), requirement.Has)
}
