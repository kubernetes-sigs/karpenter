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
	"fmt"
	"regexp"
	"strings"

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

	// Validate driver name against Kubernetes RFC 1123 subdomain requirements
	if !isValidRFC1123Subdomain(c.Driver) {
		return &ValidationError{
			Field:   "driver",
			Message: fmt.Sprintf("driver name '%s' must be a valid RFC 1123 subdomain (lowercase alphanumeric characters, '-' or '.', starting and ending with alphanumeric)", c.Driver),
		}
	}

	if len(c.Mappings) == 0 {
		return &ValidationError{Field: "mappings", Message: "at least one mapping must be defined"}
	}

	for i, mapping := range c.Mappings {
		if err := mapping.Validate(); err != nil {
			return &ValidationError{
				Field:   fmt.Sprintf("mappings[%d]", i),
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

	// Validate attribute names are valid C identifiers for Kubernetes
	for attrName := range d.Attributes {
		if !isValidCIdentifier(attrName) {
			return &ValidationError{
				Field:   "attributes",
				Message: fmt.Sprintf("attribute name '%s' must be a valid C identifier (alphanumeric and underscore only, starting with letter or underscore)", attrName),
			}
		}
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

// isValidRFC1123Subdomain validates that a string is a valid RFC 1123 subdomain
// (lowercase alphanumeric characters, '-' or '.', starting and ending with alphanumeric)
func isValidRFC1123Subdomain(name string) bool {
	// RFC 1123 subdomain regex pattern
	rfc1123SubdomainRegex := regexp.MustCompile(`^[a-z0-9]([a-z0-9\-\.]*[a-z0-9])?$`)

	// Additional constraints: max 253 characters total, labels max 63 characters
	if len(name) > 253 {
		return false
	}

	// Check each label (between dots) is max 63 characters
	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 || len(label) == 0 {
			return false
		}
	}

	return rfc1123SubdomainRegex.MatchString(name)
}

// isValidCIdentifier validates that a string is a valid C identifier
// (alphanumeric and underscore only, starting with letter or underscore)
func isValidCIdentifier(name string) bool {
	// C identifier regex: starts with letter or underscore, followed by alphanumeric or underscore
	cIdentifierRegex := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	return cIdentifierRegex.MatchString(name)
}

// SanitizeDriverName converts a driver name to be RFC 1123 subdomain compliant
func SanitizeDriverName(name string) string {
	// Convert to lowercase (though it may already be lowercase)
	sanitized := strings.ToLower(name)

	// Replace slashes and other invalid characters with dots (more semantic for driver names)
	sanitized = strings.ReplaceAll(sanitized, "/", ".")

	// Replace any remaining invalid characters with hyphens
	sanitized = regexp.MustCompile(`[^a-z0-9\.\-]`).ReplaceAllString(sanitized, "-")

	// Remove leading/trailing non-alphanumeric characters
	sanitized = regexp.MustCompile(`^[^a-z0-9]+|[^a-z0-9]+$`).ReplaceAllString(sanitized, "")

	// Collapse multiple consecutive hyphens/dots
	sanitized = regexp.MustCompile(`[\.\-]{2,}`).ReplaceAllString(sanitized, ".")

	// Ensure it's not empty and doesn't exceed length limits
	if len(sanitized) == 0 {
		sanitized = "dra-driver"
	}
	if len(sanitized) > 253 {
		sanitized = sanitized[:253]
	}

	return sanitized
}

// SanitizeAttributeName converts an attribute name to be C identifier compliant
func SanitizeAttributeName(name string) string {
	// Replace hyphens and other invalid characters with underscores
	sanitized := regexp.MustCompile(`[^A-Za-z0-9_]`).ReplaceAllString(name, "_")

	// Ensure it starts with letter or underscore
	if len(sanitized) > 0 && regexp.MustCompile(`^[0-9]`).MatchString(sanitized) {
		sanitized = "_" + sanitized
	}

	// Collapse multiple consecutive underscores
	sanitized = regexp.MustCompile(`_{2,}`).ReplaceAllString(sanitized, "_")

	// Ensure it's not empty
	if len(sanitized) == 0 {
		sanitized = "_attr"
	}

	return sanitized
}
