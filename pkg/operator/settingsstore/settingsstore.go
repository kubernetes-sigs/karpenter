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
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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

func WatchSettingsOrDie(ctx context.Context, clientSet *kubernetes.Clientset, cmw *informer.InformedWatcher, registrations ...*config.Registration) Store {
	ss := &store{
		registrations: registrations,
		stores:        map[*config.Registration]*configmap.UntypedStore{},
	}
	for _, registration := range registrations {
		if err := registration.Validate(); err != nil {
			panic(fmt.Sprintf("Validating settings registration, %v", err))
		}

		ss.stores[registration] = configmap.NewUntypedStore(
			registration.ConfigMapName,
			logging.FromContext(ctx),
			configmap.Constructors{
				registration.ConfigMapName: registration.Constructor,
			},
		)

		// TODO: Remove this Get once we don't rely on this settingsStore for initialization
		// Attempt to get the ConfigMap since WatchWithDefault doesn't wait for Add event form API-server
		cm, err := clientSet.CoreV1().ConfigMaps(cmw.Namespace).Get(ctx, registration.ConfigMapName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				cm = &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      registration.ConfigMapName,
						Namespace: cmw.Namespace,
					},
					Data: registration.DefaultData,
				}
			} else {
				panic(fmt.Sprintf("Getting settings %v, %v", registration.ConfigMapName, err))
			}
		}

		// TODO: Move this to ss.stores[registration].WatchConfigs(cmw) when the UntypedStores
		// implements a default mechanism
		cmw.WatchWithDefault(*cm, ss.stores[registration].OnConfigChanged)
	}
	return ss
}

func (s *store) InjectSettings(ctx context.Context) context.Context {
	return lo.Reduce(s.registrations, func(c context.Context, registration *config.Registration, _ int) context.Context {
		return context.WithValue(c, registration, s.stores[registration].UntypedLoad(registration.ConfigMapName))
	}, ctx)
}
