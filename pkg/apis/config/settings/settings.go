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
	"encoding/json"
	"fmt"
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/configmap"

	"github.com/aws/karpenter-core/pkg/apis/config"
)

var ContextKey = Registration

var Registration = &config.Registration{
	ConfigMapName: "karpenter-global-settings",
	Constructor:   NewSettingsFromConfigMap,
	DefaultData:   lo.Must(defaultSettings.Data()),
}

var defaultSettings = Settings{
	BatchMaxDuration:  metav1.Duration{Duration: time.Second * 10},
	BatchIdleDuration: metav1.Duration{Duration: time.Second * 1},
}

type Settings struct {
	BatchMaxDuration  metav1.Duration `json:"batchMaxDuration"`
	BatchIdleDuration metav1.Duration `json:"batchIdleDuration"`
}

func (s Settings) Data() (map[string]string, error) {
	d := map[string]string{}

	if err := json.Unmarshal(lo.Must(json.Marshal(defaultSettings)), &d); err != nil {
		return d, fmt.Errorf("unmarshalling json data, %w", err)
	}
	return d, nil
}

// NewSettingsFromConfigMap creates a Settings from the supplied ConfigMap
func NewSettingsFromConfigMap(cm *v1.ConfigMap) (Settings, error) {
	s := defaultSettings

	if err := configmap.Parse(cm.Data,
		AsPositiveMetaDuration("batchMaxDuration", &s.BatchMaxDuration),
		AsPositiveMetaDuration("batchIdleDuration", &s.BatchIdleDuration),
	); err != nil {
		// Failing to parse means that there is some error in the Settings, so we should crash
		panic(fmt.Sprintf("parsing config data, %v", err))
	}
	return s, nil
}

// AsPositiveMetaDuration parses the value at key as a time.Duration into the target, if it exists.
func AsPositiveMetaDuration(key string, target *metav1.Duration) configmap.ParseFunc {
	return func(data map[string]string) error {
		if raw, ok := data[key]; ok {
			val, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("failed to parse %q: %w", key, err)
			}
			if val <= 0 {
				return fmt.Errorf("duration value is not positive %q: %q", key, val)
			}
			*target = metav1.Duration{Duration: val}
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
