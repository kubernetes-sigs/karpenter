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
		_, until := backoff.Snapshot(nodePool)
		fakeClock.SetTime(until.Add(time.Second))
	}

	It("treats a never-failed NodePool as healthy", func() {
		Expect(backoff.IsBackedOff(spark)).To(BeFalse())
		Expect(backoff.Remaining(spark)).To(BeZero())
		level, _ := backoff.Snapshot(spark)
		Expect(level).To(Equal(0))
	})

	It("arms an exponentially-growing, jittered window on consecutive failures", func() {
		for _, expected := range []time.Duration{base, 2 * base, 4 * base, 8 * base} {
			expireWindow(spark)
			Expect(backoff.Fail(spark)).To(BeTrue())
			_, until := backoff.Snapshot(spark)
			window := until.Sub(fakeClock.Now())
			Expect(window).To(BeNumerically(">=", expected/2))
			Expect(window).To(BeNumerically("<", expected))
			Expect(backoff.IsBackedOff(spark)).To(BeTrue())
			Expect(backoff.Remaining(spark)).To(Equal(window))
		}
	})

	It("caps the window at maxDelay and saturates the level", func() {
		var lastLevel int
		for range 12 {
			expireWindow(spark)
			Expect(backoff.Fail(spark)).To(BeTrue())
			lastLevel, _ = backoff.Snapshot(spark)
		}
		Expect(lastLevel).To(Equal(5))
		_, until := backoff.Snapshot(spark)
		window := until.Sub(fakeClock.Now())
		Expect(window).To(BeNumerically(">=", max/2))
		Expect(window).To(BeNumerically("<", max))
	})

	It("is a no-op while the pool is already backed off", func() {
		Expect(backoff.Fail(spark)).To(BeTrue())
		level1, until1 := backoff.Snapshot(spark)
		Expect(level1).To(Equal(1))

		Expect(backoff.Fail(spark)).To(BeFalse())
		Expect(backoff.Fail(spark)).To(BeFalse())
		level2, until2 := backoff.Snapshot(spark)
		Expect(level2).To(Equal(1))
		Expect(until2).To(Equal(until1))
	})

	It("escalates again once the window has elapsed", func() {
		Expect(backoff.Fail(spark)).To(BeTrue())
		level1, _ := backoff.Snapshot(spark)
		Expect(level1).To(Equal(1))

		expireWindow(spark)
		Expect(backoff.IsBackedOff(spark)).To(BeFalse())
		Expect(backoff.Remaining(spark)).To(BeZero())

		Expect(backoff.Fail(spark)).To(BeTrue())
		level2, _ := backoff.Snapshot(spark)
		Expect(level2).To(Equal(2))
	})

	It("returns to healthy on Reset", func() {
		Expect(backoff.Fail(spark)).To(BeTrue())
		Expect(backoff.IsBackedOff(spark)).To(BeTrue())

		backoff.Reset(spark)
		Expect(backoff.IsBackedOff(spark)).To(BeFalse())
		Expect(backoff.Remaining(spark)).To(BeZero())
		level, _ := backoff.Snapshot(spark)
		Expect(level).To(Equal(0))
	})

	It("de-synchronizes pools that fail at the same instant", func() {
		Expect(backoff.Fail(spark)).To(BeTrue())
		Expect(backoff.Fail(ingress)).To(BeTrue())
		_, sparkUntil := backoff.Snapshot(spark)
		_, ingressUntil := backoff.Snapshot(ingress)
		Expect(sparkUntil).ToNot(Equal(ingressUntil))
	})

	It("tracks NodePools independently", func() {
		Expect(backoff.Fail(spark)).To(BeTrue())
		Expect(backoff.IsBackedOff(spark)).To(BeTrue())
		Expect(backoff.IsBackedOff(ingress)).To(BeFalse())
	})

	It("does not apply stale back-off to a recreated NodePool with the same name", func() {
		Expect(backoff.Fail(spark)).To(BeTrue())
		recreated := &v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: spark.Name, UID: types.UID("recreated-spark-uid")}}

		Expect(backoff.IsBackedOff(spark)).To(BeTrue())
		Expect(backoff.IsBackedOff(recreated)).To(BeFalse())
	})
})
