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

package prediction_test

import (
	"context"
	"sync"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	prometheusmodel "github.com/prometheus/client_model/go"

	"sigs.k8s.io/karpenter/pkg/state/prediction"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Hydrated Metric", func() {
	var store *prediction.Store

	BeforeEach(func() {
		// The gauge is process-wide, so clear what earlier specs, e.g. the store's panic spec, left behind
		prediction.StoreHydrated.Reset()
		store = prediction.NewStore(nil)
	})

	It("should not export the series before anything waits on or completes hydration", func() {
		Expect(hydratedGaugeValue()).To(BeNil())
	})
	It("should report 0 while waiting on an unhydrated store", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		Expect(store.Hydrated(ctx)).To(BeFalse())
		ExpectMetricGaugeValue(prediction.StoreHydrated, 0, nil)
	})
	It("should report 1 once a waiting store hydrates", func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan bool, 1)
		var wg sync.WaitGroup
		wg.Go(func() { done <- store.Hydrated(ctx) })
		DeferCleanup(func() { cancel(); wg.Wait() })
		Eventually(hydratedGaugeValue).Should(HaveValue(BeEquivalentTo(0)))
		store.MarkHydrated()
		Eventually(done).Should(Receive(BeTrue()))
		ExpectMetricGaugeValue(prediction.StoreHydrated, 1, nil)
	})
	It("should report 1 when the store hydrates before anything waits, and keep it after a wait", func() {
		store.MarkHydrated()
		ExpectMetricGaugeValue(prediction.StoreHydrated, 1, nil)
		Expect(store.Hydrated(context.Background())).To(BeTrue())
		ExpectMetricGaugeValue(prediction.StoreHydrated, 1, nil)
	})
	It("should keep reporting 1 when the store is marked hydrated again", func() {
		store.MarkHydrated()
		store.MarkHydrated()
		ExpectMetricGaugeValue(prediction.StoreHydrated, 1, nil)
		Expect(store.Hydrated(context.Background())).To(BeTrue())
	})
	It("should keep reporting 1 when a new store is created", func() {
		store.MarkHydrated()
		prediction.NewStore(nil)
		ExpectMetricGaugeValue(prediction.StoreHydrated, 1, nil)
	})
	It("should report 1 to every waiter and end at 1 when waiting races with hydration", func() {
		// Polling with a done ctx returns the moment the channel closes, so it catches a gauge set after the close
		polling, cancel := context.WithCancel(context.Background())
		cancel()
		const iterations, waiters = 500, 8
		for range iterations {
			prediction.StoreHydrated.Reset()
			s := prediction.NewStore(nil)
			start := make(chan struct{})
			seen := make([]*float64, waiters)
			var wg sync.WaitGroup
			for i := range waiters {
				wg.Go(func() {
					<-start
					if i%2 == 0 {
						for !s.Hydrated(polling) {
						}
					} else {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						if !s.Hydrated(ctx) {
							return
						}
					}
					seen[i] = hydratedGaugeValue()
				})
			}
			wg.Go(s.MarkHydrated)
			close(start)
			wg.Wait()
			Expect(seen).To(HaveEach(HaveValue(BeEquivalentTo(1))), "every waiter should read 1 once Hydrated returns true")
			ExpectMetricGaugeValue(prediction.StoreHydrated, 1, nil)
		}
	})
})

// hydratedGaugeValue returns the gauge's value, or nil when the series is absent. It doesn't create the series or
// assert, so it's safe off the spec goroutine.
func hydratedGaugeValue() *float64 {
	ch := make(chan prometheus.Metric, 1)
	prediction.StoreHydrated.(*opmetrics.PrometheusGauge).Collect(ch)
	close(ch)
	metric, ok := <-ch
	if !ok {
		return nil
	}
	m := &prometheusmodel.Metric{}
	if err := metric.Write(m); err != nil {
		return nil
	}
	return new(m.GetGauge().GetValue())
}
