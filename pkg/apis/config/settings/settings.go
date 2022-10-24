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

package settings

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/configmap"

	"github.com/aws/karpenter-core/pkg/apis/config"
)

var ContextKey = Registration

var Registration = &config.Registration{
	ConfigMapName: "karpenter-global-settings",
	Constructor:   NewSettingsFromConfigMap,
}

var defaultSettings = Settings{
	BatchMaxDuration:  time.Second * 10,
	BatchIdleDuration: time.Second * 1,
}

type Settings struct {
	BatchMaxDuration  time.Duration
	BatchIdleDuration time.Duration
}

// NewSettingsFromConfigMap creates a Settings from the supplied ConfigMap
func NewSettingsFromConfigMap(cm *v1.ConfigMap) (Settings, error) {
	s := defaultSettings

	if err := configmap.Parse(cm.Data,
		AsPositiveDuration("batchMaxDuration", &s.BatchMaxDuration),
		AsPositiveDuration("batchIdleDuration", &s.BatchIdleDuration),
	); err != nil {
		// Failing to parse means that there is some error in the Settings, so we should crash
		panic(fmt.Sprintf("parsing config data, %v", err))
	}
	return s, nil
}

// AsPositiveDuration parses the value at key as a time.Duration into the target, if it exists.
func AsPositiveDuration(key string, target *time.Duration) configmap.ParseFunc {
	return func(data map[string]string) error {
		if raw, ok := data[key]; ok {
			val, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("failed to parse %q: %w", key, err)
			}
			if val <= 0 {
				return fmt.Errorf("duration value is not positive %q: %q", key, val)
			}
			*target = val
		}
		return nil
	}
}

func ToContext(ctx context.Context, s Settings) context.Context {
	return context.WithValue(ctx, ContextKey, s)
}

func FromContext(ctx context.Context) Settings {
	data := ctx.Value(ContextKey)
	if data == nil {
		// This is developer error if this happens, so we should panic
		panic("settings doesn't exist in context")
	}
	return data.(Settings)
}
