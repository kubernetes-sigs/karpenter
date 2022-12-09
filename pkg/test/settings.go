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

package test

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imdario/mergo"

	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	"github.com/aws/karpenter-core/pkg/operator/settingsstore"
)

var _ settingsstore.Store = SettingsStore{}

// SettingsStore is a map from ContextKey to settings/config data
type SettingsStore map[interface{}]interface{}

func (ss SettingsStore) InjectSettings(ctx context.Context) context.Context {
	for k, v := range ss {
		ctx = context.WithValue(ctx, k, v)
	}
	return ctx
}

type SettingsOptions struct {
	DriftEnabled bool
}

func Settings(overrides ...SettingsOptions) settings.Settings {
	options := SettingsOptions{}
	for _, opts := range overrides {
		if err := mergo.Merge(&options, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("Failed to merge pod options: %s", err))
		}
	}
	return settings.Settings{
		BatchMaxDuration:  metav1.Duration{},
		BatchIdleDuration: metav1.Duration{},
		DriftEnabled:      options.DriftEnabled,
	}
}
