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

package dra_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

var _ = Describe("DRA KWOK Driver", func() {
	var (
		draConfigMap  *corev1.ConfigMap
		deployment    *appsv1.Deployment
		resourceSlice *resourcev1.ResourceSlice
	)

	BeforeEach(func() {
		// Set up node pool for GPU nodes
		gvk := nodeClass.GroupVersionKind()
		nodePool.Spec.Template.Spec.NodeClassRef = &v1.NodeClassReference{
			Group: gvk.Group,
			Kind:  nodeClass.GetKind(),
			Name:  nodeClass.GetName(),
		}
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
			{
				Key:      "node.kubernetes.io/instance-type",
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"c-4x-amd64-linux", "m-8x-amd64-linux"},
			},
		}

		// Create the NodeClass and NodePool for testing
		env.ExpectCreated(nodeClass, nodePool)
	})

	AfterEach(func() {
		// Cleanup resources
		if draConfigMap != nil {
			env.ExpectDeleted(draConfigMap)
		}
		if deployment != nil {
			env.ExpectDeleted(deployment)
		}
	})

	Context("GPU Configuration", func() {
		BeforeEach(func() {
			// Create DRA KWOK driver ConfigMap with GPU configuration
			draConfigMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dra-kwok-configmap",
					Namespace: "karpenter",
				},
				Data: map[string]string{
					"config.yaml": `
# GPU DRA Configuration for KWOK Mock Driver using upstream ResourceSlice spec
driver: karpenter.sh

# Device mappings for different GPU node types
mappings:
  # NVIDIA T4 GPUs (c-4x-amd64-linux)
  - name: gpu-t4-mapping
    nodeSelectorTerms:
      - matchExpressions:
          - key: node.kubernetes.io/instance-type
            operator: In
            values: ["c-4x-amd64-linux"]
    resourceSlice:
      driver: karpenter.sh
      pool:
        name: t4-gpu-pool
        resourceSliceCount: 1
      devices:
        - name: nvidia-t4-0
          attributes:
            type:
              String: nvidia-tesla-t4
            memory:
              String: 16Gi
            compute_capability:
              String: "7.5"
            cuda_cores:
              String: "2560"

  # NVIDIA V100 GPUs (m-8x-amd64-linux)
  - name: gpu-v100-mapping
    nodeSelectorTerms:
      - matchExpressions:
          - key: node.kubernetes.io/instance-type
            operator: In
            values: ["m-8x-amd64-linux"]
    resourceSlice:
      driver: karpenter.sh
      pool:
        name: v100-gpu-pool
        resourceSliceCount: 1
      devices:
        - name: nvidia-v100-0
          attributes:
            type:
              String: nvidia-tesla-v100
            memory:
              String: 32Gi
            compute_capability:
              String: "7.0"
            cuda_cores:
              String: "5120"
            nvlink:
              string: "true"
`,
				},
			}
		})

		It("should create and manage ResourceSlices based on ConfigMap", func() {
			By("Creating the DRA ConfigMap")
			env.ExpectCreated(draConfigMap)

			By("Waiting for ConfigMap to be processed by DRA KWOK driver")
			// The DRA KWOK driver should detect the ConfigMap immediately via watch
			// But give it a moment to reconcile
			time.Sleep(2 * time.Second)

			By("Creating a deployment that requests GPU resources")
			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gpu-workload",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: lo.ToPtr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "gpu-workload",
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "gpu-workload",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "gpu-container",
									Image: "nvidia/cuda:11.2-runtime-ubuntu20.04",
									Command: []string{
										"sh", "-c", "nvidia-smi && sleep 3600",
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("100m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("100m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
								},
							},
							NodeSelector: map[string]string{
								"testing/cluster": "unspecified",
							},
						},
					},
				},
			}

			env.ExpectCreated(deployment)

			By("Expecting a node to be created with GPU support")
			env.EventuallyExpectHealthyPodCount(labels.SelectorFromSet(map[string]string{
				"app": "gpu-workload",
			}), 1)

			By("Verifying the node has the expected GPU labels")
			nodes := env.Monitor.CreatedNodes()
			Expect(nodes).To(HaveLen(1))

			node := nodes[0]
			// Should have GPU instance type
			Expect([]string{"c-4x-amd64-linux", "m-8x-amd64-linux"}).To(ContainElement(node.Labels["node.kubernetes.io/instance-type"]))

			By("Checking that ResourceSlices are created for the GPU node")
			var resourceSlices resourcev1.ResourceSliceList
			Eventually(func() []resourcev1.ResourceSlice {
				// List all ResourceSlices and filter by nodeName
				var allResourceSlices resourcev1.ResourceSliceList
				err := env.Client.List(env.Context, &allResourceSlices)
				if err != nil {
					return nil
				}

				// Filter ResourceSlices for this specific node
				var filteredSlices []resourcev1.ResourceSlice
				for _, slice := range allResourceSlices.Items {
					if slice.Spec.NodeName != nil && *slice.Spec.NodeName == node.Name {
						filteredSlices = append(filteredSlices, slice)
					}
				}
				resourceSlices.Items = filteredSlices
				return filteredSlices
			}, 2*time.Minute, 5*time.Second).Should(Not(BeEmpty()), "DRA driver should create ResourceSlices within 2 minutes (accounting for 30s polling interval)")

			By("Verifying ResourceSlice content while node exists")
			// The resourceSlices should already be populated from the Eventually check above
			Expect(resourceSlices.Items).To(HaveLen(1))
			resourceSlice = &resourceSlices.Items[0]

			// Verify ResourceSlice has correct driver and devices
			Expect(resourceSlice.Spec.Driver).To(Equal("karpenter.sh"))
			Expect(resourceSlice.Spec.Devices).To(HaveLen(1))

			device := resourceSlice.Spec.Devices[0]
			switch node.Labels["node.kubernetes.io/instance-type"] {
			case "c-4x-amd64-linux":
				Expect(device.Name).To(Equal("nvidia-t4-0"))
				Expect(*device.Attributes[resourcev1.QualifiedName("type")].StringValue).To(Equal("nvidia-tesla-t4"))
			case "m-8x-amd64-linux":
				Expect(device.Name).To(Equal("nvidia-v100-0"))
				Expect(*device.Attributes[resourcev1.QualifiedName("type")].StringValue).To(Equal("nvidia-tesla-v100"))
			}
		})

		It("should handle ConfigMap updates dynamically", func() {
			By("Creating initial ConfigMap")
			env.ExpectCreated(draConfigMap)

			By("Creating a GPU workload")
			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gpu-workload-update",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: lo.ToPtr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "gpu-workload-update",
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "gpu-workload-update",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "gpu-container",
									Image: "nvidia/cuda:11.2-runtime-ubuntu20.04",
									Command: []string{
										"sh", "-c", "sleep 3600",
									},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("100m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("100m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
								},
							},
							NodeSelector: map[string]string{
								"testing/cluster": "unspecified",
							},
						},
					},
				},
			}

			env.ExpectCreated(deployment)

			By("Waiting for initial node and ResourceSlice creation")
			env.EventuallyExpectHealthyPodCount(labels.SelectorFromSet(map[string]string{
				"app": "gpu-workload-update",
			}), 1)

			nodes := env.Monitor.CreatedNodes()
			Expect(nodes).To(HaveLen(1))
			node := nodes[0]

			By("Updating the ConfigMap with new device configuration")
			Eventually(func() error {
				// Update ConfigMap to add more devices
				draConfigMap.Data["config.yaml"] = `
driver: karpenter.sh
mappings:
  - name: gpu-t4-updated-mapping
    nodeSelectorTerms:
      - matchExpressions:
          - key: node.kubernetes.io/instance-type
            operator: In
            values: ["c-4x-amd64-linux"]
    resourceSlice:
      driver: karpenter.sh
      pool:
        name: t4-gpu-pool-updated
        resourceSliceCount: 1
      devices:
        - name: nvidia-t4-0
          attributes:
            type:
              String: nvidia-tesla-t4
            memory:
              String: 16Gi
            compute_capability:
              String: "7.5"
        - name: nvidia-t4-1
          attributes:
            type:
              String: nvidia-tesla-t4
            memory:
              String: 16Gi
            compute_capability:
              String: "7.5"
`
				return env.Client.Update(env.Context, draConfigMap)
			}, 10*time.Second, 1*time.Second).Should(Succeed())

			By("Verifying ResourceSlices are updated with new configuration")
			if node.Labels["node.kubernetes.io/instance-type"] == "c-4x-amd64-linux" {
				Eventually(func() int {
					// List all ResourceSlices and filter by nodeName
					var allResourceSlices resourcev1.ResourceSliceList
					err := env.Client.List(env.Context, &allResourceSlices)
					if err != nil {
						return 0
					}

					// Filter ResourceSlices for this specific node
					for _, slice := range allResourceSlices.Items {
						if slice.Spec.NodeName != nil && *slice.Spec.NodeName == node.Name {
							return len(slice.Spec.Devices)
						}
					}
					return 0
				}, 2*time.Minute, 5*time.Second).Should(Equal(2), "DRA driver should update ResourceSlices within 2 minutes after ConfigMap update (accounting for 30s polling interval)")
			}
		})
	})

	Context("Advanced Device Configuration", func() {
		It("should support FPGA device types", func() {
			draConfigMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dra-kwok-configmap",
					Namespace: "karpenter",
				},
				Data: map[string]string{
					"config.yaml": `
driver: karpenter.sh
mappings:
  # FPGA mapping for c-4x-amd64-linux nodes
  - name: fpga-c4x-mapping
    nodeSelectorTerms:
      - matchExpressions:
          - key: node.kubernetes.io/instance-type
            operator: In
            values: ["c-4x-amd64-linux"]
    resourceSlice:
      driver: karpenter.sh
      pool:
        name: fpga-c4x-pool
        resourceSliceCount: 1
      devices:
        - name: xilinx-u250-0
          attributes:
            type:
              String: xilinx-alveo-u250
            memory:
              String: 32Gi
            dsp_slices:
              String: "6144"
            interface:
              String: pcie-gen3

  # FPGA mapping for m-8x-amd64-linux nodes
  - name: fpga-m8x-mapping
    nodeSelectorTerms:
      - matchExpressions:
          - key: node.kubernetes.io/instance-type
            operator: In
            values: ["m-8x-amd64-linux"]
    resourceSlice:
      driver: karpenter.sh
      pool:
        name: fpga-m8x-pool
        resourceSliceCount: 1
      devices:
        - name: xilinx-u250-0
          attributes:
            type:
              String: xilinx-alveo-u250
            memory:
              String: 64Gi
            dsp_slices:
              String: "12288"
            interface:
              String: pcie-gen3
`,
				},
			}

			env.ExpectCreated(draConfigMap)

			By("Creating FPGA workload that should trigger node creation")
			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fpga-workload",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: lo.ToPtr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "fpga-workload",
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "fpga-workload",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "fpga-container",
									Image:   "ubuntu:20.04",
									Command: []string{"sleep", "3600"},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("100m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("100m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
								},
							},
							NodeSelector: map[string]string{
								"testing/cluster": "unspecified",
							},
						},
					},
				},
			}

			env.ExpectCreated(deployment)

			By("Expecting node with FPGA support to be created")
			env.EventuallyExpectHealthyPodCount(labels.SelectorFromSet(map[string]string{
				"app": "fpga-workload",
			}), 1)

			By("Verifying FPGA ResourceSlice is created")
			nodes := env.Monitor.CreatedNodes()
			Expect(nodes).To(HaveLen(1))

			Eventually(func() []resourcev1.ResourceSlice {
				// List all ResourceSlices and filter by nodeName
				var allResourceSlices resourcev1.ResourceSliceList
				err := env.Client.List(env.Context, &allResourceSlices)
				if err != nil {
					return nil
				}

				// Filter ResourceSlices for this specific node
				var filteredSlices []resourcev1.ResourceSlice
				for _, slice := range allResourceSlices.Items {
					if slice.Spec.NodeName != nil && *slice.Spec.NodeName == nodes[0].Name {
						filteredSlices = append(filteredSlices, slice)
					}
				}
				return filteredSlices
			}, 2*time.Minute, 5*time.Second).Should(HaveLen(1), "DRA driver should create ResourceSlices within 2 minutes (accounting for 30s polling interval)")
		})

		It("should support multiple ResourceSlices for single instance type", func() {
			draConfigMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dra-kwok-configmap",
					Namespace: "karpenter",
				},
				Data: map[string]string{
					"config.yaml": `
driver: karpenter.sh
mappings:
  # First ResourceSlice: GPU devices for c-4x-amd64-linux
  - name: gpu-mapping-c4x
    nodeSelectorTerms:
      - matchExpressions:
          - key: node.kubernetes.io/instance-type
            operator: In
            values: ["c-4x-amd64-linux"]
    resourceSlice:
      driver: karpenter.sh
      pool:
        name: multi-gpu-pool
        resourceSliceCount: 1
      devices:
        - name: nvidia-t4-0
          attributes:
            type:
              string: nvidia-tesla-t4
            memory:
              string: 16Gi
            device_class:
              string: gpu
        - name: nvidia-t4-1
          attributes:
            type:
              string: nvidia-tesla-t4
            memory:
              string: 16Gi
            device_class:
              string: gpu

  # Second ResourceSlice: FPGA devices for same instance type
  - name: fpga-mapping-c4x
    nodeSelectorTerms:
      - matchExpressions:
          - key: node.kubernetes.io/instance-type
            operator: In
            values: ["c-4x-amd64-linux"]
    resourceSlice:
      driver: karpenter.sh
      pool:
        name: multi-fpga-pool
        resourceSliceCount: 1
      devices:
        - name: xilinx-u250-0
          attributes:
            type:
              string: xilinx-alveo-u250
            memory:
              string: 32Gi
            device_class:
              string: fpga
`,
				},
			}

			env.ExpectCreated(draConfigMap)

			By("Creating workload for multi-device instance type")
			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "multi-device-workload",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: lo.ToPtr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "multi-device-workload",
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "multi-device-workload",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "multi-device-container",
									Image:   "ubuntu:20.04",
									Command: []string{"sleep", "3600"},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("200m"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("200m"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
									},
								},
							},
							NodeSelector: map[string]string{
								"testing/cluster":                  "unspecified",
								"node.kubernetes.io/instance-type": "c-4x-amd64-linux", // Force specific instance type
							},
						},
					},
				},
			}

			env.ExpectCreated(deployment)

			By("Expecting node with multi-device support to be created")
			env.EventuallyExpectHealthyPodCount(labels.SelectorFromSet(map[string]string{
				"app": "multi-device-workload",
			}), 1)

			By("Verifying multiple ResourceSlices are created for single node")
			nodes := env.Monitor.CreatedNodes()
			Expect(nodes).To(HaveLen(1))
			node := nodes[0]

			var allResourceSlices []resourcev1.ResourceSlice
			Eventually(func() []resourcev1.ResourceSlice {
				var resourceSliceList resourcev1.ResourceSliceList
				err := env.Client.List(env.Context, &resourceSliceList)
				if err != nil {
					return nil
				}

				var filteredSlices []resourcev1.ResourceSlice
				for _, slice := range resourceSliceList.Items {
					if slice.Spec.NodeName != nil && *slice.Spec.NodeName == node.Name {
						filteredSlices = append(filteredSlices, slice)
					}
				}
				allResourceSlices = filteredSlices
				return filteredSlices
			}, 2*time.Minute, 5*time.Second).Should(HaveLen(2), "DRA driver should create ResourceSlices within 2 minutes (accounting for 30s polling interval)")

			By("Verifying device separation by type")
			var gpuSlice, fpgaSlice *resourcev1.ResourceSlice
			for i := range allResourceSlices {
				slice := &allResourceSlices[i]
				if len(slice.Spec.Devices) > 0 {
					deviceClass, exists := slice.Spec.Devices[0].Attributes[resourcev1.QualifiedName("device_class")]
					if exists && deviceClass.StringValue != nil {
						switch *deviceClass.StringValue {
						case "gpu":
							gpuSlice = slice
						case "fpga":
							fpgaSlice = slice
						}
					}
				}
			}

			Expect(gpuSlice).ToNot(BeNil(), "Should have GPU ResourceSlice")
			Expect(fpgaSlice).ToNot(BeNil(), "Should have FPGA ResourceSlice")

			By("Validating GPU devices")
			Expect(gpuSlice.Spec.Devices).To(HaveLen(2), "Should have 2 GPU devices")
			for i, device := range gpuSlice.Spec.Devices {
				Expect(device.Name).To(Equal(fmt.Sprintf("nvidia-t4-%d", i)))
				Expect(*device.Attributes[resourcev1.QualifiedName("device_class")].StringValue).To(Equal("gpu"))
			}

			By("Validating FPGA devices")
			Expect(fpgaSlice.Spec.Devices).To(HaveLen(1), "Should have 1 FPGA device")
			fpgaDevice := fpgaSlice.Spec.Devices[0]
			Expect(fpgaDevice.Name).To(Equal("xilinx-u250-0"))
			Expect(*fpgaDevice.Attributes[resourcev1.QualifiedName("device_class")].StringValue).To(Equal("fpga"))

			By("Confirming both ResourceSlices reference same node")
			Expect(*gpuSlice.Spec.NodeName).To(Equal(node.Name))
			Expect(*fpgaSlice.Spec.NodeName).To(Equal(node.Name))
		})
	})

	Context("Configuration Validation", func() {
		It("should handle invalid ConfigMap gracefully", func() {
			draConfigMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dra-kwok-configmap",
					Namespace: "karpenter",
				},
				Data: map[string]string{
					"config.yaml": `
driver: ""  # Invalid empty driver
mappings: []
`,
				},
			}

			By("Creating invalid ConfigMap")
			env.ExpectCreated(draConfigMap)

			By("Creating workload that requests DRA resources")
			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-config-workload",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: lo.ToPtr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "invalid-config-workload",
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "invalid-config-workload",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "test-container",
									Image:   "ubuntu:20.04",
									Command: []string{"sleep", "60"},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"invalid.example.com/device": resource.MustParse("1"),
											corev1.ResourceCPU:           resource.MustParse("100m"),
											corev1.ResourceMemory:        resource.MustParse("128Mi"),
										},
										Limits: corev1.ResourceList{
											"invalid.example.com/device": resource.MustParse("1"),
											corev1.ResourceCPU:           resource.MustParse("100m"),
											corev1.ResourceMemory:        resource.MustParse("128Mi"),
										},
									},
								},
							},
							NodeSelector: map[string]string{
								"testing/cluster": "unspecified",
							},
						},
					},
				},
			}

			env.ExpectCreated(deployment)

			By("Verifying that no ResourceSlices are created with invalid config")
			Consistently(func() int {
				var resourceSlices resourcev1.ResourceSliceList
				err := env.Client.List(env.Context, &resourceSlices)
				if err != nil {
					return -1
				}
				// Filter for our driver
				count := 0
				for _, rs := range resourceSlices.Items {
					if rs.Spec.Driver == "" { // Invalid driver should not create slices
						count++
					}
				}
				return count
			}, 10*time.Second, 1*time.Second).Should(Equal(0))
		})
	})

	Context("DRA Pod Scheduling in KWOK Environment", func() {
		BeforeEach(func() {
			// Create DRA KWOK driver ConfigMap with device configuration
			draConfigMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dra-kwok-configmap",
					Namespace: "karpenter",
				},
				Data: map[string]string{
					"config.yaml": `
driver: karpenter.sh
mappings:
  - name: gpu-scheduling-mapping
    nodeSelectorTerms:
      - matchExpressions:
          - key: node.kubernetes.io/instance-type
            operator: In
            values: ["c-4x-amd64-linux"]
    resourceSlice:
      driver: karpenter.sh
      pool:
        name: test-gpu-pool
        resourceSliceCount: 1
      devices:
        - name: nvidia-test-gpu
          attributes:
            type:
              String: nvidia-test-gpu
            memory:
              String: 8Gi
`,
				},
			}
		})

		It("should ignore DRA pods when IgnoreDRARequests is enabled (default behavior)", func() {
			By("Creating the DRA ConfigMap to simulate DRA infrastructure")
			env.ExpectCreated(draConfigMap)

			By("Creating a deployment with DRA resource claims")
			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dra-ignored-test",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: lo.ToPtr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "dra-ignored",
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "dra-ignored",
							},
						},
						Spec: corev1.PodSpec{
							ResourceClaims: []corev1.PodResourceClaim{
								{
									Name:                      "gpu-claim",
									ResourceClaimTemplateName: lo.ToPtr("gpu-claim-template"),
								},
							},
							Containers: []corev1.Container{
								{
									Name:    "dra-container",
									Image:   "ubuntu:20.04",
									Command: []string{"sleep", "60"},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("512Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("512Mi"),
										},
										Claims: []corev1.ResourceClaim{
											{Name: "gpu-claim"},
										},
									},
								},
							},
							NodeSelector: map[string]string{
								"testing/cluster": "unspecified",
							},
						},
					},
				},
			}

			env.ExpectCreated(deployment)

			By("Verifying Karpenter ignores DRA pods to avoid wasting scheduling cycles")
			// Since Karpenter doesn't have DRA support implemented yet, DRA pods get ignored
			// This prevents wasting resources trying to schedule pods that can't be satisfied
			Consistently(func() int {
				return len(env.Monitor.CreatedNodes())
			}, 15*time.Second, 1*time.Second).Should(Equal(0), "No nodes should be created for DRA pods when DRA support is disabled in Karpenter")

			By("Verifying DRA pods remain pending (not scheduled)")
			// DRA pods should exist but not be running since Karpenter ignores them
			Consistently(func() int {
				return env.Monitor.RunningPodsCount(labels.SelectorFromSet(map[string]string{
					"app": "dra-ignored",
				}))
			}, 10*time.Second, 1*time.Second).Should(Equal(0), "DRA pods should remain unscheduled until Karpenter implements DRA support")

			By("Documenting current protective behavior")
			// This test validates that Karpenter correctly identifies and ignores DRA pods
			// preventing resource waste when DRA scheduling is not yet implemented
		})

		It("should prepare DRA testing infrastructure for future Karpenter DRA implementation", func() {
			By("Creating the DRA ConfigMap to set up DRA KWOK driver")
			env.ExpectCreated(draConfigMap)

			By("Creating a regular pod to trigger KWOK node creation")
			regularDeployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "regular-kwok-test",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: lo.ToPtr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "regular-kwok",
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "regular-kwok",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "regular-container",
									Image:   "ubuntu:20.04",
									Command: []string{"sleep", "60"},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("512Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("512Mi"),
										},
									},
								},
							},
							NodeSelector: map[string]string{
								"testing/cluster": "unspecified",
							},
						},
					},
				},
			}

			env.ExpectCreated(regularDeployment)

			By("Verifying regular pod scheduling works (non-DRA pods are unaffected)")
			env.EventuallyExpectHealthyPodCount(labels.SelectorFromSet(map[string]string{
				"app": "regular-kwok",
			}), 1)

			By("Verifying KWOK node creation with proper instance types")
			nodes := env.Monitor.CreatedNodes()
			Expect(nodes).To(HaveLen(1))
			if env.IsDefaultNodeClassKWOK() {
				// Verify KWOK environment is working correctly with expected instance types
				instanceType := nodes[0].Labels["node.kubernetes.io/instance-type"]
				Expect([]string{"c-4x-amd64-linux", "m-8x-amd64-linux"}).To(ContainElement(instanceType))
			}

			By("Validating DRA KWOK driver creates ResourceSlices for future DRA testing")
			// This prepares the testing infrastructure for when Karpenter implements DRA support
			// The DRA KWOK driver should create ResourceSlices that will enable DRA testing
			if env.IsDefaultNodeClassKWOK() {
				Eventually(func() []resourcev1.ResourceSlice {
					// List all ResourceSlices and filter by nodeName
					var allResourceSlices resourcev1.ResourceSliceList
					if err := env.Client.List(env.Context, &allResourceSlices); err != nil {
						return nil
					}

					// Filter ResourceSlices for this specific node
					var filteredSlices []resourcev1.ResourceSlice
					for _, slice := range allResourceSlices.Items {
						if slice.Spec.NodeName != nil && *slice.Spec.NodeName == nodes[0].Name {
							filteredSlices = append(filteredSlices, slice)
						}
					}
					return filteredSlices
				}, 2*time.Minute, 5*time.Second).Should(Not(BeEmpty()), "DRA KWOK driver should create ResourceSlices within 2 minutes (accounting for 30s polling interval)")
			}

			By("Ensuring test infrastructure is ready for DRA feature development")
			// This test validates that the DRA testing infrastructure works in KWOK
			// When Karpenter DRA support is implemented, these ResourceSlices will enable testing
		})
	})
})
