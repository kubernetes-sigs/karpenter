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

package nodepoolbackoff

import (
	"math/rand"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	// driftBackoffBaseDelay is the first back-off window applied to a NodePool after its
	// first unrecoverable drift replacement failure. Subsequent failures grow the window
	// exponentially up to driftBackoffMaxDelay.
	driftBackoffBaseDelay = 1 * time.Minute
	// driftBackoffMaxDelay is the absolute ceiling on the (pre-jitter) back-off window.
	driftBackoffMaxDelay = 10 * time.Minute
)

// State tracks per-NodePool drift replacement back-off.
//
// A NodePool with persistently failing drift replacements is skipped during drift candidate
// selection while it is backed off, so it stops monopolizing the single per-pass drift command
// and stops burning wasted launch attempts. Once the window elapses the pool becomes eligible
// again; a successful replacement resets it and a failure grows the window exponentially (with
// jitter, capped at driftBackoffMaxDelay). See designs/drift-per-nodepool-backoff.md for details.
type State struct {
	sync.Mutex
	clock    clock.Clock
	rand     *rand.Rand
	base     time.Duration
	max      time.Duration
	maxLevel int
	state    map[types.UID]*backoffEntry
}

type backoffEntry struct {
	level int
	until time.Time
}

// Option configures State.
type Option func(*State)

// WithRand injects a deterministic random source so that jitter is reproducible in tests.
func WithRand(r *rand.Rand) Option {
	return func(s *State) {
		s.rand = r
	}
}

// WithDelays overrides the base and max back-off windows. Intended for tests.
func WithDelays(base, max time.Duration) Option {
	return func(s *State) {
		s.base = base
		s.max = max
	}
}

// NewState constructs per-NodePool drift back-off state.
func NewState(clk clock.Clock, opts ...Option) *State {
	s := &State{
		clock: clk,
		rand:  rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // jitter does not need a cryptographic source
		base:  driftBackoffBaseDelay,
		max:   driftBackoffMaxDelay,
		state: map[types.UID]*backoffEntry{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.maxLevel = saturationLevel(s.base, s.max)
	return s
}

// Fail records an unrecoverable drift replacement failure for a NodePool and arms or escalates
// its back-off window. It returns true when the state changed and false when the pool was already
// backed off.
func (s *State) Fail(nodePool *v1.NodePool) bool {
	s.Lock()
	defer s.Unlock()

	now := s.clock.Now()
	e, ok := s.state[nodePool.UID]
	if !ok {
		e = &backoffEntry{}
		s.state[nodePool.UID] = e
	}
	if e.level > 0 && now.Before(e.until) {
		return false
	}
	e.level = min(e.level+1, s.maxLevel)
	e.until = now.Add(s.jitteredWindow(e.level))
	return true
}

// Reset returns a NodePool to healthy after a successful drift replacement.
func (s *State) Reset(nodePool *v1.NodePool) {
	s.Lock()
	defer s.Unlock()
	delete(s.state, nodePool.UID)
}

// IsBackedOff reports whether a NodePool is currently backed off and should be skipped during
// drift candidate selection.
func (s *State) IsBackedOff(nodePool *v1.NodePool) bool {
	s.Lock()
	defer s.Unlock()
	e, ok := s.state[nodePool.UID]
	if !ok {
		return false
	}
	return e.level > 0 && s.clock.Now().Before(e.until)
}

// Snapshot returns the current back-off level and window expiry for a NodePool. level == 0 means
// the pool is healthy. Used for observability and tests.
func (s *State) Snapshot(nodePool *v1.NodePool) (level int, until time.Time) {
	s.Lock()
	defer s.Unlock()
	if e, ok := s.state[nodePool.UID]; ok {
		return e.level, e.until
	}
	return 0, time.Time{}
}

// Remaining returns the time remaining in the current back-off window, or zero when the NodePool
// is eligible for drift disruption.
func (s *State) Remaining(nodePool *v1.NodePool) time.Duration {
	s.Lock()
	defer s.Unlock()
	e, ok := s.state[nodePool.UID]
	if !ok {
		return 0
	}
	return max(e.until.Sub(s.clock.Now()), 0)
}

// window returns the exponential back-off window for a given level, clamped to max. It is
// overflow-safe: once the shift would exceed max (or wrap), it returns max.
func (s *State) window(level int) time.Duration {
	shift := level - 1
	if shift < 0 {
		shift = 0
	}
	if shift >= 63 {
		return s.max
	}
	scaled := s.base << shift
	if scaled <= 0 || scaled >= s.max {
		return s.max
	}
	return scaled
}

// jitteredWindow applies equal jitter to the window for a level: the result is uniformly
// distributed in [w/2, w), keeping a floor of half the window while de-synchronizing pools
// that fail at the same instant.
func (s *State) jitteredWindow(level int) time.Duration {
	w := s.window(level)
	half := w / 2
	if half <= 0 {
		return w
	}
	return half + time.Duration(s.rand.Int63n(int64(half)))
}

// saturationLevel returns the smallest level (>=1) whose pre-jitter window reaches max, so that
// level stops growing once it saturates.
func saturationLevel(base, max time.Duration) int {
	level := 1
	for base > 0 && level < 63 {
		if base<<(level-1) >= max {
			break
		}
		level++
	}
	return level
}
