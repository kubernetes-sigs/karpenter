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

package injection

import (
	"context"
	"fmt"
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/system"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/operator/options"
)

type resourceKey struct{}

func WithNamespacedName(ctx context.Context, namespacedname types.NamespacedName) context.Context {
	return context.WithValue(ctx, resourceKey{}, namespacedname)
}

func GetNamespacedName(ctx context.Context) types.NamespacedName {
	retval := ctx.Value(resourceKey{})
	if retval == nil {
		return types.NamespacedName{}
	}
	return retval.(types.NamespacedName)
}

type optionsKey struct{}

func WithOptions(ctx context.Context, opts options.Options) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

func GetOptions(ctx context.Context) options.Options {
	retval := ctx.Value(optionsKey{})
	if retval == nil {
		return options.Options{}
	}
	return retval.(options.Options)
}

type configKey struct{}

func WithConfig(ctx context.Context, config *rest.Config) context.Context {
	return context.WithValue(ctx, configKey{}, config)
}

func GetConfig(ctx context.Context) *rest.Config {
	retval := ctx.Value(configKey{})
	if retval == nil {
		return nil
	}
	return retval.(*rest.Config)
}

type controllerNameKeyType struct{}

var controllerNameKey = controllerNameKeyType{}

func WithControllerName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, controllerNameKey, name)
}

func GetControllerName(ctx context.Context) string {
	name := ctx.Value(controllerNameKey)
	if name == nil {
		return ""
	}
	return name.(string)
}

// WithSettingsOrDie injects the settings into the context for all configMaps passed through the registrations
// NOTE: Settings are resolved statically into the global context.Context at startup. This was changed from updating them
// dynamically at runtime due to the necessity of having to build logic around re-queueing to ensure that settings are
// properly reloaded for things like feature gates
func WithSettingsOrDie(ctx context.Context, kubernetesInterface kubernetes.Interface, settings ...settings.Injectable) context.Context {
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	logging.FromContext(ctx).Debugf("waiting for configmaps")
	factory := informers.NewSharedInformerFactoryWithOptions(kubernetesInterface, time.Second*30, informers.WithNamespace(system.Namespace()))
	informer := factory.Core().V1().ConfigMaps().Informer()
	factory.Start(cancelCtx.Done())

	for _, setting := range settings {
		cm := lo.Must(waitForConfigMap(ctx, setting.ConfigMap(), informer))
		ctx = lo.Must(setting.Inject(ctx, cm))
	}
	return ctx
}

// waitForConfigMap waits until all registered configMaps in the settingsStore are created
func waitForConfigMap(ctx context.Context, name string, informer cache.SharedIndexInformer) (*v1.ConfigMap, error) {
	for {
		configMap, exists, err := informer.GetStore().GetByKey(types.NamespacedName{Namespace: system.Namespace(), Name: name}.String())
		if configMap != nil && exists && err == nil {
			return configMap.(*v1.ConfigMap), nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context canceled")
		case <-time.After(time.Millisecond * 500):
		}
	}
}
