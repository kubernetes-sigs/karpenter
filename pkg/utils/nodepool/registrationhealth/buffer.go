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

package registrationhealth

import "sync"

const (
	BufferSize     = 4
	ThresholdFalse = 0.5 // 50% of 0s for NodeRegistrationHealthy=False
)

type NodeRegistrationBuffer struct {
	mu           sync.RWMutex
	values       []bool
	currentIndex int
}

func New() *NodeRegistrationBuffer {
	return &NodeRegistrationBuffer{
		values:       make([]bool, 0, BufferSize),
		currentIndex: 0,
	}
}

func (b *NodeRegistrationBuffer) AddHealth(value bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// If buffer is not full, append the new value
	if len(b.values) < BufferSize {
		b.values = append(b.values, value)
	} else {
		// If buffer is full, replace the oldest entry
		b.values[b.currentIndex] = value
		b.currentIndex = (b.currentIndex + 1) % BufferSize
	}
}

func (b *NodeRegistrationBuffer) IsHealthy() int {
	if len(b.values) == 0 {
		// health unknown
		return 0
	}

	// Count number of true and false
	unhealthyCount := 0
	for _, value := range b.values {
		if !value {
			unhealthyCount++
		}
	}

	// Determine health status based on thresholds
	if (float64(unhealthyCount) / float64(BufferSize)) >= ThresholdFalse {
		// unhealthy
		return -1
	} else {
		// healthy
		return 1
	}
}

func (b *NodeRegistrationBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.values = b.values[:0]
	b.currentIndex = 0
}
