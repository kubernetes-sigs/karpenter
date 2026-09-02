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

package disruption

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

const (
	// driftBackoffBaseDelay is the first back-off window applied to a NodePool after its
	// first unrecoverable drift replacement failure. Subsequent failures grow the window
	// exponentially up to driftBackoffMaxDelay.
	driftBackoffBaseDelay = 1 * time.Minute
	// driftBackoffMaxDelay is the absolute ceiling on the (pre-jitter) back-off window.
	driftBackoffMaxDelay = 10 * time.Minute
	// driftBackoffGaugeInterval is how often the singleton controller re-publishes the decaying
	// drift_backoff_seconds gauge. The gauge reports "seconds remaining", which shrinks in
	// real time, so it must be re-published on a cadence rather than only on Fail/Reset.
	driftBackoffGaugeInterval = 5 * time.Second
)

// NodePoolBackoff tracks per-NodePool drift replacement back-off.

// A NodePool with persistently failing drift replacements is skipped during drift candidate
// selection while it is backed off, so it stops monopolizing the single per-pass drift command
// and stops burning wasted launch attempts. Once the window elapses the pool becomes eligible
// again; a successful replacement resets it and a failure grows the window exponentially (with
// jitter, capped at driftBackoffMaxDelay). See designs/drift-per-nodepool-backoff.md for details.
type NodePoolBackoff struct {
	mu       sync.Mutex
	clock    clock.Clock
	rand     *rand.Rand
	base     time.Duration
	max      time.Duration
	maxLevel int
	state    map[string]*backoffEntry // keyed by NodePool name

	// published records the NodePools that currently have a drift_backoff_seconds series so
	// stale series can be deleted once a pool becomes eligible again (reset or window elapsed).
	published map[string]struct{}
}

type backoffEntry struct {
	level int       // number of failed back-off windows (0 == healthy)
	until time.Time // pool is skipped during selection before this time
}

// NodePoolBackoffOption configures a NodePoolBackoff.
type NodePoolBackoffOption func(*NodePoolBackoff)

// WithBackoffRand injects a deterministic random source so that jitter is reproducible in tests.
func WithBackoffRand(r *rand.Rand) NodePoolBackoffOption {
	return func(b *NodePoolBackoff) {
		b.rand = r
	}
}

// WithBackoffDelays overrides the base and max back-off windows. Intended for tests.
func WithBackoffDelays(base, max time.Duration) NodePoolBackoffOption {
	return func(b *NodePoolBackoff) {
		b.base = base
		b.max = max
	}
}

// NewNodePoolBackoff constructs a per-NodePool drift back-off tracker.
func NewNodePoolBackoff(clk clock.Clock, opts ...NodePoolBackoffOption) *NodePoolBackoff {
	b := &NodePoolBackoff{
		clock:     clk,
		rand:      rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec // jitter does not need a cryptographic source
		base:      driftBackoffBaseDelay,
		max:       driftBackoffMaxDelay,
		state:     map[string]*backoffEntry{},
		published: map[string]struct{}{},
	}
	for _, opt := range opts {
		opt(b)
	}
	b.maxLevel = saturationLevel(b.base, b.max)
	return b
}

// Fail records an unrecoverable drift replacement failure for a NodePool and arms (or escalates)
// its back-off window. It is a no-op while the pool is already backed off, so that a burst of
// failures within a single window counts as one failed window rather than many. This keeps level
// counting failed windows, not individual launch attempts.
func (b *NodePoolBackoff) Fail(nodePool string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	e, ok := b.state[nodePool]
	if !ok {
		e = &backoffEntry{}
		b.state[nodePool] = e
	}
	// Idempotent within a window: if the pool is already backed off, don't escalate.
	if e.level > 0 && now.Before(e.until) {
		return
	}
	// level saturates once the window reaches max, so the exponent never overflows.
	e.level = min(e.level+1, b.maxLevel)
	e.until = now.Add(b.jitteredWindow(e.level))

	DriftBackoffsTotal.Inc(map[string]string{metrics.NodePoolLabel: nodePool})
}

