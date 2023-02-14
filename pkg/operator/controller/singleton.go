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
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"k8s.io/client-go/util/workqueue"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/ratelimiter"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/metrics"
)

type SingletonBuilder struct {
	mgr manager.Manager
}

func NewSingletonManagedBy(m manager.Manager) SingletonBuilder {
	return SingletonBuilder{
		mgr: m,
	}
}

func (b SingletonBuilder) Complete(r Reconciler) error {
	return b.mgr.Add(newSingleton(r))
}

type Singleton struct {
	Reconciler
	metrics     *singletonMetrics
	rateLimiter ratelimiter.RateLimiter
}

type singletonMetrics struct {
	reconcileDuration prometheus.Histogram
	reconcileErrors   prometheus.Counter
}

func newSingleton(r Reconciler) *Singleton {
	return &Singleton{
		Reconciler:  r,
		metrics:     newSingletonMetrics(r.Name()),
		rateLimiter: workqueue.DefaultItemBasedRateLimiter(),
	}
}

func newSingletonMetrics(name string) *singletonMetrics {
	metrics := &singletonMetrics{
		reconcileDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Namespace: metrics.Namespace,
				Subsystem: strings.ReplaceAll(name, ".", "_"),
				Name:      "reconcile_time_seconds",
				Help:      "Length of time per reconcile.",
				Buckets:   metrics.DurationBuckets(),
			},
		),
		reconcileErrors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metrics.Namespace,
				Subsystem: strings.ReplaceAll(name, ".", "_"),
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
	logging.FromContext(ctx).Infof("starting controller")
	defer logging.FromContext(ctx).Infof("stopping controller")

	for {
		select {
		case <-time.After(s.reconcile(ctx)):
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *Singleton) reconcile(ctx context.Context) time.Duration {
	measureDuration := metrics.Measure(s.metrics.reconcileDuration)
	res, err := s.Reconcile(ctx, singletonRequest)
	measureDuration() // Observe the length of time between the function creation and now

	switch {
	case err != nil:
		s.metrics.reconcileErrors.Inc()
		logging.FromContext(ctx).Error(err)
		return s.rateLimiter.When(singletonRequest)
	case res.Requeue:
		return s.rateLimiter.When(singletonRequest)
	default:
		s.rateLimiter.Forget(singletonRequest)
		return lo.Ternary(res.RequeueAfter > 0, res.RequeueAfter, time.Duration(0))
	}
}

func (s *Singleton) NeedLeaderElection() bool {
	return true
}
