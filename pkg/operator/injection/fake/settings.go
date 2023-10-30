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

package fake

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/configmap"

	"github.com/aws/karpenter-core/pkg/apis/settings"
)

type settingsKeyType struct{}

var ContextKey = settingsKeyType{}

var defaultSettings = &Settings{
	TestArg: "default",
}

type Settings struct {
	TestArg string `json:"testArg"`
}

func (*Settings) ConfigMap() string {
	return "karpenter-global-settings"
}

func (*Settings) Inject(ctx context.Context, cm *v1.ConfigMap) (context.Context, error) {
	s := defaultSettings

	if err := configmap.Parse(cm.Data,
		configmap.AsString("testArg", &s.TestArg),
	); err != nil {
		return ctx, fmt.Errorf("parsing config data, %w", err)
	}
	return ToContext(ctx, s), nil
}

func (*Settings) FromContext(ctx context.Context) settings.Injectable {
	return FromContext(ctx)
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
