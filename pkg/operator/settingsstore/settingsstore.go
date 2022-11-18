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
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
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

// NewWatcherOrDie creates the settings store watchers to watch for configMap updates to any settings store in registrations
// Before returning, it waits for all ConfigMaps passed through registration to be created
func NewWatcherOrDie(ctx context.Context, kubernetesInterface kubernetes.Interface, cmw *informer.InformedWatcher, registrations ...*config.Registration) Store {
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
		ss.stores[registration].WatchConfigs(cmw)
	}
	// Waits for all the ConfigMaps to be created before we continue onto the
	ss.waitForConfigMapsOrDie(ctx, kubernetesInterface, cmw)
	return ss
}

// waitForConfigMapsOrDie waits until all registered configMaps in the settingsStore are created
func (s *store) waitForConfigMapsOrDie(ctx context.Context, kubernetesInterface kubernetes.Interface, configMapWatcher *informer.InformedWatcher) {
	logging.FromContext(ctx).Debugf("Waiting for settings ConfigMap(s) creation")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	factory := informers.NewSharedInformerFactoryWithOptions(kubernetesInterface, time.Second*30, informers.WithNamespace(configMapWatcher.Namespace))
	configMapInformer := factory.Core().V1().ConfigMaps().Informer()
	factory.Start(ctx.Done())
	expectedNames := sets.NewString(lo.Map(s.registrations, func(r *config.Registration, _ int) string {
		return r.ConfigMapName
	})...)
	for {
		got := configMapInformer.GetStore().List()
		gotNames := sets.NewString(lo.Map(got, func(obj interface{}, _ int) string {
			return obj.(*v1.ConfigMap).Name
		})...)
		if gotNames.IsSuperset(expectedNames) {
			break
		}
		select {
		case <-ctx.Done():
			panic(fmt.Sprintf("Timed out waiting for ConfigMap(s) %v to be created", lo.Keys(expectedNames)))
		case <-time.After(time.Millisecond * 500):
		}
	}
	logging.FromContext(ctx).Debugf("settings ConfigMap(s) exist")
}

func (s *store) InjectSettings(ctx context.Context) context.Context {
	return lo.Reduce(s.registrations, func(c context.Context, registration *config.Registration, _ int) context.Context {
		return context.WithValue(c, registration, s.stores[registration].UntypedLoad(registration.ConfigMapName))
	}, ctx)
}
