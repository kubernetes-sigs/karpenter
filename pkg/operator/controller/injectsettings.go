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

	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/operator/settingsstore"
)

type injectSettingsDecorator struct {
	controller    Controller
	settingsStore settingsstore.Store
}

// InjectSettings wraps a Controller to inject the global settings config as an in-memory object into
// the Reconcile context. This allows pulling settings out of the context by deserialization with functions
// like settings.FromContext(ctx)
func InjectSettings(controller Controller, ss settingsstore.Store) Controller {
	return &injectSettingsDecorator{
		controller:    controller,
		settingsStore: ss,
	}
}

func (sd *injectSettingsDecorator) Name() string {
	return sd.controller.Name()
}

func (sd *injectSettingsDecorator) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = sd.settingsStore.InjectSettings(ctx)
	return sd.controller.Reconcile(ctx, req)
}

func (sd *injectSettingsDecorator) Builder(ctx context.Context, mgr manager.Manager) Builder {
	return sd.controller.Builder(ctx, mgr)
}
