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

	"go.uber.org/multierr"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Builder struct {
	mgr           manager.Manager
	name          string
	leaderElected bool
}

func NewSingletonManagedBy(m manager.Manager) Builder {
	return Builder{
		mgr: m,
	}
}

func (b Builder) Named(n string) Builder {
	b.name = n
	return b
}

func (b Builder) LeaderElected() Builder {
	b.leaderElected = true
	return b
}

func (b Builder) Complete(r reconcile.Reconciler) error {
	return b.mgr.Add(newSingleton(r, b.name, b.leaderElected))
}

type Singleton struct {
	reconcile.Reconciler

	name          string
	leaderElected bool
}

func newSingleton(r reconcile.Reconciler, name string, leaderElected bool) *Singleton {
	return &Singleton{
		Reconciler:    r,
		name:          name,
		leaderElected: leaderElected,
	}
}

func (s *Singleton) Start(ctx context.Context) error {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(s.name))
	logging.FromContext(ctx).Infof("Starting Controller")
	defer logging.FromContext(ctx).Infof("Stopping Controller")
	for {
		res, errs := s.Reconcile(ctx, reconcile.Request{})
		if errs != nil {
			for _, err := range multierr.Errors(errs) {
				logging.FromContext(ctx).Error(err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(res.RequeueAfter):
		}
	}
}

func (s *Singleton) NeedLeaderElection() bool {
	return s.leaderElected
}
