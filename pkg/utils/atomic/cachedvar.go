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

package atomic

import (
	"sync"
	"time"
)

// CachedVariable is an atomic cached variable. It will store a value for the specified lifetime and allow
// retrieval of the value.
type CachedVariable[T any] struct {
	mu       sync.Mutex
	lifetime time.Duration
	lastSet  time.Time
	value    T
}

// NewCachedVariable creates a new cached variable with a given lifetime.
func NewCachedVariable[T any](lifetime time.Duration) *CachedVariable[T] {
	return &CachedVariable[T]{
		lifetime: lifetime,
	}
}

// Get returns the value if possible and a boolean indicating if the value was available.
func (c *CachedVariable[T]) Get() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.lastSet) > c.lifetime {
		return c.value, false
	}
	return c.value, true
}

// Set sets the value and resets the lifetime.
func (c *CachedVariable[T]) Set(v T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSet = time.Now()
	c.value = v
}

// Reset resets the cached variable so that it no longer stores a value.
func (c *CachedVariable[T]) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSet = time.Time{}
}
