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

package config

import "sync"

// Store is a thread-safe store for the driver configuration.
// It allows both the ConfigMap controller and ResourceSlice controller to access
// the current configuration without creating a circular dependency.
type Store struct {
	sync.Mutex
	config *Config
}

// NewStore creates a new config store
func NewStore() *Store {
	return &Store{}
}

// Get returns the current configuration (may be nil if not yet loaded)
func (s *Store) Get() *Config {
	s.Lock()
	defer s.Unlock()
	return s.config
}

// Set updates the configuration
func (s *Store) Set(cfg *Config) {
	s.Lock()
	defer s.Unlock()
	s.config = cfg
}

// Clear removes the current configuration
func (s *Store) Clear() {
	s.Lock()
	defer s.Unlock()
	s.config = nil
}
