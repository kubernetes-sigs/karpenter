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

package config

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
)

// Registration defines a ConfigMap registration to be watched by the settingsstore.Watcher
// and to be injected into the Reconcile() contexts of controllers
type Registration struct {
	ConfigMapName string
	Constructor   func(*v1.ConfigMap) (interface{}, error)
}

func (r Registration) Validate() error {
	if r.ConfigMapName == "" {
		return fmt.Errorf("configMapName cannot be empty in SettingsStore registration")
	}
	if r.Constructor == nil {
		return fmt.Errorf("constructor cannot be nil")
	}
	return nil
}
