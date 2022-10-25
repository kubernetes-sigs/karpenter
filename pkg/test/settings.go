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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aws/karpenter-core/pkg/apis/config/settings"
)

// SettingsStore is a map from ContextKey to settings/config data
type SettingsStore map[interface{}]interface{}

func (ss SettingsStore) InjectSettings(ctx context.Context) context.Context {
	for k, v := range ss {
		ctx = context.WithValue(ctx, k, v)
	}
	return ctx
}

func Settings() settings.Settings {
	return settings.Settings{
		BatchMaxDuration:  metav1.Duration{Duration: time.Second * 10},
		BatchIdleDuration: metav1.Duration{Duration: time.Second},
	}
}
