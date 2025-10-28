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

package controllers

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/config"
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers Suite")
}

var _ = Describe("ConfigMapController", func() {
	var (
		controller         *ConfigMapController
		fakeClient         *fake.ClientBuilder
		configMapName      = "dra-kwok-configmap"
		configMapNamespace = "karpenter"
	)

	BeforeEach(func() {
		fakeClient = fake.NewClientBuilder()
		controller = NewConfigMapController(fakeClient.Build(), configMapName, configMapNamespace, nil)
	})

	// Unit tests focus on core parsing and validation logic
	// Integration tests with real Kubernetes resources are in test/suites/regression/dra_kwok_test.go

	Describe("parseConfigFromConfigMap", func() {
		It("should parse valid YAML configuration", func() {
			configMap := &corev1.ConfigMap{
				Data: map[string]string{
					"config.yaml": `
driver: karpenter.sh/dra-kwok-driver
mappings:
- name: gpu-mapping
  nodeSelector:
    matchLabels:
      node.kubernetes.io/instance-type: g4dn.xlarge
  resourceSlice:
    driver: karpenter.sh/dra-kwok-driver
    pool:
      name: gpu-pool
      resourceSliceCount: 1
    devices:
    - name: nvidia-gpu-0
      attributes:
        type:
          string: nvidia-tesla-t4
        memory:
          string: 16Gi
`,
				},
			}

			cfg, err := controller.parseConfigFromConfigMap(configMap)

			Expect(err).ToNot(HaveOccurred())
			Expect(cfg).ToNot(BeNil())
			Expect(cfg.Driver).To(Equal("karpenter.sh.dra-kwok-driver"))
			Expect(cfg.Mappings).To(HaveLen(1))
			Expect(cfg.Mappings[0].Name).To(Equal("gpu-mapping"))
			Expect(cfg.Mappings[0].ResourceSlice.Devices).To(HaveLen(1))
			Expect(cfg.Mappings[0].ResourceSlice.Devices[0].Name).To(Equal("nvidia-gpu-0"))
			Expect(cfg.Mappings[0].ResourceSlice.Driver).To(Equal("karpenter.sh/dra-kwok-driver"))
			Expect(cfg.Mappings[0].ResourceSlice.Pool.Name).To(Equal("gpu-pool"))
		})

		It("should handle different config key names", func() {
			testCases := []string{"config.yaml"}

			for _, key := range testCases {
				configMap := &corev1.ConfigMap{
					Data: map[string]string{
						key: `
driver: test.example.com/device
mappings:
- name: test-mapping
  nodeSelector:
    matchLabels:
      test: value
  resourceSlice:
    driver: test.example.com/device
    pool:
      name: test-pool
      resourceSliceCount: 1
    devices:
    - name: test-device-0
`,
					},
				}

				cfg, err := controller.parseConfigFromConfigMap(configMap)
				Expect(err).ToNot(HaveOccurred())
				Expect(cfg.Driver).To(Equal("test.example.com.device"))
			}
		})

		It("should return error for missing configuration keys", func() {
			configMap := &corev1.ConfigMap{
				Data: map[string]string{
					"other-key": "some-data",
				},
			}

			_, err := controller.parseConfigFromConfigMap(configMap)

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no configuration found in configmap"))
		})

		It("should return error for invalid YAML", func() {
			configMap := &corev1.ConfigMap{
				Data: map[string]string{
					"config.yaml": "invalid: yaml: content: [",
				},
			}

			_, err := controller.parseConfigFromConfigMap(configMap)

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to parse YAML configuration"))
		})
	})

	Describe("GetConfig", func() {
		It("should return nil when no configuration is loaded", func() {
			cfg := controller.GetConfig()
			Expect(cfg).To(BeNil())
		})

		It("should return current configuration after setting", func() {
			testConfig := &config.Config{
				Driver: "test.example.com/device",
			}

			controller.driverConfig = testConfig

			cfg := controller.GetConfig()
			Expect(cfg).To(Equal(testConfig))
		})
	})

	Describe("Constructor", func() {
		It("should create controller with correct parameters", func() {
			onConfigChangeCalled := false
			testClient := fake.NewClientBuilder().Build()
			testController := NewConfigMapController(
				testClient,
				"test-config",
				"test-namespace",
				func(cfg *config.Config) {
					onConfigChangeCalled = true
				},
			)

			Expect(testController).ToNot(BeNil())
			Expect(testController.configMapName).To(Equal("test-config"))
			Expect(testController.configMapNamespace).To(Equal("test-namespace"))

			// Test callback function works
			testController.onConfigChange(nil)
			Expect(onConfigChangeCalled).To(BeTrue())
		})
	})
})
