/*
Copyright The Kubernetes Authors.

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

package v1

import (
	"encoding/json"
	"time"
)

const Never = "Never"

// NillableDuration is a wrapper around time.Duration which supports correct
// marshaling to YAML and JSON. It uses the value "Never" to signify
// that the duration is disabled and sets the inner duration as nil
type NillableDuration struct {
	raw string
	*time.Duration
}

func NewNillableDuration(duration string) (NillableDuration, error) {
	d := NillableDuration{}
	err := d.UnmarshalJSON([]byte(duration))
	return d, err
}

// UnmarshalJSON implements the json.Unmarshaller interface.
func (d *NillableDuration) UnmarshalJSON(b []byte) error {
	err := json.Unmarshal(b, &d.raw)
	if err != nil {
		return err
	}
	if d.raw == Never {
		return nil
	}
	pd, err := time.ParseDuration(d.raw)
	if err != nil {
		return err
	}
	d.Duration = &pd
	return nil
}

// MarshalJSON implements the json.Marshaler interface.
func (d NillableDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.raw)
	// if d.Duration == nil {
	// 	return json.Marshal(Never)
	// }
	// return json.Marshal(d.Duration.String())
}

// ToUnstructured implements the value.UnstructuredConverter interface.
func (d NillableDuration) ToUnstructured() interface{} {
	return d.raw
}
