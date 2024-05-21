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

package controller

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/ratelimiter"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	Immediately = 1 * time.Nanosecond
)

type Controller interface {
	Register(context.Context, manager.Manager) error
}

type SingletonBuilder struct {
	mgr  manager.Manager
	name string
}

func NewSingletonManagedBy(m manager.Manager) SingletonBuilder {
	return SingletonBuilder{
		mgr: m,
	}
}

func (b SingletonBuilder) Named(name string) SingletonBuilder {
	b.name = name
	return b
}

func (b SingletonBuilder) Complete(r reconcile.Reconciler) error {
	return b.mgr.Add(newSingleton(r, b.name))
}

type Singleton struct {
	reconcile.Reconciler
	name        string
	rateLimiter ratelimiter.RateLimiter
}

func newSingleton(r reconcile.Reconciler, name string) *Singleton {
	s := &Singleton{
		Reconciler:  r,
		name:        name,
		rateLimiter: workqueue.DefaultItemBasedRateLimiter(),
	}
	s.initMetrics()
	return s
}

// initMetrics is effectively the same metrics initialization function used by controller-runtime
// https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/internal/controller/controller.go
func (s *Singleton) initMetrics() {
	activeWorkers.WithLabelValues(s.name).Set(0)
	reconcileErrors.WithLabelValues(s.name).Add(0)
	reconcileTotal.WithLabelValues(s.name, labelError).Add(0)
	reconcileTotal.WithLabelValues(s.name, labelRequeueAfter).Add(0)
	reconcileTotal.WithLabelValues(s.name, labelRequeue).Add(0)
	reconcileTotal.WithLabelValues(s.name, labelSuccess).Add(0)
	workerCount.WithLabelValues(s.name).Set(float64(1))
}

var singletonRequest = reconcile.Request{}

func (s *Singleton) Start(ctx context.Context) error {
	ctx = injection.WithControllerName(ctx, s.name)
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithName(s.name))
	log.FromContext(ctx).Info("Starting controller")
	defer log.FromContext(ctx).Info("Stopping controller")

	for {
		select {
		case <-time.After(s.reconcile(ctx)):
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *Singleton) reconcile(ctx context.Context) time.Duration {
	activeWorkers.WithLabelValues(s.name).Inc()
	defer activeWorkers.WithLabelValues(s.name).Dec()

	measureDuration := metrics.Measure(reconcileDuration.WithLabelValues(s.name))
	res, err := s.Reconcile(ctx, singletonRequest)
	measureDuration() // Observe the length of time between the function creation and now

	switch {
	case err != nil:
		reconcileErrors.WithLabelValues(s.name).Inc()
		reconcileTotal.WithLabelValues(s.name, labelError).Inc()
		log.FromContext(ctx).Error(err, "Reconciler error")
		return s.rateLimiter.When(singletonRequest)
	case res.Requeue:
		reconcileTotal.WithLabelValues(s.name, labelRequeue).Inc()
		return s.rateLimiter.When(singletonRequest)
	default:
		s.rateLimiter.Forget(singletonRequest)
		switch {
		case res.RequeueAfter > 0:
			reconcileTotal.WithLabelValues(s.name, labelRequeueAfter).Inc()
			return res.RequeueAfter
		default:
			reconcileTotal.WithLabelValues(s.name, labelSuccess).Inc()
			return time.Duration(0)
		}
	}
}

func (s *Singleton) NeedLeaderElection() bool {
	return true
}

func init() {
	mergeMetrics()
}

const (
	labelError        = "error"
	labelRequeueAfter = "requeue_after"
	labelRequeue      = "requeue"
	labelSuccess      = "success"
)

// Metrics below are copied metrics fired by controller-runtime in its /internal package. This is leveraged
// so that we can fire to the same namespace as users expect other controller-runtime metrics to be fired
// https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/internal/controller/metrics/metrics.go
var (
	reconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "controller_runtime_reconcile_total",
		Help: "Total number of reconciliations per controller",
	}, []string{"controller", "result"})
	reconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "controller_runtime_reconcile_time_seconds",
		Help:    "Length of time per reconciliation per controller",
		Buckets: metrics.DurationBuckets(),
	}, []string{"controller"})
	reconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "controller_runtime_reconcile_errors_total",
		Help: "Total number of reconciliation errors per controller",
	}, []string{"controller"})
	workerCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "controller_runtime_max_concurrent_reconciles",
		Help: "Maximum number of concurrent reconciles per controller",
	}, []string{"controller"})
	activeWorkers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "controller_runtime_active_workers",
		Help: "Number of currently used workers per controller",
	}, []string{"controller"})
)

// mergeMetrics merges the singletonMetrics with metrics already registered in the controller-runtime metrics registry
// https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/internal/controller/metrics/metrics.go
// We know that all these metrics should be registered by controller-runtime so we should switch over
func mergeMetrics() {
	err := &prometheus.AlreadyRegisteredError{}
	errors.As(crmetrics.Registry.Register(reconcileTotal), err)
	reconcileTotal = err.ExistingCollector.(*prometheus.CounterVec)
	errors.As(crmetrics.Registry.Register(reconcileDuration), err)
	reconcileDuration = err.ExistingCollector.(*prometheus.HistogramVec)
	errors.As(crmetrics.Registry.Register(reconcileErrors), err)
	reconcileErrors = err.ExistingCollector.(*prometheus.CounterVec)
	errors.As(crmetrics.Registry.Register(workerCount), err)
	workerCount = err.ExistingCollector.(*prometheus.GaugeVec)
	errors.As(crmetrics.Registry.Register(activeWorkers), err)
	activeWorkers = err.ExistingCollector.(*prometheus.GaugeVec)
}
