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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	drav1alpha1 "sigs.k8s.io/karpenter/dra-kwok-driver/pkg/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

var _ = Describe("DRA KWOK Driver", func() {
	var (
		draDriverConfig *drav1alpha1.DRAConfig
		deployment      *appsv1.Deployment
		resourceSlice   *resourcev1.ResourceSlice
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
		if draDriverConfig != nil {
			env.ExpectDeleted(draDriverConfig)
		}
		if deployment != nil {
			env.ExpectDeleted(deployment)
		}
	})

	Context("GPU Configuration", func() {
		BeforeEach(func() {
			// Create DRAConfig CRD with GPU configuration
			draDriverConfig = &drav1alpha1.DRAConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test.karpenter.sh",
					Namespace: "karpenter",
				},
				Spec: drav1alpha1.DRAConfigSpec{
					Driver: "test.karpenter.sh",
					Mappings: []drav1alpha1.Mapping{
						{
							Name: "gpu-t4-mapping",
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      "node.kubernetes.io/instance-type",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{"c-4x-amd64-linux"},
										},
									},
								},
							},
							ResourceSlice: drav1alpha1.ResourceSliceTemplate{
								Pool: resourcev1.ResourcePool{
									Name:               "t4-gpu-pool",
									ResourceSliceCount: 1,
								},
								Devices: []resourcev1.Device{
									{
										Name: "nvidia-t4-0",
										Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
											"type":               {StringValue: lo.ToPtr("nvidia-tesla-t4")},
											"memory":             {StringValue: lo.ToPtr("16Gi")},
											"compute_capability": {StringValue: lo.ToPtr("7.5")},
											"cuda_cores":         {StringValue: lo.ToPtr("2560")},
										},
									},
								},
							},
						},
						{
							Name: "gpu-v100-mapping",
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      "node.kubernetes.io/instance-type",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{"m-8x-amd64-linux"},
										},
									},
								},
							},
							ResourceSlice: drav1alpha1.ResourceSliceTemplate{
								Pool: resourcev1.ResourcePool{
									Name:               "v100-gpu-pool",
									ResourceSliceCount: 1,
								},
								Devices: []resourcev1.Device{
									{
										Name: "nvidia-v100-0",
										Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
											"type":               {StringValue: lo.ToPtr("nvidia-tesla-v100")},
											"memory":             {StringValue: lo.ToPtr("32Gi")},
											"compute_capability": {StringValue: lo.ToPtr("7.0")},
											"cuda_cores":         {StringValue: lo.ToPtr("5120")},
											"nvlink":             {StringValue: lo.ToPtr("true")},
										},
									},
								},
							},
						},
					},
				},
			}
		})

		It("should create and manage ResourceSlices based on CRD configuration", func() {
			By("Creating the DRAConfig CRD")
			env.ExpectCreated(draDriverConfig)

			By("Waiting for CRD to be processed by DRA KWOK driver")
			// The DRA KWOK driver should detect the CRD immediately via watch
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
			Expect(resourceSlice.Spec.Driver).To(Equal("test.karpenter.sh"))
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

		It("should handle DRAConfig updates dynamically", func() {
			By("Creating initial DRAConfig")
			env.ExpectCreated(draDriverConfig)

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

			By("Updating the DRAConfig with new device configuration")
			Eventually(func() error {
				// Fetch latest version to avoid conflicts
				latest := &drav1alpha1.DRAConfig{}
				if err := env.Client.Get(env.Context, client.ObjectKeyFromObject(draDriverConfig), latest); err != nil {
					return err
				}

				// Update with additional devices
				latest.Spec.Mappings = []drav1alpha1.Mapping{
					{
						Name: "gpu-t4-updated-mapping",
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "node.kubernetes.io/instance-type",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{"c-4x-amd64-linux"},
									},
								},
							},
						},
						ResourceSlice: drav1alpha1.ResourceSliceTemplate{
							Pool: resourcev1.ResourcePool{
								Name:               "t4-gpu-pool-updated",
								ResourceSliceCount: 1,
							},
							Devices: []resourcev1.Device{
								{
									Name: "nvidia-t4-0",
									Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
										"type":               {StringValue: lo.ToPtr("nvidia-tesla-t4")},
										"memory":             {StringValue: lo.ToPtr("16Gi")},
										"compute_capability": {StringValue: lo.ToPtr("7.5")},
									},
								},
								{
									Name: "nvidia-t4-1",
									Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
										"type":               {StringValue: lo.ToPtr("nvidia-tesla-t4")},
										"memory":             {StringValue: lo.ToPtr("16Gi")},
										"compute_capability": {StringValue: lo.ToPtr("7.5")},
									},
								},
							},
						},
					},
				}

				return env.Client.Update(env.Context, latest)
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
			draDriverConfig = &drav1alpha1.DRAConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test.karpenter.sh",
					Namespace: "karpenter",
				},
				Spec: drav1alpha1.DRAConfigSpec{
					Driver: "test.karpenter.sh",
					Mappings: []drav1alpha1.Mapping{
						{
							Name: "fpga-c4x-mapping",
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      "node.kubernetes.io/instance-type",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{"c-4x-amd64-linux"},
										},
									},
								},
							},
							ResourceSlice: drav1alpha1.ResourceSliceTemplate{
								Pool: resourcev1.ResourcePool{
									Name:               "fpga-c4x-pool",
									ResourceSliceCount: 1,
								},
								Devices: []resourcev1.Device{
									{
										Name: "xilinx-u250-0",
										Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
											"type":       {StringValue: lo.ToPtr("xilinx-alveo-u250")},
											"memory":     {StringValue: lo.ToPtr("32Gi")},
											"dsp_slices": {StringValue: lo.ToPtr("6144")},
											"interface":  {StringValue: lo.ToPtr("pcie-gen3")},
										},
									},
								},
							},
						},
						{
							Name: "fpga-m8x-mapping",
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      "node.kubernetes.io/instance-type",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{"m-8x-amd64-linux"},
										},
									},
								},
							},
							ResourceSlice: drav1alpha1.ResourceSliceTemplate{
								Pool: resourcev1.ResourcePool{
									Name:               "fpga-m8x-pool",
									ResourceSliceCount: 1,
								},
								Devices: []resourcev1.Device{
									{
										Name: "xilinx-u250-0",
										Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
											"type":       {StringValue: lo.ToPtr("xilinx-alveo-u250")},
											"memory":     {StringValue: lo.ToPtr("64Gi")},
											"dsp_slices": {StringValue: lo.ToPtr("12288")},
											"interface":  {StringValue: lo.ToPtr("pcie-gen3")},
										},
									},
								},
							},
						},
					},
				},
			}

			env.ExpectCreated(draDriverConfig)

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
			draDriverConfig = &drav1alpha1.DRAConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test.karpenter.sh",
					Namespace: "karpenter",
				},
				Spec: drav1alpha1.DRAConfigSpec{
					Driver: "test.karpenter.sh",
					Mappings: []drav1alpha1.Mapping{
						{
							Name: "gpu-mapping-c4x-slice1",
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      "node.kubernetes.io/instance-type",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{"c-4x-amd64-linux"},
										},
									},
								},
							},
							ResourceSlice: drav1alpha1.ResourceSliceTemplate{
								Pool: resourcev1.ResourcePool{
									Name:               "multi-device-pool",
									ResourceSliceCount: 2,
								},
								Devices: []resourcev1.Device{
									{
										Name: "nvidia-t4-0",
										Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
											"type":         {StringValue: lo.ToPtr("nvidia-tesla-t4")},
											"memory":       {StringValue: lo.ToPtr("16Gi")},
											"device_class": {StringValue: lo.ToPtr("gpu")},
										},
									},
									{
										Name: "nvidia-t4-1",
										Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
											"type":         {StringValue: lo.ToPtr("nvidia-tesla-t4")},
											"memory":       {StringValue: lo.ToPtr("16Gi")},
											"device_class": {StringValue: lo.ToPtr("gpu")},
										},
									},
								},
							},
						},
						{
							Name: "gpu-mapping-c4x-slice2",
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      "node.kubernetes.io/instance-type",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{"c-4x-amd64-linux"},
										},
									},
								},
							},
							ResourceSlice: drav1alpha1.ResourceSliceTemplate{
								Pool: resourcev1.ResourcePool{
									Name:               "multi-device-pool",
									ResourceSliceCount: 2,
								},
								Devices: []resourcev1.Device{
									{
										Name: "xilinx-u250-0",
										Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
											"type":         {StringValue: lo.ToPtr("xilinx-alveo-u250")},
											"memory":       {StringValue: lo.ToPtr("32Gi")},
											"device_class": {StringValue: lo.ToPtr("fpga")},
										},
									},
								},
							},
						},
					},
				},
			}

			env.ExpectCreated(draDriverConfig)

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
		It("should reject invalid DRAConfig via API server validation", func() {
			// With CRD, invalid configs are rejected at creation time by API server
			invalidConfig := &drav1alpha1.DRAConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test.karpenter.sh",
					Namespace: "karpenter",
				},
				Spec: drav1alpha1.DRAConfigSpec{
					Driver:   "invalid driver name!",  // Invalid: contains spaces
					Mappings: []drav1alpha1.Mapping{}, // Invalid: empty mappings
				},
			}

			By("Attempting to create invalid DRAConfig")
			// API server should reject this due to validation rules
			err := env.Client.Create(env.Context, invalidConfig)
			Expect(err).To(HaveOccurred(), "API server should reject invalid DRAConfig")
			Expect(err.Error()).To(ContainSubstring("spec.driver"), "Error should mention driver field validation")

			By("Creating valid config with empty driver should also fail")
			invalidConfig2 := &drav1alpha1.DRAConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-config-2",
					Namespace: "karpenter",
				},
				Spec: drav1alpha1.DRAConfigSpec{
					Driver:   "", // Empty driver
					Mappings: []drav1alpha1.Mapping{},
				},
			}
			err = env.Client.Create(env.Context, invalidConfig2)
			Expect(err).To(HaveOccurred(), "API server should reject empty driver")

			By("Verifying that no ResourceSlices are created for invalid configs")
			// Since configs were rejected, no ResourceSlices should exist
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

			By("Verifying that no ResourceSlices are created for invalid configs")
			// Since configs were rejected, no ResourceSlices should exist
			Consistently(func() int {
				var resourceSlices resourcev1.ResourceSliceList
				err := env.Client.List(env.Context, &resourceSlices)
				if err != nil {
					return -1
				}
				return len(resourceSlices.Items)
			}, 10*time.Second, 1*time.Second).Should(Equal(0), "No ResourceSlices should exist for rejected configs")
		})
	})

	Context("DRA Pod Scheduling in KWOK Environment", func() {
		BeforeEach(func() {
			// Create DRAConfig with device configuration
			draDriverConfig = &drav1alpha1.DRAConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test.karpenter.sh",
					Namespace: "karpenter",
				},
				Spec: drav1alpha1.DRAConfigSpec{
					Driver: "test.karpenter.sh",
					Mappings: []drav1alpha1.Mapping{
						{
							Name: "gpu-scheduling-mapping",
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      "node.kubernetes.io/instance-type",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{"c-4x-amd64-linux"},
										},
									},
								},
							},
							ResourceSlice: drav1alpha1.ResourceSliceTemplate{
								Pool: resourcev1.ResourcePool{
									Name:               "test-gpu-pool",
									ResourceSliceCount: 1,
								},
								Devices: []resourcev1.Device{
									{
										Name: "nvidia-test-gpu",
										Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
											"type":   {StringValue: lo.ToPtr("nvidia-test-gpu")},
											"memory": {StringValue: lo.ToPtr("8Gi")},
										},
									},
								},
							},
						},
					},
				},
			}
		})

		It("should ignore DRA pods when IgnoreDRARequests is enabled (default behavior)", func() {
			By("Creating the DRAConfig to simulate DRA infrastructure")
			env.ExpectCreated(draDriverConfig)

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
			By("Creating the DRAConfig to set up DRA KWOK driver")
			env.ExpectCreated(draDriverConfig)

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

	Context("CRD-Specific Features", func() {
		It("should support DRAConfig CRD status fields", func() {
			By("Creating DRAConfig")
			draDriverConfig = &drav1alpha1.DRAConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test.karpenter.sh",
					Namespace: "karpenter",
				},
				Spec: drav1alpha1.DRAConfigSpec{
					Driver: "test.karpenter.sh",
					Mappings: []drav1alpha1.Mapping{
						{
							Name: "status-mapping",
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      "node.kubernetes.io/instance-type",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{"c-4x-amd64-linux"},
										},
									},
								},
							},
							ResourceSlice: drav1alpha1.ResourceSliceTemplate{
								Pool: resourcev1.ResourcePool{
									Name:               "status-pool",
									ResourceSliceCount: 1,
								},
								Devices: []resourcev1.Device{
									{
										Name: "test-device-0",
										Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
											"type": {StringValue: lo.ToPtr("test-gpu")},
										},
									},
								},
							},
						},
					},
				},
			}
			env.ExpectCreated(draDriverConfig)

			By("Verifying CRD can be read back")
			retrieved := &drav1alpha1.DRAConfig{}
			Eventually(func() error {
				return env.Client.Get(env.Context, client.ObjectKeyFromObject(draDriverConfig), retrieved)
			}, 10*time.Second, 1*time.Second).Should(Succeed(), "DRAConfig should be readable")

			By("Verifying status subresource exists and can be updated")
			// The status may be nil if not yet initialized by the controller
			if retrieved.Status != nil {
				Expect(retrieved.Status.ResourceSliceCount).To(BeNil()) // Not yet populated
				Expect(retrieved.Status.NodeCount).To(BeNil())          // Not yet populated
			}

			By("Simulating status update (as DRA driver controller would do)")
			// Retry status update to handle conflicts with driver controller
			Eventually(func() error {
				// Re-fetch to get latest resourceVersion
				latest := &drav1alpha1.DRAConfig{}
				if err := env.Client.Get(env.Context, client.ObjectKeyFromObject(draDriverConfig), latest); err != nil {
					return err
				}

				// Initialize Status if nil
				if latest.Status == nil {
					latest.Status = &drav1alpha1.DRAConfigStatus{}
				}

				latest.Status.Conditions = []metav1.Condition{
					{
						Type:               "Ready",
						Status:             metav1.ConditionTrue,
						LastTransitionTime: metav1.Now(),
						Reason:             "ConfigurationValid",
						Message:            "DRAConfig is valid and ready",
					},
				}
				latest.Status.NodeCount = ptr.To(int32(2))
				latest.Status.ResourceSliceCount = ptr.To(int32(2))

				// Update status subresource (may conflict if driver updates concurrently)
				return env.Client.Status().Update(env.Context, latest)
			}, 10*time.Second, 500*time.Millisecond).Should(Succeed(), "Status update should eventually succeed")

			By("Verifying status was persisted")
			updated := &drav1alpha1.DRAConfig{}
			Eventually(func() bool {
				if err := env.Client.Get(env.Context, client.ObjectKeyFromObject(draDriverConfig), updated); err != nil {
					return false
				}
				if updated.Status == nil {
					return false
				}
				return ptr.Deref(updated.Status.NodeCount, 0) == 2 &&
					ptr.Deref(updated.Status.ResourceSliceCount, 0) == 2 &&
					len(updated.Status.Conditions) > 0
			}, 10*time.Second, 1*time.Second).Should(BeTrue(), "Status should be persisted")

			By("Verifying Ready condition details")
			// The driver controller may also update conditions, so check that our condition exists
			var readyCondition *metav1.Condition
			for i := range updated.Status.Conditions {
				if updated.Status.Conditions[i].Type == "Ready" &&
					updated.Status.Conditions[i].Reason == "ConfigurationValid" {
					readyCondition = &updated.Status.Conditions[i]
					break
				}
			}
			Expect(readyCondition).ToNot(BeNil(), "Should have Ready condition with ConfigurationValid reason")
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))

			// Cleanup
			env.ExpectDeleted(draDriverConfig)
		})
	})
})
