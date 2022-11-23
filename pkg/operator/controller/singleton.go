/*
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

package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/client-go/util/workqueue"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/ratelimiter"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/metrics"
)

type WaitUntilFunc func(context.Context)

type Options struct {
	DisableWaitOnError bool
}

type SingletonBuilder struct {
	mgr       manager.Manager
	waitUntil WaitUntilFunc
	options   Options
}

func NewSingletonManagedBy(m manager.Manager) SingletonBuilder {
	return SingletonBuilder{
		mgr: m,
	}
}

// WaitUntil runs the passed WaitFunc prior to each reconcile loop and waits for the function
// to exit before
func (b SingletonBuilder) WaitUntil(waitUntil WaitUntilFunc) SingletonBuilder {
	b.waitUntil = waitUntil
	return b
}

func (b SingletonBuilder) WithOptions(o Options) SingletonBuilder {
	b.options = o
	return b
}

func (b SingletonBuilder) Complete(r Reconciler) error {
	return b.mgr.Add(newSingleton(r, b.waitUntil, b.options))
}

type Singleton struct {
	Reconciler

	metrics *singletonMetrics

	waitUntil   WaitUntilFunc
	options     Options
	rateLimiter ratelimiter.RateLimiter
}

type singletonMetrics struct {
	reconcileDuration prometheus.Histogram
	reconcileErrors   prometheus.Counter
}

func newSingleton(r Reconciler, waitUntil WaitUntilFunc, opts Options) *Singleton {
	return &Singleton{
		Reconciler:  r,
		metrics:     newSingletonMetrics(r.Name()),
		waitUntil:   waitUntil,
		options:     opts,
		rateLimiter: workqueue.DefaultItemBasedRateLimiter(),
	}
}

func newSingletonMetrics(name string) *singletonMetrics {
	metrics := &singletonMetrics{
		reconcileDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Namespace: metrics.Namespace,
				Subsystem: name,
				Name:      "reconcile_time_seconds",
				Help:      "Length of time per reconcile.",
				Buckets:   metrics.DurationBuckets(),
			},
		),
		reconcileErrors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metrics.Namespace,
				Subsystem: name,
				Name:      "reconcile_errors_total",
				Help:      "Total number of reconcile errors.",
			},
		),
	}
	crmetrics.Registry.MustRegister(metrics.reconcileDuration, metrics.reconcileErrors)
	return metrics
}

var singletonRequest = reconcile.Request{}

func (s *Singleton) Start(ctx context.Context) error {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(s.Name()))
	logging.FromContext(ctx).Infof("starting Controller")
	defer logging.FromContext(ctx).Infof("stopping Controller")
	for {
		// This waits until the waitUntil is completed or the context is closed
		// to avoid hanging on the wait when the context gets closed in the middle of the wait
		if s.waitUntil != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-withDoneChan(func() { s.waitUntil(ctx) }):
			}
		}
		measureDuration := metrics.Measure(s.metrics.reconcileDuration)
		res, errs := s.Reconcile(ctx, singletonRequest)
		measureDuration() // Observe the length of time between the function creation and now

		var waitDuration time.Duration
		switch {
		case errs != nil:
			s.metrics.reconcileErrors.Inc()
			for _, err := range multierr.Errors(errs) {
				logging.FromContext(ctx).Error(err)
			}
			if !s.options.DisableWaitOnError {
				waitDuration = s.rateLimiter.When(singletonRequest)
			}
		case res.Requeue:
			waitDuration = s.rateLimiter.When(singletonRequest)
		default:
			waitDuration = lo.Ternary(res.RequeueAfter > 0, res.RequeueAfter, time.Duration(0))
			s.rateLimiter.Forget(singletonRequest)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(waitDuration):
		}
	}
}

func (s *Singleton) NeedLeaderElection() bool {
	return true
}

func withDoneChan(f func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()
	return done
}
