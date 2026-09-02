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

package disruption_test

import (
	"math/rand"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	clocktesting "k8s.io/utils/clock/testing"

	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/metrics"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const driftBackoffSecondsMetric = "karpenter_nodepools_drift_backoff_seconds"

var _ = Describe("NodePoolBackoff", func() {
	const (
		base = time.Minute
		max  = 10 * time.Minute
	)
	var (
		fakeClock *clocktesting.FakeClock
		backoff   *disruption.NodePoolBackoff
	)

	BeforeEach(func() {
		fakeClock = clocktesting.NewFakeClock(time.Now())
		// Seed the jitter source so windows are deterministic across runs.
		backoff = disruption.NewNodePoolBackoff(fakeClock,
			disruption.WithBackoffDelays(base, max),
			disruption.WithBackoffRand(rand.New(rand.NewSource(1))), //nolint:gosec // deterministic test source
		)
	})

	// expireWindow advances the clock just past the pool's current back-off window.
	expireWindow := func(nodePool string) {
		_, until := backoff.Snapshot(nodePool)
		fakeClock.SetTime(until.Add(time.Second))
	}

	It("treats a never-failed NodePool as healthy", func() {
		Expect(backoff.IsBackedOff("spark")).To(BeFalse())
		level, _ := backoff.Snapshot("spark")
		Expect(level).To(Equal(0))
	})

	It("arms an exponentially-growing, jittered window on consecutive failures", func() {
		// Each level's window is equal-jittered into [w/2, w), where w = min(base*2^(level-1), max).
		for _, expected := range []time.Duration{base, 2 * base, 4 * base, 8 * base} {
			expireWindow("spark") // ensure we're not inside the previous window (no-op otherwise)
			backoff.Fail("spark")
			_, until := backoff.Snapshot("spark")
			window := until.Sub(fakeClock.Now())
			Expect(window).To(BeNumerically(">=", expected/2))
			Expect(window).To(BeNumerically("<", expected))
			Expect(backoff.IsBackedOff("spark")).To(BeTrue())
		}
	})

	It("caps the window at maxDelay and saturates the level", func() {
		var lastLevel int
		for range 12 {
			expireWindow("spark")
			backoff.Fail("spark")
			lastLevel, _ = backoff.Snapshot("spark")
		}
		// With base=1m, max=10m the window saturates at level 5 (1m→2m→4m→8m→16m capped to 10m).
		Expect(lastLevel).To(Equal(5))
		_, until := backoff.Snapshot("spark")
		window := until.Sub(fakeClock.Now())
		Expect(window).To(BeNumerically(">=", max/2))
		Expect(window).To(BeNumerically("<", max))
	})

	It("is a no-op while the pool is already backed off (counts windows, not attempts)", func() {
		backoff.Fail("spark")
		level1, until1 := backoff.Snapshot("spark")
		Expect(level1).To(Equal(1))

		// A burst of further failures inside the same window must not escalate.
		backoff.Fail("spark")
		backoff.Fail("spark")
		level2, until2 := backoff.Snapshot("spark")
		Expect(level2).To(Equal(1))
		Expect(until2).To(Equal(until1))
	})

	It("escalates again once the window has elapsed", func() {
		backoff.Fail("spark")
		level1, _ := backoff.Snapshot("spark")
		Expect(level1).To(Equal(1))

		expireWindow("spark")
		Expect(backoff.IsBackedOff("spark")).To(BeFalse()) // eligible again while level stays > 0

		backoff.Fail("spark")
		level2, _ := backoff.Snapshot("spark")
		Expect(level2).To(Equal(2))
	})

	It("returns to healthy on Reset", func() {
		backoff.Fail("spark")
		Expect(backoff.IsBackedOff("spark")).To(BeTrue())

		backoff.Reset("spark")
		Expect(backoff.IsBackedOff("spark")).To(BeFalse())
		level, _ := backoff.Snapshot("spark")
		Expect(level).To(Equal(0))
	})

	It("de-synchronizes pools that fail at the same instant", func() {
		// Two pools failing at the same time with the same level should get different windows,
		// so they don't retry in lockstep after a shared event (e.g. an AZ-wide ICE).
		backoff.Fail("spark")
		backoff.Fail("ingress")
		_, sparkUntil := backoff.Snapshot("spark")
		_, ingressUntil := backoff.Snapshot("ingress")
		Expect(sparkUntil).ToNot(Equal(ingressUntil))
	})

	It("tracks NodePools independently", func() {
		backoff.Fail("spark")
		Expect(backoff.IsBackedOff("spark")).To(BeTrue())
		Expect(backoff.IsBackedOff("ingress")).To(BeFalse())
	})

	Context("drift_backoff_seconds gauge", func() {
		const pool = "spark-gauge"

		// Ensure the global series doesn't leak into other specs.
		AfterEach(func() {
			backoff.Reset(pool)
			ExpectSingletonReconciled(ctx, backoff)
		})

		It("publishes the seconds remaining while backed off and decays over time", func() {
			backoff.Fail(pool)
			_, until := backoff.Snapshot(pool)

			result := ExpectSingletonReconciled(ctx, backoff)
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))
			ExpectMetricGaugeValue(disruption.DriftBackoffSeconds, until.Sub(fakeClock.Now()).Seconds(), map[string]string{metrics.NodePoolLabel: pool})

			// Advancing the clock shrinks the reported value on the next reconcile.
			fakeClock.Step(15 * time.Second)
			ExpectSingletonReconciled(ctx, backoff)
			ExpectMetricGaugeValue(disruption.DriftBackoffSeconds, until.Sub(fakeClock.Now()).Seconds(), map[string]string{metrics.NodePoolLabel: pool})
		})

		It("deletes the series once the pool is reset", func() {
			backoff.Fail(pool)
			ExpectSingletonReconciled(ctx, backoff)
			_, ok := FindMetricWithLabelValues(driftBackoffSecondsMetric, map[string]string{metrics.NodePoolLabel: pool})
			Expect(ok).To(BeTrue())

			backoff.Reset(pool)
			ExpectSingletonReconciled(ctx, backoff)
			_, ok = FindMetricWithLabelValues(driftBackoffSecondsMetric, map[string]string{metrics.NodePoolLabel: pool})
			Expect(ok).To(BeFalse())
		})

		It("deletes the series once the back-off window elapses", func() {
			backoff.Fail(pool)
			ExpectSingletonReconciled(ctx, backoff)
			_, ok := FindMetricWithLabelValues(driftBackoffSecondsMetric, map[string]string{metrics.NodePoolLabel: pool})
			Expect(ok).To(BeTrue())

			expireWindow(pool)
			ExpectSingletonReconciled(ctx, backoff)
			_, ok = FindMetricWithLabelValues(driftBackoffSecondsMetric, map[string]string{metrics.NodePoolLabel: pool})
			Expect(ok).To(BeFalse())
		})
	})
})
