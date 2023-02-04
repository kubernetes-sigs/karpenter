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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imdario/mergo"

	"github.com/aws/karpenter-core/pkg/apis/settings"
)

func Settings(overrides ...settings.Settings) *settings.Settings {
	options := settings.Settings{}
	for _, opts := range overrides {
		if err := mergo.Merge(&options, opts, mergo.WithOverride); err != nil {
			panic(fmt.Sprintf("Failed to merge pod options: %s", err))
		}
	}
	if options.BatchMaxDuration == nil {
		options.BatchMaxDuration = &metav1.Duration{}
	}
	if options.BatchIdleDuration == nil {
		options.BatchIdleDuration = &metav1.Duration{}
	}
	if options.TTLAfterNotRegistered == nil {
		options.TTLAfterNotRegistered = &metav1.Duration{Duration: time.Minute * 15}
	}
	return &settings.Settings{
		BatchMaxDuration:      options.BatchMaxDuration,
		BatchIdleDuration:     options.BatchIdleDuration,
		TTLAfterNotRegistered: options.TTLAfterNotRegistered,
		DriftEnabled:          options.DriftEnabled,
	}
}
