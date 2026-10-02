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

package nodepoolbackoff_test

import (
	"math/rand"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/state/nodepoolbackoff"
)

var _ = Describe("State", func() {
	const (
		base = time.Minute
		max  = 10 * time.Minute
	)
	var (
		fakeClock *clocktesting.FakeClock
		backoff   *nodepoolbackoff.State
		spark     *v1.NodePool
		ingress   *v1.NodePool
	)

	BeforeEach(func() {
		fakeClock = clocktesting.NewFakeClock(time.Now())
		spark = &v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "spark", UID: types.UID("spark-uid")}}
		ingress = &v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "ingress", UID: types.UID("ingress-uid")}}
		backoff = nodepoolbackoff.NewState(fakeClock,
			nodepoolbackoff.WithDelays(base, max),
			nodepoolbackoff.WithRand(rand.New(rand.NewSource(1))), //nolint:gosec // deterministic test source
		)
	})

	expireWindow := func(nodePool *v1.NodePool) {
		_, until, _ := backoff.GetBackoff(nodePool)
		fakeClock.SetTime(until.Add(time.Second))
	}

	It("treats a never-failed NodePool as healthy", func() {
		Expect(backoff.IsBackedOff(spark)).To(BeFalse())
		Expect(backoff.Remaining(spark)).To(BeZero())
		level, _, backedOff := backoff.GetBackoff(spark)
		Expect(level).To(Equal(0))
		Expect(backedOff).To(BeFalse())
	})

	It("arms an exponentially-growing, jittered window on consecutive failures", func() {
		for _, expected := range []time.Duration{base, 2 * base, 4 * base, 8 * base} {
			expireWindow(spark)
			Expect(backoff.Fail(spark, fakeClock.Now())).To(BeTrue())
			_, until, backedOff := backoff.GetBackoff(spark)
			window := until.Sub(fakeClock.Now())
			Expect(window).To(BeNumerically(">=", expected/2))
			Expect(window).To(BeNumerically("<", expected))
			Expect(backedOff).To(BeTrue())
			Expect(backoff.IsBackedOff(spark)).To(BeTrue())
			Expect(backoff.Remaining(spark)).To(Equal(window))
		}
	})

	It("caps the window at maxDelay and saturates the level", func() {
		var lastLevel int
		for range 12 {
			expireWindow(spark)
			Expect(backoff.Fail(spark, fakeClock.Now())).To(BeTrue())
			lastLevel, _, _ = backoff.GetBackoff(spark)
		}
		Expect(lastLevel).To(Equal(5))
		_, until, _ := backoff.GetBackoff(spark)
		window := until.Sub(fakeClock.Now())
		Expect(window).To(BeNumerically(">=", max/2))
		Expect(window).To(BeNumerically("<", max))
	})

	It("is a no-op while the pool is already backed off", func() {
		Expect(backoff.Fail(spark, fakeClock.Now())).To(BeTrue())
		level1, until1, _ := backoff.GetBackoff(spark)
		Expect(level1).To(Equal(1))

		Expect(backoff.Fail(spark, fakeClock.Now())).To(BeFalse())
		Expect(backoff.Fail(spark, fakeClock.Now())).To(BeFalse())
		level2, until2, _ := backoff.GetBackoff(spark)
		Expect(level2).To(Equal(1))
		Expect(until2).To(Equal(until1))
	})

	It("does not escalate for a delayed failure from the same attempt burst", func() {
		firstAttemptStartedAt := fakeClock.Now()
		delayedAttemptStartedAt := fakeClock.Now()

		fakeClock.Step(time.Second)
		Expect(backoff.Fail(spark, firstAttemptStartedAt)).To(BeTrue())
		_, until1, _ := backoff.GetBackoff(spark)

		fakeClock.SetTime(until1.Add(time.Second))
		Expect(backoff.Fail(spark, delayedAttemptStartedAt)).To(BeFalse())
		level, until2, backedOff := backoff.GetBackoff(spark)
		Expect(level).To(Equal(1))
		Expect(until2).To(Equal(until1))
		Expect(backedOff).To(BeFalse())

		// An attempt started after the prior effective failure belongs to the next retry cycle.
		Expect(backoff.Fail(spark, fakeClock.Now())).To(BeTrue())
		level, _, _ = backoff.GetBackoff(spark)
		Expect(level).To(Equal(2))
	})

	It("escalates again once the window has elapsed", func() {
		Expect(backoff.Fail(spark, fakeClock.Now())).To(BeTrue())
		level1, _, _ := backoff.GetBackoff(spark)
		Expect(level1).To(Equal(1))

		expireWindow(spark)
		Expect(backoff.IsBackedOff(spark)).To(BeFalse())
		Expect(backoff.Remaining(spark)).To(BeZero())

		Expect(backoff.Fail(spark, fakeClock.Now())).To(BeTrue())
		level2, _, _ := backoff.GetBackoff(spark)
		Expect(level2).To(Equal(2))
	})

	It("returns to healthy on Reset", func() {
		Expect(backoff.Fail(spark, fakeClock.Now())).To(BeTrue())
		Expect(backoff.IsBackedOff(spark)).To(BeTrue())

		backoff.Reset(spark)
		Expect(backoff.IsBackedOff(spark)).To(BeFalse())
		Expect(backoff.Remaining(spark)).To(BeZero())
		level, _, _ := backoff.GetBackoff(spark)
		Expect(level).To(Equal(0))
	})

	It("de-synchronizes pools that fail at the same instant", func() {
		Expect(backoff.Fail(spark, fakeClock.Now())).To(BeTrue())
		Expect(backoff.Fail(ingress, fakeClock.Now())).To(BeTrue())
		_, sparkUntil, _ := backoff.GetBackoff(spark)
		_, ingressUntil, _ := backoff.GetBackoff(ingress)
		Expect(sparkUntil).ToNot(Equal(ingressUntil))
	})

	It("tracks NodePools independently", func() {
		Expect(backoff.Fail(spark, fakeClock.Now())).To(BeTrue())
		Expect(backoff.IsBackedOff(spark)).To(BeTrue())
		Expect(backoff.IsBackedOff(ingress)).To(BeFalse())
	})

	It("does not apply stale back-off to a recreated NodePool with the same name", func() {
		Expect(backoff.Fail(spark, fakeClock.Now())).To(BeTrue())
		recreated := &v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: spark.Name, UID: types.UID("recreated-spark-uid")}}

		Expect(backoff.IsBackedOff(spark)).To(BeTrue())
		Expect(backoff.IsBackedOff(recreated)).To(BeFalse())
	})
})
