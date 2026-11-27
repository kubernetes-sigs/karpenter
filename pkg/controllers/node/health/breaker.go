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

package health

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// UnhealthyThreshold is the share of a group's nodes, rounded up, that may be unhealthy before node repair stops for
	// the group. Many nodes failing at once usually share a cause that replacing them won't fix.
	UnhealthyThreshold = "20%"
	// BlockedRepeatInterval is how long node repair waits before it repeats an event or a log saying the breaker blocks
	// it.
	BlockedRepeatInterval = 15 * time.Minute
)

// Breaker is one check of node repair's breaker for a group of nodes. A tripped breaker stops repair in the group.
type Breaker struct {
	// Nodes is the number of nodes counted, healthy or not.
	Nodes int
	// Unhealthy is the number of counted nodes that are unhealthy.
	Unhealthy int
	// Tripped is true when more than UnhealthyThreshold of the nodes are unhealthy, which stops repair for the group.
	Tripped bool
}

// LogValues returns the breaker's counts as structured logging key/value pairs.
func (b Breaker) LogValues() []any {
	return []any{"nodes", b.Nodes, "unhealthy-nodes", b.Unhealthy}
}

// CheckBreaker returns the breaker for a group of nodes, given how many nodes it has and how many are unhealthy. It
// trips when more than UnhealthyThreshold of the nodes, rounded up, are unhealthy.
func CheckBreaker(nodes, unhealthy int) Breaker {
	return Breaker{Nodes: nodes, Unhealthy: unhealthy, Tripped: unhealthy > allowedUnhealthy(nodes)}
}

// TrippedAt returns when the first unhealthy node past the threshold became unhealthy, which dates the trip from node
// data, or zero if the breaker isn't tripped. unhealthySince holds when each unhealthy node became unhealthy, and is
// sorted in place.
func TrippedAt(nodes int, unhealthySince []time.Time) time.Time {
	allowed := allowedUnhealthy(nodes)
	if len(unhealthySince) <= allowed {
		return time.Time{}
	}
	slices.SortFunc(unhealthySince, time.Time.Compare)
	return unhealthySince[allowed]
}

// allowedUnhealthy returns how many of the nodes may be unhealthy before the breaker trips. It rounds up, so one
// unhealthy node doesn't stop repair in a small group.
func allowedUnhealthy(nodes int) int {
	threshold := intstr.FromString(UnhealthyThreshold)
	return lo.Must(intstr.GetScaledValueFromIntOrPercent(&threshold, nodes, true))
}

// BreakerLog logs when the breaker blocks node repair. Repair checks the breaker for every node it would repair, on
// every pass, so it logs each NodePool at most once every BlockedRepeatInterval.
type BreakerLog struct {
	// mu makes checking and setting an entry one step, so callers at the same time log once.
	mu     sync.Mutex
	logged *cache.Expiring
}

// NewBreakerLog returns a BreakerLog that times its interval with clk.
func NewBreakerLog(clk clock.Clock) *BreakerLog {
	return &BreakerLog{logged: cache.NewExpiringWithClock(clk)}
}

// Blocked logs that the breaker blocks repair for the NodePool. An empty nodePool name means the breaker counted every
// node in the cluster.
func (l *BreakerLog) Blocked(ctx context.Context, nodePool string, breaker Breaker) {
	l.mu.Lock()
	_, logged := l.logged.Get(nodePool)
	if !logged {
		l.logged.Set(nodePool, struct{}{}, BlockedRepeatInterval)
	}
	l.mu.Unlock()
	if logged {
		return
	}
	logger := log.FromContext(ctx)
	if nodePool != "" {
		logger = logger.WithValues("NodePool", klog.KRef("", nodePool))
	}
	logger.WithValues(breaker.LogValues()...).Info("blocking node repair, more than " + UnhealthyThreshold + " of nodes are unhealthy")
}
