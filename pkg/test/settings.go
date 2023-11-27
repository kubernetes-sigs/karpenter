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
	"fmt"
	"time"

	"github.com/imdario/mergo"

	"sigs.k8s.io/karpenter/pkg/apis/settings"
)

func Settings(overrides ...settings.Settings) *settings.Settings {
	options := settings.Settings{}
	for _, opts := range overrides {
		if err := mergo.Merge(&options, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("Failed to merge pod options: %s", err))
		}
	}
	if options.BatchMaxDuration == 0 {
		options.BatchMaxDuration = 10 * time.Second
	}
	if options.BatchIdleDuration == 0 {
		options.BatchIdleDuration = time.Second
	}
	return &settings.Settings{
		BatchMaxDuration:  options.BatchMaxDuration,
		BatchIdleDuration: options.BatchIdleDuration,
		DriftEnabled:      options.DriftEnabled,
	}
}
