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
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/configmap"
)

type settingsKeyType struct{}

var ContextKey = settingsKeyType{}

var defaultSettings = &Settings{
	BatchMaxDuration:      &metav1.Duration{Duration: time.Second * 10},
	BatchIdleDuration:     &metav1.Duration{Duration: time.Second * 1},
	TTLAfterNotRegistered: &metav1.Duration{Duration: time.Minute * 15},
	DriftEnabled:          false,
}

// +k8s:deepcopy-gen=true
type Settings struct {
	BatchMaxDuration      *metav1.Duration
	BatchIdleDuration     *metav1.Duration
	TTLAfterNotRegistered *metav1.Duration
	// This feature flag is temporary and will be removed in the near future.
	DriftEnabled bool
}

func (*Settings) ConfigMap() string {
	return "karpenter-global-settings"
}

// Inject creates a Settings from the supplied ConfigMap
func (*Settings) Inject(ctx context.Context, cm *v1.ConfigMap) (context.Context, error) {
	s := defaultSettings.DeepCopy()

	if err := configmap.Parse(cm.Data,
		AsMetaDuration("batchMaxDuration", &s.BatchMaxDuration),
		AsMetaDuration("batchIdleDuration", &s.BatchIdleDuration),
		AsMetaDuration("ttlAfterNotRegistered", &s.TTLAfterNotRegistered),
		configmap.AsBool("featureGates.driftEnabled", &s.DriftEnabled),
	); err != nil {
		return ctx, fmt.Errorf("parsing settings, %w", err)
	}
	if err := s.Validate(); err != nil {
		return ctx, fmt.Errorf("validating settings, %w", err)
	}
	return ToContext(ctx, s), nil
}

func (in *Settings) Validate() (err error) {
	if in.BatchMaxDuration == nil {
		err = errors.Join(err, fmt.Errorf("batchMaxDuration is required"))
	} else if in.BatchMaxDuration.Duration <= 0 {
		err = errors.Join(err, fmt.Errorf("batchMaxDuration cannot be negative"))
	}
	if in.BatchIdleDuration == nil {
		err = errors.Join(err, fmt.Errorf("batchIdleDuration is required"))
	} else if in.BatchIdleDuration.Duration <= 0 {
		err = errors.Join(err, fmt.Errorf("batchIdleDuration cannot be negative"))
	}
	if in.TTLAfterNotRegistered != nil && in.TTLAfterNotRegistered.Duration <= 0 {
		err = errors.Join(err, fmt.Errorf("ttlAfterNotRegistered cannot be negative"))
	}
	return err
}

// AsMetaDuration parses the value at key as a time.Duration into the target, if it exists.
func AsMetaDuration(key string, target **metav1.Duration) configmap.ParseFunc {
	return func(data map[string]string) error {
		if raw, ok := data[key]; ok {
			if raw == "" {
				*target = nil
				return nil
			}
			val, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("failed to parse %q: %w", key, err)
			}
			*target = &metav1.Duration{Duration: val}
		}
		return nil
	}
}

func ToContext(ctx context.Context, s *Settings) context.Context {
	return context.WithValue(ctx, ContextKey, s)
}

func FromContext(ctx context.Context) *Settings {
	data := ctx.Value(ContextKey)
	if data == nil {
		// This is developer error if this happens, so we should panic
		panic("settings doesn't exist in context")
	}
	return data.(*Settings)
}
