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
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

// SupportedTargets are the workload kinds a pod can be resolved to through its owners, so the only kinds a prediction
// can apply to. Pods of other workloads, e.g. an Argo Rollout, resolve to their ReplicaSet or to no target.
var SupportedTargets = sets.New(
	schema.GroupKind{Group: "apps", Kind: "Deployment"},
	schema.GroupKind{Group: "apps", Kind: "ReplicaSet"},
	schema.GroupKind{Group: "apps", Kind: "StatefulSet"},
	schema.GroupKind{Group: "apps", Kind: "DaemonSet"},
	schema.GroupKind{Group: "", Kind: "ReplicationController"},
	schema.GroupKind{Group: "batch", Kind: "Job"},
	schema.GroupKind{Group: "batch", Kind: "CronJob"},
)

// TargetKey identifies the workload a prediction applies to by name rather than UID, matching how a VPA targets
// workloads, so a workload that's deleted and recreated with the same name keeps its prediction.
type TargetKey struct {
	schema.GroupKind
	types.NamespacedName
}

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
	byTarget map[TargetKey][]targetEntry
	// bySource maps the prediction source identity to its TargetKey, for deletion cleanup.
	bySource map[types.NamespacedName]TargetKey
}

func NewStore() *Store {
	return &Store{
		hydrationCh: make(chan struct{}),
		byTarget:    make(map[TargetKey][]targetEntry),
		bySource:    make(map[types.NamespacedName]TargetKey),
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

// Reset removes all predictions and reports whether any were active.
func (s *Store) Reset() bool {
	s.Lock()
	defer s.Unlock()
	changed := len(s.byTarget) > 0
	s.byTarget = make(map[TargetKey][]targetEntry)
	s.bySource = make(map[types.NamespacedName]TargetKey)
	return changed
}

func (s *Store) Len() int {
	s.RLock()
	defer s.RUnlock()
	return len(s.byTarget)
}

// Set stores a prediction from the given source for the given target and reports whether the active prediction of any
// target changed. If the source previously targeted a different workload, the old entry is removed.
func (s *Store) Set(source types.NamespacedName, target TargetKey, p *Prediction, createdAt time.Time) bool {
	s.Lock()
	defer s.Unlock()

	changed := false
	if prev, ok := s.bySource[source]; ok && prev != target {
		changed = s.removeEntry(prev, source)
	}
	before, _ := s.active(target)

	s.bySource[source] = target

	entries := s.byTarget[target]
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
	sort.Slice(entries, func(i, j int) bool {
		return stronger(entries[i], entries[j])
	})
	s.byTarget[target] = entries
	after, _ := s.active(target)
	// Compare by value since sources recompute their prediction on every update, even when it's unchanged
	return !equality.Semantic.DeepEqual(before, after) || changed
}

// Delete removes the prediction from the given source and reports whether the target's active prediction changed. If
// other sources target the same workload, the next-strongest is automatically promoted.
func (s *Store) Delete(source types.NamespacedName) bool {
	s.Lock()
	defer s.Unlock()

	target, ok := s.bySource[source]
	if !ok {
		return false
	}
	delete(s.bySource, source)
	return s.removeEntry(target, source)
}

// Get returns the active (strongest) prediction for the given target.
// Callers should not mutate the returned Prediction.
func (s *Store) Get(target TargetKey) (*Prediction, bool) {
	s.RLock()
	defer s.RUnlock()
	return s.active(target)
}

// removeEntry removes the entry for the given source from the target's list and reports whether the target's active
// prediction changed. If the list becomes empty, the target key is removed from the map.
func (s *Store) removeEntry(target TargetKey, source types.NamespacedName) bool {
	before, _ := s.active(target)
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
	after, _ := s.active(target)
	return !equality.Semantic.DeepEqual(before, after)
}

func (s *Store) active(target TargetKey) (*Prediction, bool) {
	entries := s.byTarget[target]
	if len(entries) == 0 {
		return nil, false
	}
	return entries[0].prediction, true
}

func stronger(a, b targetEntry) bool {
	if !a.createdAt.Equal(b.createdAt) {
		return a.createdAt.Before(b.createdAt)
	}
	return a.source.String() < b.source.String()
}
