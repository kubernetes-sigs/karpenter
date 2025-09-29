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

package config

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Config represents the DRA KWOK driver configuration loaded from ConfigMap
type Config struct {
	// Driver specifies the DRA driver name for ResourceSlices
	Driver string `yaml:"driver" json:"driver"`
	// Mappings defines how to map node labels to device configurations
	Mappings []Mapping `yaml:"mappings" json:"mappings"`
}

// Mapping defines a mapping from node selector to ResourceSlice configuration
type Mapping struct {
	// Name is a human-readable identifier for this mapping
	Name string `yaml:"name" json:"name"`
	// NodeSelector determines which nodes this mapping applies to
	NodeSelector metav1.LabelSelector `yaml:"nodeSelector" json:"nodeSelector"`
	// ResourceSlice defines the devices to create for matching nodes
	ResourceSlice ResourceSliceConfig `yaml:"resourceSlice" json:"resourceSlice"`
}

// ResourceSliceConfig defines the configuration for creating ResourceSlices
type ResourceSliceConfig struct {
	// Devices specifies the device configurations to create
	Devices []DeviceConfig `yaml:"devices" json:"devices"`
}

// DeviceConfig defines the configuration for a device type
type DeviceConfig struct {
	// Name is the base name for devices of this type
	Name string `yaml:"name" json:"name"`
	// Count specifies how many devices of this type to create
	Count int `yaml:"count" json:"count"`
	// Attributes defines the attributes for devices of this type
	Attributes map[string]string `yaml:"attributes" json:"attributes"`
}

// Validate validates the configuration and returns an error if invalid
func (c *Config) Validate() error {
	if c.Driver == "" {
		return &ValidationError{Field: "driver", Message: "driver name cannot be empty"}
	}

	if len(c.Mappings) == 0 {
		return &ValidationError{Field: "mappings", Message: "at least one mapping must be defined"}
	}

	for i, mapping := range c.Mappings {
		if err := mapping.Validate(); err != nil {
			return &ValidationError{
				Field:   "mappings[" + string(rune(i)) + "]",
				Message: err.Error(),
			}
		}
	}

	return nil
}

// Validate validates the mapping configuration
func (m *Mapping) Validate() error {
	if m.Name == "" {
		return &ValidationError{Field: "name", Message: "mapping name cannot be empty"}
	}

	if len(m.NodeSelector.MatchLabels) == 0 && len(m.NodeSelector.MatchExpressions) == 0 {
		return &ValidationError{
			Field:   "nodeSelector",
			Message: "nodeSelector must have at least one matchLabel or matchExpression",
		}
	}

	return m.ResourceSlice.Validate()
}

// Validate validates the ResourceSlice configuration
func (r *ResourceSliceConfig) Validate() error {
	if len(r.Devices) == 0 {
		return &ValidationError{Field: "devices", Message: "at least one device must be defined"}
	}

	for i, device := range r.Devices {
		if err := device.Validate(); err != nil {
			return &ValidationError{
				Field:   "devices[" + string(rune(i)) + "]",
				Message: err.Error(),
			}
		}
	}

	return nil
}

// Validate validates the device configuration
func (d *DeviceConfig) Validate() error {
	if d.Name == "" {
		return &ValidationError{Field: "name", Message: "device name cannot be empty"}
	}

	if d.Count <= 0 {
		return &ValidationError{Field: "count", Message: "device count must be greater than zero"}
	}

	return nil
}

// ValidationError represents a configuration validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Message
}
