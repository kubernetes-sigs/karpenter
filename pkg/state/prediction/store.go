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

package prediction

import (
	"context"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Prediction holds the predicted resource requests for all containers of a workload.
// Maps container name to its predicted resource requests.
type Prediction struct {
	Containers map[string]corev1.ResourceList
}

// targetEntry pairs a prediction with metadata about its source for tie-breaking.
type targetEntry struct {
	prediction *Prediction
	source     types.NamespacedName
	createdAt  time.Time
}

// Store is a thread-safe cache of predictions, indexed for O(1) lookup
// by target workload. When multiple sources target the same workload,
// the store uses VPA's tie-breaking semantics (earliest creation time wins,
// then lexicographically smallest name) to determine which prediction is active.
type Store struct {
	sync.RWMutex
	hydrationCh   chan struct{}
	hydrationOnce sync.Once
	// byTarget indexes all contending predictions by the workload they apply to.
	// Entries are sorted by strength (strongest first).
	byTarget map[types.UID][]targetEntry
	// bySource maps the prediction source identity to its TargetKey, for deletion cleanup.
	bySource map[types.NamespacedName]types.UID
}

func NewStore() *Store {
	return &Store{
		hydrationCh: make(chan struct{}),
		byTarget:    make(map[types.UID][]targetEntry),
		bySource:    make(map[types.NamespacedName]types.UID),
	}
}

func (s *Store) MarkHydrated() {
	s.hydrationOnce.Do(func() { close(s.hydrationCh) })
}

func (s *Store) Hydrated(ctx context.Context) bool {
	select {
	case <-s.hydrationCh:
		return true
	case <-ctx.Done():
		return false
	}
}

// Set stores a prediction from the given source for the given target.
// If the source previously targeted a different workload, the old entry is removed.
func (s *Store) Set(source types.NamespacedName, targetUID types.UID, p *Prediction, createdAt time.Time) {
	s.Lock()
	defer s.Unlock()

	if prev, ok := s.bySource[source]; ok && prev != targetUID {
		s.removeEntry(prev, source)
	}

	s.bySource[source] = targetUID

	entries := s.byTarget[targetUID]
	found := false
	for i := range entries {
		if entries[i].source == source {
			entries[i].prediction = p
			entries[i].createdAt = createdAt
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, targetEntry{
			prediction: p,
			source:     source,
			createdAt:  createdAt,
		})
	}
	if len(entries) > 1 {
		sort.Slice(entries, func(i, j int) bool {
			return stronger(entries[i], entries[j])
		})
	}
	s.byTarget[targetUID] = entries
}

// Delete removes the prediction from the given source. If other sources target
// the same workload, the next-strongest is automatically promoted.
func (s *Store) Delete(source types.NamespacedName) {
	s.Lock()
	defer s.Unlock()

	if target, ok := s.bySource[source]; ok {
		s.removeEntry(target, source)
		delete(s.bySource, source)
	}
}

// Get returns the active (strongest) prediction for the given target.
// Callers should not mutate the returned Prediction.
func (s *Store) Get(targetUID types.UID) (*Prediction, bool) {
	s.RLock()
	defer s.RUnlock()

	entries := s.byTarget[targetUID]
	if len(entries) == 0 {
		return nil, false
	}
	return entries[0].prediction, true
}

// removeEntry removes the entry for the given source from the target's list.
// If the list becomes empty, the target key is removed from the map.
func (s *Store) removeEntry(target types.UID, source types.NamespacedName) {
	entries := s.byTarget[target]
	for i := range entries {
		if entries[i].source == source {
			entries = append(entries[:i], entries[i+1:]...)
			break
		}
	}
	if len(entries) == 0 {
		delete(s.byTarget, target)
	} else {
		s.byTarget[target] = entries
	}
}

func stronger(a, b targetEntry) bool {
	if !a.createdAt.Equal(b.createdAt) {
		return a.createdAt.Before(b.createdAt)
	}
	return a.source.String() < b.source.String()
}
