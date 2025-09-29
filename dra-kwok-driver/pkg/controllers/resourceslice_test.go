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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/dra-kwok-driver/pkg/config"
)

// stringPtr returns a pointer to a string
func stringPtr(s string) *string {
	return &s
}

var _ = Describe("ResourceSliceController", func() {
	var (
		ctx                context.Context
		resourceController *ResourceSliceController
		configController   *ConfigMapController
		fakeClient         client.Client
		scheme             *runtime.Scheme
		driverName         = "kwok.example.com/gpu"
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(resourcev1.AddToScheme(scheme)).To(Succeed())

		fakeClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		configController = NewConfigMapController(fakeClient, "dra-kwok-config", "karpenter", nil)
		resourceController = NewResourceSliceController(fakeClient, driverName, configController)
	})

	Describe("isKWOKNode", func() {
		It("should identify KWOK nodes by provider label", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"kwok.x-k8s.io/provider": "kwok",
					},
				},
			}
			Expect(resourceController.isKWOKNode(node)).To(BeTrue())
		})

		It("should identify KWOK nodes by type label", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"type": "kwok",
					},
				},
			}
			Expect(resourceController.isKWOKNode(node)).To(BeTrue())
		})

		It("should identify KWOK nodes by annotation", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"kwok.x-k8s.io/node": "fake",
					},
				},
			}
			Expect(resourceController.isKWOKNode(node)).To(BeTrue())
		})

		It("should not identify regular nodes as KWOK", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"kubernetes.io/os": "linux",
					},
				},
			}
			Expect(resourceController.isKWOKNode(node)).To(BeFalse())
		})
	})

	Describe("findMatchingMapping", func() {
		var mappings []config.Mapping

		BeforeEach(func() {
			mappings = []config.Mapping{
				{
					Name: "gpu-mapping",
					NodeSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"node.kubernetes.io/instance-type": "g4dn.xlarge",
						},
					},
					ResourceSlice: config.ResourceSliceConfig{
						Devices: []config.DeviceConfig{
							{Name: "nvidia-gpu", Count: 1},
						},
					},
				},
				{
					Name: "multi-gpu-mapping",
					NodeSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"node.kubernetes.io/instance-type": "p3.2xlarge",
							"accelerator":                      "nvidia-tesla-v100",
						},
					},
					ResourceSlice: config.ResourceSliceConfig{
						Devices: []config.DeviceConfig{
							{Name: "nvidia-gpu", Count: 4},
						},
					},
				},
			}
		})

		It("should find matching mapping by single label", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "g4dn.xlarge",
					},
				},
			}

			mapping := resourceController.findMatchingMapping(node, mappings)
			Expect(mapping).ToNot(BeNil())
			Expect(mapping.Name).To(Equal("gpu-mapping"))
		})

		It("should find matching mapping by multiple labels", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "p3.2xlarge",
						"accelerator":                      "nvidia-tesla-v100",
						"other-label":                      "other-value",
					},
				},
			}

			mapping := resourceController.findMatchingMapping(node, mappings)
			Expect(mapping).ToNot(BeNil())
			Expect(mapping.Name).To(Equal("multi-gpu-mapping"))
		})

		It("should return nil for non-matching node", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "t3.micro",
					},
				},
			}

			mapping := resourceController.findMatchingMapping(node, mappings)
			Expect(mapping).To(BeNil())
		})

		It("should return first matching mapping when multiple match", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "g4dn.xlarge",
						"extra-label":                      "extra-value",
					},
				},
			}

			mapping := resourceController.findMatchingMapping(node, mappings)
			Expect(mapping).ToNot(BeNil())
			Expect(mapping.Name).To(Equal("gpu-mapping"))
		})
	})

	Describe("Reconcile", func() {
		var node *corev1.Node

		BeforeEach(func() {
			node = &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kwok-node-1",
					UID:  "node-uid-123",
					Labels: map[string]string{
						"kwok.x-k8s.io/provider":           "kwok",
						"node.kubernetes.io/instance-type": "g4dn.xlarge",
					},
				},
			}

			// Set up configuration in the config controller
			testConfig := &config.Config{
				Driver: driverName,
				Mappings: []config.Mapping{
					{
						Name: "gpu-mapping",
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"node.kubernetes.io/instance-type": "g4dn.xlarge",
							},
						},
						ResourceSlice: config.ResourceSliceConfig{
							Devices: []config.DeviceConfig{
								{
									Name:  "nvidia-gpu",
									Count: 2,
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
			configController.driverConfig = testConfig
		})

		It("should skip non-KWOK nodes", func() {
			regularNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "regular-node",
					Labels: map[string]string{
						"kubernetes.io/os": "linux",
					},
				},
			}

			Expect(fakeClient.Create(ctx, regularNode)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: regularNode.Name,
				},
			}

			result, err := resourceController.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Verify no ResourceSlices were created
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(0))
		})

		It("should handle missing configuration gracefully", func() {
			// Clear configuration
			configController.driverConfig = nil

			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: node.Name,
				},
			}

			result, err := resourceController.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Verify no ResourceSlices were created
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(0))
		})

		It("should create ResourceSlices for matching KWOK node", func() {
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: node.Name,
				},
			}

			result, err := resourceController.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Verify ResourceSlice was created
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(1))

			rs := resourceSlices.Items[0]
			Expect(*rs.Spec.NodeName).To(Equal(node.Name))
			Expect(rs.Spec.Driver).To(Equal(driverName))
			Expect(rs.Spec.Devices).To(HaveLen(2)) // 2 devices as configured
			Expect(rs.Labels["kwok.x-k8s.io/managed-by"]).To(Equal("dra-kwok-driver"))
			Expect(rs.Labels["kwok.x-k8s.io/node"]).To(Equal(node.Name))

			// Verify device attributes
			for _, device := range rs.Spec.Devices {
				Expect(device.Attributes).To(HaveLen(2))

				typeAttr, ok := device.Attributes[resourcev1.QualifiedName("type")]
				Expect(ok).To(BeTrue())
				Expect(typeAttr.StringValue).ToNot(BeNil())
				Expect(*typeAttr.StringValue).To(Equal("nvidia-tesla-v100"))

				memoryAttr, ok := device.Attributes[resourcev1.QualifiedName("memory")]
				Expect(ok).To(BeTrue())
				Expect(memoryAttr.StringValue).ToNot(BeNil())
				Expect(*memoryAttr.StringValue).To(Equal("32Gi"))
			}
		})

		It("should clean up ResourceSlices when node is deleted", func() {
			// First create the node and ResourceSlices
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: node.Name},
			}

			_, err := resourceController.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Verify ResourceSlice exists
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(1))

			// Delete the node
			Expect(fakeClient.Delete(ctx, node)).To(Succeed())

			// Reconcile after deletion
			result, err := resourceController.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Verify ResourceSlices are cleaned up
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(0))
		})

		It("should clean up ResourceSlices when no mapping matches", func() {
			// Create node with different labels that won't match
			nodeNoMatch := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kwok-node-no-match",
					UID:  "node-uid-456",
					Labels: map[string]string{
						"kwok.x-k8s.io/provider":           "kwok",
						"node.kubernetes.io/instance-type": "t3.micro", // Won't match mapping
					},
				},
			}

			Expect(fakeClient.Create(ctx, nodeNoMatch)).To(Succeed())

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: nodeNoMatch.Name},
			}

			result, err := resourceController.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Verify no ResourceSlices were created
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(0))
		})
	})

	Describe("GetResourceSlicesForNode", func() {
		It("should return empty slice when no ResourceSlices exist", func() {
			resourceSlices, err := resourceController.GetResourceSlicesForNode(ctx, "non-existent-node")
			Expect(err).ToNot(HaveOccurred())
			Expect(resourceSlices).To(HaveLen(0))
		})

		It("should return ResourceSlices for specified node", func() {
			// Create a ResourceSlice
			rs := &resourcev1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-resourceslice",
					Labels: map[string]string{
						"kwok.x-k8s.io/managed-by": "dra-kwok-driver",
						"kwok.x-k8s.io/node":       "test-node",
					},
				},
				Spec: resourcev1.ResourceSliceSpec{
					NodeName: stringPtr("test-node"),
					Driver:   driverName,
					Pool: resourcev1.ResourcePool{
						Name:               "test-pool",
						ResourceSliceCount: 1,
					},
				},
			}

			Expect(fakeClient.Create(ctx, rs)).To(Succeed())

			resourceSlices, err := resourceController.GetResourceSlicesForNode(ctx, "test-node")
			Expect(err).ToNot(HaveOccurred())
			Expect(resourceSlices).To(HaveLen(1))
			Expect(resourceSlices[0].Name).To(Equal("test-resourceslice"))
		})
	})
})
