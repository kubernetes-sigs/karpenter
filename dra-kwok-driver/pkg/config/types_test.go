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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config Suite")
}

var _ = Describe("Config", func() {
	Describe("Validation", func() {
		It("should validate a complete config", func() {
			config := &Config{
				Driver: "karpenter.sh/dra-kwok-driver",
				Mappings: []Mapping{
					{
						Name: "gpu-mapping",
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"node.kubernetes.io/instance-type": "g4dn.xlarge",
							},
						},
						ResourceSlice: ResourceSliceConfig{
							Devices: []DeviceConfig{
								{
									Name:  "nvidia-gpu",
									Count: 1,
									Attributes: map[string]string{
										"type":   "nvidia-tesla-v100",
										"memory": "32Gi",
									},
								},
							},
						},
					},
				},
			}
			Expect(config.Driver).To(Equal("karpenter.sh/dra-kwok-driver"))
			Expect(config.Mappings).To(HaveLen(1))
			Expect(config.Mappings[0].Name).To(Equal("gpu-mapping"))
			Expect(config.Mappings[0].ResourceSlice.Devices).To(HaveLen(1))
			Expect(config.Mappings[0].ResourceSlice.Devices[0].Name).To(Equal("nvidia-gpu"))
		})

		It("should return error for empty driver", func() {
			config := &Config{
				Driver: "",
				Mappings: []Mapping{
					{
						Name: "test",
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{"test": "value"},
						},
						ResourceSlice: ResourceSliceConfig{
							Devices: []DeviceConfig{
								{Name: "test", Count: 1},
							},
						},
					},
				},
			}
			err := config.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("driver name cannot be empty"))
		})

		It("should return error for no mappings", func() {
			config := &Config{
				Driver:   "test-driver",
				Mappings: []Mapping{},
			}
			err := config.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("at least one mapping must be defined"))
		})
	})

	Describe("DeviceConfig", func() {
		It("should support device configuration with attributes", func() {
			device := DeviceConfig{
				Name:  "multi-gpu",
				Count: 4,
				Attributes: map[string]string{
					"type":   "nvidia-v100",
					"memory": "32Gi",
				},
			}
			Expect(device.Name).To(Equal("multi-gpu"))
			Expect(device.Count).To(Equal(4))
			Expect(device.Attributes).To(HaveLen(2))
			Expect(device.Attributes["type"]).To(Equal("nvidia-v100"))
			Expect(device.Attributes["memory"]).To(Equal("32Gi"))
		})

		It("should validate device name is required", func() {
			device := DeviceConfig{
				Name:  "",
				Count: 1,
			}
			err := device.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("device name cannot be empty"))
		})

		It("should validate device count is positive", func() {
			device := DeviceConfig{
				Name:  "test-device",
				Count: 0,
			}
			err := device.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("device count must be greater than zero"))
		})
	})

	Describe("Mapping", func() {
		It("should validate mapping correctly", func() {
			mapping := Mapping{
				Name: "test-mapping",
				NodeSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"karpenter.sh/provisioner": "gpu-provisioner",
					},
				},
				ResourceSlice: ResourceSliceConfig{
					Devices: []DeviceConfig{
						{
							Name:  "test-gpu",
							Count: 2,
						},
					},
				},
			}
			Expect(mapping.Name).To(Equal("test-mapping"))
			Expect(mapping.NodeSelector.MatchLabels["karpenter.sh/provisioner"]).To(Equal("gpu-provisioner"))
			Expect(mapping.ResourceSlice.Devices).To(HaveLen(1))

			err := mapping.Validate()
			Expect(err).ToNot(HaveOccurred())
		})

		It("should return error for empty name", func() {
			mapping := Mapping{
				Name: "",
				NodeSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{"test": "value"},
				},
				ResourceSlice: ResourceSliceConfig{
					Devices: []DeviceConfig{
						{Name: "test", Count: 1},
					},
				},
			}
			err := mapping.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("mapping name cannot be empty"))
		})

		It("should return error for empty node selector", func() {
			mapping := Mapping{
				Name:         "test",
				NodeSelector: metav1.LabelSelector{},
				ResourceSlice: ResourceSliceConfig{
					Devices: []DeviceConfig{
						{Name: "test", Count: 1},
					},
				},
			}
			err := mapping.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("nodeSelector must have at least one matchLabel or matchExpression"))
		})
	})

	Describe("ResourceSliceConfig", func() {
		It("should return error for no devices", func() {
			rsc := ResourceSliceConfig{
				Devices: []DeviceConfig{},
			}
			err := rsc.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("at least one device must be defined"))
		})
	})
})
