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

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/config"
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers Suite")
}

var _ = Describe("ConfigMapController", func() {
	var (
		ctx                context.Context
		controller         *ConfigMapController
		fakeClient         *fake.ClientBuilder
		configMapName      = "dra-kwok-config"
		configMapNamespace = "karpenter"
		receivedConfig     *config.Config
		onConfigChange     = func(cfg *config.Config) {
			receivedConfig = cfg
		}
	)

	BeforeEach(func() {
		ctx = context.Background()
		fakeClient = fake.NewClientBuilder()
		receivedConfig = nil
	})

	Describe("parseConfigFromConfigMap", func() {
		It("should parse valid YAML configuration", func() {
			configMap := &corev1.ConfigMap{
				Data: map[string]string{
					"config.yaml": `
driver: kwok.example.com/gpu
mappings:
- name: gpu-mapping
  nodeSelector:
    matchLabels:
      node.kubernetes.io/instance-type: g4dn.xlarge
  resourceSlice:
    devices:
    - name: nvidia-gpu
      count: 1
      attributes:
        type: nvidia-tesla-v100
        memory: 32Gi
`,
				},
			}

			controller = NewConfigMapController(fakeClient.Build(), configMapName, configMapNamespace, onConfigChange)
			cfg, err := controller.parseConfigFromConfigMap(configMap)

			Expect(err).ToNot(HaveOccurred())
			Expect(cfg).ToNot(BeNil())
			Expect(cfg.Driver).To(Equal("kwok.example.com/gpu"))
			Expect(cfg.Mappings).To(HaveLen(1))
			Expect(cfg.Mappings[0].Name).To(Equal("gpu-mapping"))
			Expect(cfg.Mappings[0].ResourceSlice.Devices).To(HaveLen(1))
			Expect(cfg.Mappings[0].ResourceSlice.Devices[0].Name).To(Equal("nvidia-gpu"))
		})

		It("should handle different config key names", func() {
			controller = NewConfigMapController(fakeClient.Build(), configMapName, configMapNamespace, onConfigChange)

			testCases := []string{"config.yaml", "config.yml", "config", "dra-kwok-config.yaml"}

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
    devices:
    - name: test-device
      count: 1
`,
					},
				}

				cfg, err := controller.parseConfigFromConfigMap(configMap)
				Expect(err).ToNot(HaveOccurred())
				Expect(cfg.Driver).To(Equal("test.example.com/device"))
			}
		})

		It("should return error for missing configuration", func() {
			configMap := &corev1.ConfigMap{
				Data: map[string]string{
					"other-key": "some-data",
				},
			}

			controller = NewConfigMapController(fakeClient.Build(), configMapName, configMapNamespace, onConfigChange)
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

			controller = NewConfigMapController(fakeClient.Build(), configMapName, configMapNamespace, onConfigChange)
			_, err := controller.parseConfigFromConfigMap(configMap)

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to parse YAML configuration"))
		})
	})

	Describe("Reconcile", func() {
		It("should ignore ConfigMaps with different names", func() {
			client := fakeClient.Build()
			controller = NewConfigMapController(client, configMapName, configMapNamespace, onConfigChange)

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "different-configmap",
					Namespace: configMapNamespace,
				},
			}

			result, err := controller.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(receivedConfig).To(BeNil())
		})

		It("should ignore ConfigMaps in different namespaces", func() {
			client := fakeClient.Build()
			controller = NewConfigMapController(client, configMapName, configMapNamespace, onConfigChange)

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      configMapName,
					Namespace: "different-namespace",
				},
			}

			result, err := controller.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(receivedConfig).To(BeNil())
		})

		It("should handle ConfigMap not found", func() {
			client := fakeClient.Build()
			controller = NewConfigMapController(client, configMapName, configMapNamespace, onConfigChange)

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}

			result, err := controller.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(receivedConfig).To(BeNil()) // Should call onConfigChange with nil
		})

		It("should successfully process valid ConfigMap", func() {
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"config.yaml": `
driver: kwok.example.com/gpu
mappings:
- name: gpu-mapping
  nodeSelector:
    matchLabels:
      node.kubernetes.io/instance-type: g4dn.xlarge
  resourceSlice:
    devices:
    - name: nvidia-gpu
      count: 2
      attributes:
        type: nvidia-tesla-v100
`,
				},
			}

			client := fakeClient.WithObjects(configMap).Build()
			controller = NewConfigMapController(client, configMapName, configMapNamespace, onConfigChange)

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}

			result, err := controller.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			Expect(receivedConfig).ToNot(BeNil())
			Expect(receivedConfig.Driver).To(Equal("kwok.example.com/gpu"))
			Expect(receivedConfig.Mappings).To(HaveLen(1))
		})

		It("should handle invalid configuration gracefully", func() {
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"config.yaml": `
driver: ""  # Invalid - empty driver name
mappings: []
`,
				},
			}

			client := fakeClient.WithObjects(configMap).Build()
			controller = NewConfigMapController(client, configMapName, configMapNamespace, onConfigChange)

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}

			// Should not return error to avoid constant requeuing
			result, err := controller.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Should not update configuration for invalid config
			Expect(receivedConfig).To(BeNil())
		})
	})

	Describe("LoadInitialConfig", func() {
		It("should load initial configuration successfully", func() {
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"config.yaml": `
driver: kwok.example.com/gpu
mappings:
- name: gpu-mapping
  nodeSelector:
    matchLabels:
      node.kubernetes.io/instance-type: g4dn.xlarge
  resourceSlice:
    devices:
    - name: nvidia-gpu
      count: 1
`,
				},
			}

			client := fakeClient.WithObjects(configMap).Build()
			controller = NewConfigMapController(client, configMapName, configMapNamespace, onConfigChange)

			err := controller.LoadInitialConfig(ctx)
			Expect(err).ToNot(HaveOccurred())

			cfg := controller.GetConfig()
			Expect(cfg).ToNot(BeNil())
			Expect(cfg.Driver).To(Equal("kwok.example.com/gpu"))
		})

		It("should handle missing ConfigMap gracefully", func() {
			client := fakeClient.Build()
			controller = NewConfigMapController(client, configMapName, configMapNamespace, onConfigChange)

			err := controller.LoadInitialConfig(ctx)
			Expect(err).ToNot(HaveOccurred())

			cfg := controller.GetConfig()
			Expect(cfg).To(BeNil())
		})

		It("should return error for invalid initial configuration", func() {
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"config.yaml": `
driver: ""  # Invalid
mappings: []
`,
				},
			}

			client := fakeClient.WithObjects(configMap).Build()
			controller = NewConfigMapController(client, configMapName, configMapNamespace, onConfigChange)

			err := controller.LoadInitialConfig(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid initial configuration"))
		})
	})

	Describe("GetConfig", func() {
		It("should return nil when no configuration is loaded", func() {
			client := fakeClient.Build()
			controller = NewConfigMapController(client, configMapName, configMapNamespace, onConfigChange)

			cfg := controller.GetConfig()
			Expect(cfg).To(BeNil())
		})

		It("should return current configuration", func() {
			client := fakeClient.Build()
			controller = NewConfigMapController(client, configMapName, configMapNamespace, onConfigChange)

			testConfig := &config.Config{
				Driver: "test.example.com/device",
				Mappings: []config.Mapping{
					{
						Name: "test-mapping",
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{"test": "value"},
						},
						ResourceSlice: config.ResourceSliceConfig{
							Devices: []config.DeviceConfig{
								{Name: "test-device", Count: 1},
							},
						},
					},
				},
			}

			controller.driverConfig = testConfig

			cfg := controller.GetConfig()
			Expect(cfg).To(Equal(testConfig))
		})
	})
})
