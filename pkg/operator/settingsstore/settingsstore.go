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

package settingsstore

import (
	"context"

	"github.com/samber/lo"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/configmap/informer"
	"knative.dev/pkg/logging"

	"github.com/aws/karpenter-core/pkg/apis/config"
)

type Store interface {
	InjectSettings(context.Context) context.Context
}

type store struct {
	registrations []*config.Registration
	// We store multiple untyped stores, so we can watch the same ConfigMap but convert it in different ways
	// For instance, we have karpenter-global-settings that converts into a cloudprovider-specific config and a global config
	stores map[*config.Registration]*configmap.UntypedStore
}

func WatchSettings(ctx context.Context, cmw *informer.InformedWatcher, registrations ...*config.Registration) Store {
	ss := &store{
		registrations: registrations,
		stores:        map[*config.Registration]*configmap.UntypedStore{},
	}
	for _, registration := range registrations {
		ss.stores[registration] = configmap.NewUntypedStore(
			registration.ConfigMapName,
			logging.FromContext(ctx),
			configmap.Constructors{
				registration.ConfigMapName: registration.Constructor,
			},
		)
		ss.stores[registration].WatchConfigs(cmw)
	}
	return ss
}

func (s *store) InjectSettings(ctx context.Context) context.Context {
	return lo.Reduce(s.registrations, func(c context.Context, registration *config.Registration, _ int) context.Context {
		return context.WithValue(c, registration, s.stores[registration].UntypedLoad(registration.ConfigMapName))
	}, ctx)
}
