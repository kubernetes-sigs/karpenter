/*
Copyright 2023 The Kubernetes Authors.

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

package v1beta1_test

import (
	"math"
	"strings"
	"time"

	"github.com/Pallinder/go-randomdata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"

	. "sigs.k8s.io/karpenter/pkg/apis/v1beta1"
)

var _ = Describe("Budgets", func() {
	var nodePool *NodePool
	var budgets []Budget
	var fakeClock *clock.FakeClock

	BeforeEach(func() {
		// Set the time to the middle of the year of 2000, the best year ever
		fakeClock = clock.NewFakeClock(time.Date(2000, time.June, 15, 12, 30, 30, 0, time.UTC))
		budgets = []Budget{
			{
				MaxUnavailable: "10",
				Crontab:        lo.ToPtr("* * * * *"),
				Duration:       lo.ToPtr(metav1.Duration{Duration: lo.Must(time.ParseDuration("1h"))}),
			},
			{
				MaxUnavailable: "100",
				Crontab:        lo.ToPtr("* * * * *"),
				Duration:       lo.ToPtr(metav1.Duration{Duration: lo.Must(time.ParseDuration("1h"))}),
			},
			{
				MaxUnavailable: "10%",
				Crontab:        lo.ToPtr("* * * * *"),
				Duration:       lo.ToPtr(metav1.Duration{Duration: lo.Must(time.ParseDuration("1h"))}),
			},
			{
				MaxUnavailable: "100%",
				Crontab:        lo.ToPtr("* * * * *"),
				Duration:       lo.ToPtr(metav1.Duration{Duration: lo.Must(time.ParseDuration("1h"))}),
			},
		}
		nodePool = &NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: NodePoolSpec{
				Disruption: Disruption{
					Budgets: budgets,
				},
			},
		}
	})
	Context("NodePool/AllowedDisruptions", func() {
		It("should return the min allowedDisruptions", func() {
			min, err := nodePool.GetAllowedDisruptions(ctx, fakeClock, 100)
			Expect(err).To(Succeed())
			Expect(min).To(BeNumerically("==", 10))
		})
		It("should return the min allowedDisruptions, ignoring inactive crons", func() {
			// Make the first and third budgets inactive
			budgets[0].Crontab = lo.ToPtr("@yearly")
			budgets[2].Crontab = lo.ToPtr("@yearly")
			min, err := nodePool.GetAllowedDisruptions(ctx, fakeClock, 100)
			Expect(err).To(Succeed())
			Expect(min).To(BeNumerically("==", 100))
		})
		It("should return MaxInt32 if all crons are inactive", func() {
			budgets[0].Crontab = lo.ToPtr("@yearly")
			budgets[1].Crontab = lo.ToPtr("@yearly")
			budgets[2].Crontab = lo.ToPtr("@yearly")
			budgets[3].Crontab = lo.ToPtr("@yearly")
			min, err := nodePool.GetAllowedDisruptions(ctx, fakeClock, 100)
			Expect(err).To(Succeed())
			Expect(min).To(BeNumerically("==", math.MaxInt32))
		})
		It("should fail and return zero values if a crontab is invalid", func() {
			budgets[0].Crontab = lo.ToPtr("@wrongly")
			min, err := nodePool.GetAllowedDisruptions(ctx, fakeClock, 100)
			Expect(err).ToNot(Succeed())
			Expect(min).To(BeNumerically("==", 0))
		})
	})
	Context("Budget/AllowedDisruptions", func() {
		It("should fail and return zero values if a crontab is invalid", func() {
			budgets[0].Crontab = lo.ToPtr("@wrongly")
			val, err := budgets[0].GetAllowedDisruptions(fakeClock)
			Expect(err).ToNot(Succeed())
			Expect(val.IntVal).To(BeNumerically("==", 0))
		})
		It("should return MaxInt32 when a budget is inactive", func() {
			budgets[0].Crontab = lo.ToPtr("@yearly")
			budgets[0].Duration = lo.ToPtr(metav1.Duration{Duration: lo.Must(time.ParseDuration("1h"))})
			val, err := budgets[0].GetAllowedDisruptions(fakeClock)
			Expect(err).To(Succeed())
			Expect(val.IntVal).To(BeNumerically("==", math.MaxInt32))
		})
		It("should return the int maxUnavailable when a budget is active", func() {
			val, err := budgets[0].GetAllowedDisruptions(fakeClock)
			Expect(err).To(Succeed())
			Expect(val.IntVal).To(BeNumerically("==", 10))
		})
		It("should return the string maxUnavailable when a budget is active", func() {
			val, err := budgets[2].GetAllowedDisruptions(fakeClock)
			Expect(err).To(Succeed())
			Expect(val.StrVal).To(Equal("10%"))
		})
	})
	Context("IsActive", func() {
		It("should return that a crontab is active when crontab and duration are nil", func() {
			budgets[0].Crontab = nil
			budgets[0].Duration = nil
			active, err := budgets[0].IsActive(fakeClock)
			Expect(err).To(Succeed())
			Expect(active).To(BeTrue())
		})
		It("should return that a crontab is active", func() {
			active, err := budgets[0].IsActive(fakeClock)
			Expect(err).To(Succeed())
			Expect(active).To(BeTrue())
		})
		It("should return that a crontab is inactive", func() {
			budgets[0].Crontab = lo.ToPtr("@yearly")
			active, err := budgets[0].IsActive(fakeClock)
			Expect(err).To(Succeed())
			Expect(active).To(BeFalse())
		})
		It("should return that a crontab is active when the crontab hit is in the middle of the duration", func() {
			// Set the date to the start of the year 1000, the best year ever
			fakeClock = clock.NewFakeClock(time.Date(1000, time.January, 1, 12, 0, 0, 0, time.UTC))
			budgets[0].Crontab = lo.ToPtr("@yearly")
			budgets[0].Duration = lo.ToPtr(metav1.Duration{Duration: lo.Must(time.ParseDuration("24h"))})
			active, err := budgets[0].IsActive(fakeClock)
			Expect(err).To(Succeed())
			Expect(active).To(BeTrue())
		})
		It("should return that a crontab is active when the duration is longer than the recurrence", func() {
			// Set the date to the first monday in 2024, the best year ever
			fakeClock = clock.NewFakeClock(time.Date(2024, time.January, 7, 0, 0, 0, 0, time.UTC))
			budgets[0].Crontab = lo.ToPtr("@daily")
			budgets[0].Duration = lo.ToPtr(metav1.Duration{Duration: lo.Must(time.ParseDuration("48h"))})
			active, err := budgets[0].IsActive(fakeClock)
			Expect(err).To(Succeed())
			Expect(active).To(BeTrue())
		})
		It("should return that a crontab is inactive when the crontab hit is after the duration", func() {
			// Set the date to the first monday in 2024, the best year ever
			fakeClock = clock.NewFakeClock(time.Date(2024, time.January, 7, 0, 0, 0, 0, time.UTC))
			budgets[0].Crontab = lo.ToPtr("30 6 * * SUN")
			budgets[0].Duration = lo.ToPtr(metav1.Duration{Duration: lo.Must(time.ParseDuration("6h"))})
			active, err := budgets[0].IsActive(fakeClock)
			Expect(err).To(Succeed())
			Expect(active).ToNot(BeTrue())
		})
	})
})
