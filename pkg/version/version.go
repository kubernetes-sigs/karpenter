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

package version

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/kubernetes"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type Provider struct {
	kubernetes kubernetes.Interface
	version    atomic.Pointer[version.Version]
}

func NewProvider(kubernetes kubernetes.Interface) *Provider {
	return &Provider{
		kubernetes: kubernetes,
	}
}

func (p *Provider) Version() *version.Version {
	if v := p.version.Load(); v != nil {
		return v
	}
	panic("no version discovered")
}

func (p *Provider) Update(ctx context.Context) error {
	info, err := p.kubernetes.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("discovering server info, %w", err)
	}
	v, err := version.ParseMajorMinor(fmt.Sprintf("%s.%s", info.Major, info.Minor))
	if err != nil {
		return fmt.Errorf("parsing version, %w", err)
	}
	newVersion := v
	if oldVersion := p.version.Swap(v); oldVersion != nil && !oldVersion.EqualTo(newVersion) {
		log.FromContext(ctx).WithValues("version", newVersion.String()).Info("discovered kubernetes version")
	}
	return nil
}

type Controller struct {
	provider *Provider
}

func NewController(provider *Provider) *Controller {
	return &Controller{
		provider: provider,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	if err := c.provider.Update(ctx); err != nil {
		return reconciler.Result{}, fmt.Errorf("updating kubernetes version info, %w", err)
	}
	return reconciler.Result{RequeueAfter: time.Minute}, nil
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("version").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
