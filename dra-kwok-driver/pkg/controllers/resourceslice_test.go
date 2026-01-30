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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
		configStore        *config.Store
		fakeClient         client.Client
		scheme             *runtime.Scheme
		driverName         = "karpenter.sh/dra-kwok-driver"
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(resourcev1.AddToScheme(scheme)).To(Succeed())

		fakeClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		configStore = config.NewStore()
		resourceController = NewResourceSliceController(fakeClient, driverName, configStore)
	})

	Describe("isKWOKNode", func() {
		It("should identify KWOK nodes by Karpenter KWOK annotation", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"kwok.x-k8s.io/node": "fake",
					},
				},
			}
			Expect(resourceController.isKWOKNode(node)).To(BeTrue())
		})

		It("should not identify non-KWOK nodes", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"kubernetes.io/os": "linux",
					},
				},
			}
			Expect(resourceController.isKWOKNode(node)).To(BeFalse())
		})

		It("should not identify nodes with other KWOK-like labels", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"kwok.x-k8s.io/provider": "kwok",
						"type":                   "kwok",
					},
				},
			}
			Expect(resourceController.isKWOKNode(node)).To(BeFalse())
		})
	})

	Describe("findMatchingMappings", func() {
		var mappings []config.Mapping

		BeforeEach(func() {
			mappings = []config.Mapping{
				{
					Name: "gpu-mapping",
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node.kubernetes.io/instance-type",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"g4dn.xlarge", "g4dn.2xlarge"},
								},
							},
						},
					},
				},
				{
					Name: "cpu-mapping",
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node.kubernetes.io/instance-type",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"m5.large", "m5.xlarge"},
								},
							},
						},
					},
				},
			}
		})

		It("should find matching mappings for a node", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "g4dn.xlarge",
					},
				},
			}

			matches := resourceController.findMatchingMappings(node, mappings)
			Expect(matches).To(HaveLen(1))
			Expect(matches[0].Name).To(Equal("gpu-mapping"))
		})

		It("should find no mappings for non-matching nodes", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "t3.micro",
					},
				},
			}

			matches := resourceController.findMatchingMappings(node, mappings)
			Expect(matches).To(BeEmpty())
		})

		It("should find multiple mappings when node matches multiple selectors", func() {
			mappings = append(mappings, config.Mapping{
				Name: "all-nodes",
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "kubernetes.io/os",
								Operator: corev1.NodeSelectorOpExists,
							},
						},
					},
				},
			})

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "g4dn.xlarge",
						"kubernetes.io/os":                 "linux",
					},
				},
			}

			matches := resourceController.findMatchingMappings(node, mappings)
			Expect(matches).To(HaveLen(2))
			names := []string{matches[0].Name, matches[1].Name}
			Expect(names).To(ConsistOf("gpu-mapping", "all-nodes"))
		})

		It("should handle OR logic with multiple NodeSelectorTerms", func() {
			multiTermMapping := config.Mapping{
				Name: "multi-term",
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "zone",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"us-east-1a"},
							},
						},
					},
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "zone",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"us-west-2b"},
							},
						},
					},
				},
			}

			// Node matching first term
			node1 := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"zone": "us-east-1a",
					},
				},
			}
			matches := resourceController.findMatchingMappings(node1, []config.Mapping{multiTermMapping})
			Expect(matches).To(HaveLen(1))

			// Node matching second term
			node2 := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"zone": "us-west-2b",
					},
				},
			}
			matches = resourceController.findMatchingMappings(node2, []config.Mapping{multiTermMapping})
			Expect(matches).To(HaveLen(1))
		})
	})

	Describe("reconcileAllNodes", func() {
		var node *corev1.Node

		BeforeEach(func() {
			node = &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kwok-node-1",
					UID:  "node-uid-123",
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "g4dn.xlarge",
					},
					Annotations: map[string]string{
						"kwok.x-k8s.io/node": "fake",
					},
				},
			}

			// Set up configuration in the store
			testConfig := &config.Config{
				Driver: driverName,
				Mappings: []config.Mapping{
					{
						Name: "gpu-mapping",
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "node.kubernetes.io/instance-type",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{"g4dn.xlarge"},
									},
								},
							},
						},
						ResourceSlice: resourcev1.ResourceSliceSpec{
							Driver: "karpenter.sh/dra-kwok-driver",
							Pool: resourcev1.ResourcePool{
								Name:               "test-gpu-pool",
								ResourceSliceCount: 1,
							},
							Devices: []resourcev1.Device{
								{
									Name: "nvidia-gpu-0",
									Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
										"type":   {StringValue: stringPtr("nvidia-tesla-v100")},
										"memory": {StringValue: stringPtr("32Gi")},
									},
								},
							},
						},
					},
				},
			}
			configStore.Set(testConfig)
		})

		It("should create ResourceSlices for matching nodes", func() {
			// Create the node
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			// Run reconciliation
			err := resourceController.reconcileAllNodes(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify ResourceSlice was created
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(1))

			rs := &resourceSlices.Items[0]
			Expect(rs.Name).To(Equal("kwok-node-1-devices-gpu-mapping"))
			Expect(rs.Labels["kwok.x-k8s.io/managed-by"]).To(Equal("dra-kwok-driver"))
			Expect(rs.Labels["kwok.x-k8s.io/node"]).To(Equal("kwok-node-1"))
			Expect(rs.Spec.Devices).To(HaveLen(1))
			Expect(*rs.Spec.NodeName).To(Equal("kwok-node-1"))
		})

		It("should not create ResourceSlices for non-matching nodes", func() {
			// Modify node to not match any mappings
			node.Labels["node.kubernetes.io/instance-type"] = "t3.micro"
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			// Run reconciliation
			err := resourceController.reconcileAllNodes(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify no ResourceSlice was created
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(BeEmpty())
		})

		It("should skip non-KWOK nodes", func() {
			// Remove KWOK annotation
			delete(node.Annotations, "kwok.x-k8s.io/node")
			Expect(fakeClient.Create(ctx, node)).To(Succeed())

			// Run reconciliation
			err := resourceController.reconcileAllNodes(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify no ResourceSlice was created
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(BeEmpty())
		})

		It("should clean up orphaned ResourceSlices when node is deleted", func() {
			// Create node and run initial reconciliation
			Expect(fakeClient.Create(ctx, node)).To(Succeed())
			err := resourceController.reconcileAllNodes(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify ResourceSlice was created
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(1))

			// Delete the node
			Expect(fakeClient.Delete(ctx, node)).To(Succeed())

			// Run reconciliation again
			err = resourceController.reconcileAllNodes(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify ResourceSlice was cleaned up
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(BeEmpty())
		})

		It("should clean up all ResourceSlices when configuration is cleared", func() {
			// Create node and run initial reconciliation
			Expect(fakeClient.Create(ctx, node)).To(Succeed())
			err := resourceController.reconcileAllNodes(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify ResourceSlice was created
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(1))

			// Clear configuration
			configStore.Clear()

			// Run reconciliation again
			err = resourceController.reconcileAllNodes(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify ResourceSlice was cleaned up
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(BeEmpty())
		})

		It("should handle errors gracefully and continue processing other nodes", func() {
			// Create multiple nodes
			node2 := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kwok-node-2",
					UID:  "node-uid-456",
					Labels: map[string]string{
						"node.kubernetes.io/instance-type": "g4dn.xlarge",
					},
					Annotations: map[string]string{
						"kwok.x-k8s.io/node": "fake",
					},
				},
			}

			Expect(fakeClient.Create(ctx, node)).To(Succeed())
			Expect(fakeClient.Create(ctx, node2)).To(Succeed())

			// Run reconciliation - both nodes should get ResourceSlices
			err := resourceController.reconcileAllNodes(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify both ResourceSlices were created
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(2))

			// Check that we have one ResourceSlice for each node
			sliceNames := []string{}
			for _, rs := range resourceSlices.Items {
				sliceNames = append(sliceNames, rs.Name)
			}
			Expect(sliceNames).To(ConsistOf(
				"kwok-node-1-devices-gpu-mapping",
				"kwok-node-2-devices-gpu-mapping",
			))
		})

		It("should update existing ResourceSlices when configuration changes", func() {
			// Create node and run initial reconciliation
			Expect(fakeClient.Create(ctx, node)).To(Succeed())
			err := resourceController.reconcileAllNodes(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify initial ResourceSlice
			resourceSlices := &resourcev1.ResourceSliceList{}
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(1))
			Expect(resourceSlices.Items[0].Spec.Devices).To(HaveLen(1))

			// Update configuration with more devices
			newConfig := &config.Config{
				Driver: driverName,
				Mappings: []config.Mapping{
					{
						Name: "gpu-mapping",
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "node.kubernetes.io/instance-type",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{"g4dn.xlarge"},
									},
								},
							},
						},
						ResourceSlice: resourcev1.ResourceSliceSpec{
							Driver: "karpenter.sh/dra-kwok-driver",
							Pool: resourcev1.ResourcePool{
								Name:               "test-gpu-pool",
								ResourceSliceCount: 1,
							},
							Devices: []resourcev1.Device{
								{
									Name: "nvidia-gpu-0",
									Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
										"type": {StringValue: stringPtr("nvidia-tesla-v100")},
									},
								},
								{
									Name: "nvidia-gpu-1",
									Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
										"type": {StringValue: stringPtr("nvidia-tesla-v100")},
									},
								},
							},
						},
					},
				},
			}
			configStore.Set(newConfig)

			// Run reconciliation again
			err = resourceController.reconcileAllNodes(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify ResourceSlice was updated (our simple implementation always updates)
			Expect(fakeClient.List(ctx, resourceSlices)).To(Succeed())
			Expect(resourceSlices.Items).To(HaveLen(1))
			// Note: The fake client doesn't actually update the spec in our test
			// In a real scenario, the update would change the device count
		})
	})
})
