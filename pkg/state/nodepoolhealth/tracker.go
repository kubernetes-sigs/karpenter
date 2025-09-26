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

package nodepoolhealth

import (
	"sync"

	"sigs.k8s.io/karpenter/pkg/utils/ringbuffer"
)

const (
	BufferSize     = 4
	ThresholdFalse = 0.5 // 50% of 0s for NodeRegistrationHealthy=False
)

type Status int

const (
	StatusUnknown Status = iota
	StatusHealthy
	StatusUnhealthy
)

type Tracker struct {
	sync.RWMutex
	buffer ringbuffer.Buffer[bool]
}

func NewTracker(capacity int) *Tracker {
	return &Tracker{
		buffer: *ringbuffer.NewBuffer[bool](capacity),
	}
}

func (t *Tracker) Update(success bool) {
	t.Lock()
	defer t.Unlock()

	t.buffer.Insert(success)
}

func (t *Tracker) Reset() {
	t.Lock()
	defer t.Unlock()

	t.buffer.Reset()
}

func (t *Tracker) Status() Status {
	t.Lock()
	defer t.Unlock()

	if t.buffer.Len() == 0 {
		return StatusUnknown
	}
	// Count number of true and false
	unhealthyCount := 0
	for _, value := range t.buffer.GetItems() {
		if !value {
			unhealthyCount++
		}
	}
	// Determine health status based on threshold
	if (float64(unhealthyCount) / float64(t.buffer.Capacity())) >= ThresholdFalse {
		return StatusUnhealthy
	} else {
		return StatusHealthy
	}
}

func (t *Tracker) SetStatus(status Status) {
	switch status {
	case StatusUnknown:
		t.buffer.Reset()
	case StatusHealthy:
		t.Update(true)
	case StatusUnhealthy:
		t.Update(false)
		t.Update(false)
	}
}

type State map[string]*Tracker

func (s State) nodePoolNodeRegistration(nodepool string) *Tracker {
	tracker, exists := s[nodepool]
	if !exists {
		tracker = NewTracker(BufferSize)
		s[nodepool] = tracker
	}
	return tracker
}

func (s State) Status(nodePool string) Status {
	return s.nodePoolNodeRegistration(nodePool).Status()
}

func (s State) Update(nodePool string, launchStatus bool) {
	s.nodePoolNodeRegistration(nodePool).Update(launchStatus)
}

func (s State) SetStatus(nodePool string, status Status) {
	s.nodePoolNodeRegistration(nodePool).SetStatus(status)
}

func (s State) ResetStatus(nodePool string) {
	s.nodePoolNodeRegistration(nodePool).Reset()
}
