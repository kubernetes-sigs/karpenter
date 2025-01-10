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

package fake

import (
	"maps"

	"github.com/awslabs/operatorpkg/option"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type ReservationManagerProvider struct {
	snapshots map[types.UID]*snapshot
	capacity  map[string]int // map[offering name]total capacity
}

type snapshot struct {
	reservations map[string]sets.Set[string] // map[reservation id]set[offering name]
	capacity     map[string]int
}

func NewReservationManagerProvider() *ReservationManagerProvider {
	return &ReservationManagerProvider{
		snapshots: map[types.UID]*snapshot{},
		capacity:  map[string]int{},
	}
}

// SetCapacity sets the total number of instances available for a given reservationID. This value will be decremented
// internally each time an instance is launched for the given reservationID.
func (p *ReservationManagerProvider) SetCapacity(reservationID string, capacity int) {
	p.capacity[reservationID] = capacity
}

// Capacity returns the total number of instances
func (p *ReservationManagerProvider) Capacity(reservationID string) int {
	return p.capacity[reservationID]
}

// create decrements the availability for the given reservationID by one.
func (p *ReservationManagerProvider) create(reservationID string) {
	lo.Must0(p.capacity[reservationID] > 0, "created an instance with an offering with no availability")
	p.capacity[reservationID] -= 1
}

// getSnapshot returns an existing snapshot, if one exists for the given UUID, or creates a new one
func (p *ReservationManagerProvider) getSnapshot(uuid *types.UID) *snapshot {
	if uuid != nil {
		if snapshot, ok := p.snapshots[*uuid]; ok {
			return snapshot
		}
	}
	snapshot := &snapshot{
		reservations: map[string]sets.Set[string]{},
		capacity:     map[string]int{},
	}
	maps.Copy(snapshot.capacity, p.capacity)
	if uuid != nil {
		p.snapshots[*uuid] = snapshot
	}
	return snapshot
}

func (p *ReservationManagerProvider) Reset() {
	*p = *NewReservationManagerProvider()
}

func (p *ReservationManagerProvider) ReservationManager(reservationID string, opts ...option.Function[cloudprovider.GetInstanceTypeOptions]) cloudprovider.ReservationManager {
	return snapshotAdapter{
		snapshot:      p.getSnapshot(option.Resolve(opts...).AvailabilitySnapshotUUID),
		reservationID: reservationID,
	}
}

type snapshotAdapter struct {
	*snapshot
	reservationID string
}

func (a snapshotAdapter) Reserve(reservationID string) bool {
	if reservations, ok := a.reservations[reservationID]; ok && reservations.Has(a.reservationID) {
		return true
	}
	if a.capacity[a.reservationID] > 0 {
		reservations, ok := a.reservations[reservationID]
		if !ok {
			reservations = sets.New[string]()
			a.reservations[reservationID] = reservations
		}
		reservations.Insert(a.reservationID)
		a.capacity[a.reservationID] -= 1
		return true
	}
	return false
}

func (a snapshotAdapter) Release(reservationID string) {
	if reservations, ok := a.reservations[reservationID]; ok && reservations.Has(a.reservationID) {
		reservations.Delete(a.reservationID)
	}
}
