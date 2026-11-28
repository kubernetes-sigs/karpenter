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

package registry

import "slices"

// Current is the registry sealed by labels.Register, or nil before Register.
var Current *Registry

// binds records every var labels.Register wrote, with the value it held before, in write order.
var binds []bind

type bind struct {
	ptr      *string
	original string
}

// Bind writes value into ptr and records ptr's previous value so Reset can restore it.
func Bind(ptr *string, value string) {
	binds = append(binds, bind{ptr: ptr, original: *ptr})
	*ptr = value
}

// Reset restores every bound var to the value it held before it was bound and clears Current.
func Reset() {
	for _, b := range slices.Backward(binds) {
		*b.ptr = b.original
	}
	binds = nil
	Current = nil
}

// Snapshot is the process-wide state at the time Save was called.
type Snapshot struct {
	registry *Registry
	binds    []bind
	values   []string
}

// Save captures Current and the current value of every bound var.
func Save() Snapshot {
	s := Snapshot{registry: Current, binds: slices.Clone(binds), values: make([]string, len(binds))}
	for i, b := range binds {
		s.values[i] = *b.ptr
	}
	return s
}

// Restore resets the current state, then reinstates the saved registry and writes the saved values back into the
// vars that were bound when Save was called.
func (s Snapshot) Restore() {
	Reset()
	Current = s.registry
	binds = s.binds
	for i, b := range binds {
		*b.ptr = s.values[i]
	}
}
