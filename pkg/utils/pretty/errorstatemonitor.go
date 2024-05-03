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

package pretty

import (
	"time"

	"k8s.io/utils/clock"
)

// Error state monitor encapsulates the logic for checking state eventually reaches its target by error Threshold and logs an error every interval we don't see the error.
type ErrorStateMonitor struct {
	goal           interface{}
	errorThreshold time.Duration // amount of time we tolerate errorStateMonitor not reaching goal state
	interval       time.Duration // frequency we want error state monitor to log
	clk            clock.Clock

	lastResolved time.Time // Last time we reached goal state
	lastAlarmed  time.Time
}

func NewErrorStateMonitor(goal interface{}, errorThreshold, interval time.Duration, clk clock.Clock) *ErrorStateMonitor {
	return &ErrorStateMonitor{
		goal:           goal,
		errorThreshold: errorThreshold,
		interval:       interval,
		clk:            clk,
		lastResolved:   clk.Now(),
		lastAlarmed:    time.Time{},
	}
}

// Alarm returns a boolean value on if we need to alarm based on  if our actual state has been out of sync with our goal state for longer than the allowed errorThreshold
func (e *ErrorStateMonitor) Alarm(actual any) bool {
	now := e.clk.Now()
	if actual == e.goal {
		e.lastResolved = now
		// When reaching the goal state, we want all future alarms to fire when reaching errorThreshold
		// so we should reset lastAlarmed here.
		e.lastAlarmed = time.Time{}
		return false
	}
	if now.Sub(e.lastResolved) > e.errorThreshold {
		if e.lastAlarmed.IsZero() || now.Sub(e.lastAlarmed) > e.interval {
			e.lastAlarmed = now
			return true
		}
	}
	return false
}

// LastResolved exposes a read only value of the last time our state passed in met our goal state
func (e *ErrorStateMonitor) LastResolved() time.Time {
	return e.lastResolved
}

// LastAlarmed exposes a read only value of the last time we alarmed based on not matching our goal state
func (e *ErrorStateMonitor) LastAlarmed() time.Time {
	return e.lastAlarmed
}
