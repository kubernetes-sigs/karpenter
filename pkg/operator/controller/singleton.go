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

	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/client-go/util/workqueue"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/ratelimiter"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Options struct {
	DisableWaitOnError bool
}

type SingletonBuilder struct {
	mgr     manager.Manager
	name    string
	options Options
}

func NewSingletonManagedBy(m manager.Manager) SingletonBuilder {
	return SingletonBuilder{
		mgr: m,
	}
}

func (b SingletonBuilder) Named(n string) SingletonBuilder {
	b.name = n
	return b
}

func (b SingletonBuilder) WithOptions(o Options) SingletonBuilder {
	b.options = o
	return b
}

func (b SingletonBuilder) Complete(r reconcile.Reconciler) error {
	return b.mgr.Add(newSingleton(r, b.name, b.options))
}

type Singleton struct {
	reconcile.Reconciler

	name        string
	options     Options
	rateLimiter ratelimiter.RateLimiter
}

func newSingleton(r reconcile.Reconciler, name string, opts Options) *Singleton {
	return &Singleton{
		Reconciler:  r,
		name:        name,
		options:     opts,
		rateLimiter: workqueue.DefaultItemBasedRateLimiter(),
	}
}

var singletonRequest = reconcile.Request{}

func (s *Singleton) Start(ctx context.Context) error {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(s.name))
	logging.FromContext(ctx).Infof("Starting Controller")
	defer logging.FromContext(ctx).Infof("Stopping Controller")
	for {
		var waitDuration time.Duration
		res, errs := s.Reconcile(ctx, singletonRequest)

		switch {
		case errs != nil:
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
