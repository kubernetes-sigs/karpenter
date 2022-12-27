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

	"github.com/go-playground/validator/v10"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/configmap"

	"github.com/aws/karpenter-core/pkg/apis/config"
)

var ContextKey = Registration

var Registration = &config.Registration{
	ConfigMapName: "karpenter-global-settings",
	Constructor:   NewSettingsFromConfigMap,
}

var defaultSettings = Settings{
	BatchMaxDuration:  metav1.Duration{Duration: time.Second * 10},
	BatchIdleDuration: metav1.Duration{Duration: time.Second * 1},
	DriftEnabled:      false,
}

type Settings struct {
	BatchMaxDuration  metav1.Duration
	BatchIdleDuration metav1.Duration
	// This feature flag is temporary and will be removed in the near future.
	DriftEnabled bool
	// This feature flag is temporary, for development, and will be removed in the near future
	MachineEnabled bool
}

// NewSettingsFromConfigMap creates a Settings from the supplied ConfigMap
func NewSettingsFromConfigMap(cm *v1.ConfigMap) (Settings, error) {
	s := defaultSettings

	if err := configmap.Parse(cm.Data,
		AsMetaDuration("batchMaxDuration", &s.BatchMaxDuration),
		AsMetaDuration("batchIdleDuration", &s.BatchIdleDuration),
		configmap.AsBool("featureGates.driftEnabled", &s.DriftEnabled),
		configmap.AsBool("featureGates.machineEnabled", &s.MachineEnabled),
	); err != nil {
		// Failing to parse means that there is some error in the Settings, so we should crash
		panic(fmt.Sprintf("parsing settings, %v", err))
	}
	if err := s.Validate(); err != nil {
		// Failing to validate means that there is some error in the Settings, so we should crash
		panic(fmt.Sprintf("validating settings, %v", err))
	}
	return s, nil
}

// Validate leverages struct tags with go-playground/validator so you can define a struct with custom
// validation on fields i.e.
//
//	type ExampleStruct struct {
//	    Example  metav1.Duration `json:"example" validate:"required,min=10m"`
//	}
func (s Settings) Validate() (err error) {
	validate := validator.New()
	if s.BatchMaxDuration.Duration <= 0 {
		err = multierr.Append(err, fmt.Errorf("batchMaxDuration cannot be negative"))
	}
	if s.BatchIdleDuration.Duration <= 0 {
		err = multierr.Append(err, fmt.Errorf("batchMaxDuration cannot be negative"))
	}
	return multierr.Append(err, validate.Struct(s))
}

// AsMetaDuration parses the value at key as a time.Duration into the target, if it exists.
func AsMetaDuration(key string, target *metav1.Duration) configmap.ParseFunc {
	return func(data map[string]string) error {
		if raw, ok := data[key]; ok {
			val, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("failed to parse %q: %w", key, err)
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
