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

package controller_test

import (
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/utils/controller"
)

var _ = Describe("RetryConfig", func() {
	defaults := controller.RetryConfig{
		InitialDelay: time.Second,
		MaxDelay:     time.Minute,
		QPS:          10,
		Burst:        100,
	}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: "nodeclaim"}}

	It("should apply a maximum override while retaining the initial delay and reset backoff after success", func() {
		limiter, err := (controller.RetryConfig{MaxDelay: 5 * time.Second}).NewRateLimiter(defaults)
		Expect(err).NotTo(HaveOccurred())
		for _, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second} {
			Expect(limiter.When(request)).To(Equal(delay))
		}
		other := reconcile.Request{NamespacedName: types.NamespacedName{Name: "other"}}
		Expect(limiter.When(other)).To(Equal(time.Second))
		limiter.Forget(request)
		Expect(limiter.NumRequeues(request)).To(BeZero())
		Expect(limiter.When(request)).To(Equal(time.Second))
	})

	It("should allow overriding the initial delay independently", func() {
		limiter, err := (controller.RetryConfig{InitialDelay: 10 * time.Second}).NewRateLimiter(defaults)
		Expect(err).NotTo(HaveOccurred())
		for _, delay := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute} {
			Expect(limiter.When(request)).To(Equal(delay))
		}
	})

	DescribeTable("should reject invalid retry settings after applying defaults",
		func(config controller.RetryConfig) {
			_, err := config.NewRateLimiter(defaults)
			Expect(err).To(HaveOccurred())
		},
		Entry("negative initial delay", controller.RetryConfig{InitialDelay: -time.Second}),
		Entry("negative maximum delay", controller.RetryConfig{MaxDelay: -time.Second}),
		Entry("maximum below inherited initial delay", controller.RetryConfig{MaxDelay: time.Millisecond}),
		Entry("initial above inherited maximum delay", controller.RetryConfig{InitialDelay: 2 * time.Minute}),
		Entry("negative QPS", controller.RetryConfig{QPS: -1}),
		Entry("NaN QPS", controller.RetryConfig{QPS: math.NaN()}),
		Entry("infinite QPS", controller.RetryConfig{QPS: math.Inf(1)}),
		Entry("negative burst", controller.RetryConfig{Burst: -1}),
	)

	It("should share the configured bucket across resources and honor the longer delay", func() {
		limiter, err := (controller.RetryConfig{
			InitialDelay: time.Second,
			MaxDelay:     time.Minute,
			QPS:          0.001,
			Burst:        2,
		}).NewRateLimiter(controller.RetryConfig{})
		Expect(err).NotTo(HaveOccurred())
		start := time.Now()
		for _, name := range []string{"first", "second"} {
			request := reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
			Expect(limiter.When(request)).To(Equal(time.Second))
		}
		third := reconcile.Request{NamespacedName: types.NamespacedName{Name: "third"}}
		delay := limiter.When(third)
		// Account for elapsed time rather than sleeping or assuming a fixed execution time.
		Expect(delay).To(BeNumerically(">=", max(time.Second, 1000*time.Second-time.Since(start))))
		Expect(delay).To(BeNumerically("<=", 1000*time.Second))
	})
})