// Reset returns a NodePool to healthy after a successful drift replacement.
func (b *NodePoolBackoff) Reset(nodePool string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, nodePool)
}

// IsBackedOff reports whether a NodePool is currently backed off and should be skipped during
// drift candidate selection. It never mutates state.
func (b *NodePoolBackoff) IsBackedOff(nodePool string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.state[nodePool]
	if !ok {
		return false
	}
	return e.level > 0 && b.clock.Now().Before(e.until)
}

// Snapshot returns the current back-off level and window expiry for a NodePool. level == 0 means
// the pool is healthy. Used for observability (events, gauges) and tests.
func (b *NodePoolBackoff) Snapshot(nodePool string) (level int, until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.state[nodePool]; ok {
		return e.level, e.until
	}
	return 0, time.Time{}
}

func (b *NodePoolBackoff) Name() string {
	return "metrics.nodepool.driftbackoff"
}

// Reconcile refreshes the decaying drift_backoff_seconds gauge. The gauge reports "seconds
// remaining" in the current window, a value that shrinks in real time, so re-publishing it
// periodically is what makes it decay between Fail/Reset events. RequeueAfter (wall-clock) is
// independent of the injectable clock used to compute window expiry, so tests can drive a single
// reconcile under a fake clock.
func (b *NodePoolBackoff) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, b.Name()) //nolint:ineffassign,staticcheck
	b.refreshMetrics()
	return reconciler.Result{RequeueAfter: driftBackoffGaugeInterval}, nil
}

func (b *NodePoolBackoff) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(b.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(b))
}

// refreshMetrics recomputes drift_backoff_seconds for every tracked NodePool: it publishes the
// seconds remaining for pools that are currently backed off and deletes the series for pools that
// have become eligible again (reset, window elapsed) so the gauge never reports a stale value.
func (b *NodePoolBackoff) refreshMetrics() {
	now := b.clock.Now()

	b.mu.Lock()
	active := make(map[string]float64, len(b.state))
	for nodePool, e := range b.state {
		if e.level > 0 {
			if remaining := e.until.Sub(now); remaining > 0 {
				active[nodePool] = remaining.Seconds()
			}
		}
	}
	// Series published on a prior refresh that are no longer active must be deleted.
	var stale []string
	for nodePool := range b.published {
		if _, ok := active[nodePool]; !ok {
			stale = append(stale, nodePool)
		}
	}
	published := make(map[string]struct{}, len(active))
	for nodePool := range active {
		published[nodePool] = struct{}{}
	}
	b.published = published
	b.mu.Unlock()

	for nodePool, seconds := range active {
		DriftBackoffSeconds.Set(seconds, map[string]string{metrics.NodePoolLabel: nodePool})
	}
	for _, nodePool := range stale {
		DriftBackoffSeconds.Delete(map[string]string{metrics.NodePoolLabel: nodePool})
	}
}

// window returns the exponential back-off window for a given level, clamped to max. It is
// overflow-safe: once the shift would exceed max (or wrap), it returns max.
func (b *NodePoolBackoff) window(level int) time.Duration {
	shift := level - 1
	if shift < 0 {
		shift = 0
	}
	if shift >= 63 {
		return b.max
	}
	scaled := b.base << shift
	// Detect overflow (negative/zero) or exceeding the ceiling.
	if scaled <= 0 || scaled >= b.max {
		return b.max
	}
	return scaled
}

// jitteredWindow applies equal jitter to the window for a level: the result is uniformly
// distributed in [w/2, w), keeping a floor of half the window while de-synchronizing pools
// that fail at the same instant.
func (b *NodePoolBackoff) jitteredWindow(level int) time.Duration {
	w := b.window(level)
	half := w / 2
	if half <= 0 {
		return w
	}
	return half + time.Duration(b.rand.Int63n(int64(half)))
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
