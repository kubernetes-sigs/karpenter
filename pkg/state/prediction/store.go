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
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TargetKey identifies a workload that a prediction applies to.
type TargetKey struct {
	Namespace string
	Kind      string
	Name      string
}

// Prediction holds the predicted resource requests for all containers of a workload.
// Maps container name to its predicted resource requests.
type Prediction struct {
	Containers map[string]corev1.ResourceList
}

// Store is a thread-safe cache of predictions, indexed for O(1) lookup
// by target workload.
type Store struct {
	mu sync.RWMutex
	// byTarget indexes predictions by the workload they apply to.
	byTarget map[TargetKey]*Prediction
	// bySource maps the prediction source identity to its TargetKey, for deletion cleanup.
	bySource map[types.NamespacedName]TargetKey
}

func NewStore() *Store {
	return &Store{
		byTarget: make(map[TargetKey]*Prediction),
		bySource: make(map[types.NamespacedName]TargetKey),
	}
}

func (s *Store) Set(source types.NamespacedName, target TargetKey, p *Prediction) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if prev, ok := s.bySource[source]; ok && prev != target {
		delete(s.byTarget, prev)
	}

	s.byTarget[target] = p
	s.bySource[source] = target
}

func (s *Store) Delete(source types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if target, ok := s.bySource[source]; ok {
		delete(s.byTarget, target)
		delete(s.bySource, source)
	}
}

func (s *Store) Get(namespace, targetKind, targetName string) (*Prediction, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.byTarget[TargetKey{Namespace: namespace, Kind: targetKind, Name: targetName}]
	return p, ok
}
