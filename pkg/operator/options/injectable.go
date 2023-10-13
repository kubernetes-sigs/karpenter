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

package options

import (
	"context"

	"github.com/aws/karpenter-core/pkg/apis/settings"
)

// Injectable defines a ConfigMap registration to be loaded into context on startup
type Injectable interface {
	Inject(context.Context, ...string) (context.Context, error)
	MergeSettings(context.Context, ...settings.Injectable) context.Context

	// Note: required to extract options from root context and insert into manager context
	ToContext(context.Context, Injectable) context.Context
	FromContext(context.Context) Injectable
}
