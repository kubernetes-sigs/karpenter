/*
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

package provisioning_test

import (
	"encoding/json"
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/test"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Provisioner/Provisioning", func() {
	var wellKnownLabelsV1Alpha5, wellKnownLabelsV1Beta1 sets.Set[string]
	BeforeEach(func() {
		wellKnownLabelsV1Alpha5 = v1alpha5.WellKnownLabels.Clone()
		wellKnownLabelsV1Beta1 = v1beta1.WellKnownLabels.Clone()
		DeferCleanup(func() {
			v1alpha5.WellKnownLabels = wellKnownLabelsV1Alpha5
			v1beta1.WellKnownLabels = wellKnownLabelsV1Beta1
		})
	})
	It("should provision nodes", func() {
		ExpectApplied(ctx, env.Client, test.Provisioner())
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		nodes := &v1.NodeList{}
		Expect(env.Client.List(ctx, nodes)).To(Succeed())
		Expect(len(nodes.Items)).To(Equal(1))
		ExpectScheduled(ctx, env.Client, pod)
	})
	It("should continue with provisioning when at least a provisioner doesn't have resolved instance types", func() {
		provNotDefined := test.Provisioner()
		provNotDefined.Spec.ProviderRef = nil
		ExpectApplied(ctx, env.Client, test.Provisioner(), provNotDefined)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		nodes := &v1.NodeList{}
		Expect(env.Client.List(ctx, nodes)).To(Succeed())
		Expect(len(nodes.Items)).To(Equal(1))
		ExpectScheduled(ctx, env.Client, pod)
	})
	It("should ignore provisioners that are deleting", func() {
		provisioner := test.Provisioner()
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectDeletionTimestampSet(ctx, env.Client, provisioner)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		nodes := &v1.NodeList{}
		Expect(env.Client.List(ctx, nodes)).To(Succeed())
		Expect(len(nodes.Items)).To(Equal(0))
		ExpectNotScheduled(ctx, env.Client, pod)
	})
	It("should provision nodes for pods with supported node selectors", func() {
		provisioner := test.Provisioner()
		schedulable := []*v1.Pod{
			// Constrained by provisioner
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name}}),
			// Constrained by zone
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyZone: "test-zone-1"}}),
			// Constrained by instanceType
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelInstanceTypeStable: "default-instance-type"}}),
			// Constrained by architecture
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelArchStable: "arm64"}}),
			// Constrained by operatingSystem
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelOSStable: string(v1.Linux)}}),
		}
		unschedulable := []*v1.Pod{
			// Ignored, matches another provisioner
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1alpha5.ProvisionerNameLabelKey: "unknown"}}),
			// Ignored, invalid zone
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyZone: "unknown"}}),
			// Ignored, invalid instance type
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelInstanceTypeStable: "unknown"}}),
			// Ignored, invalid architecture
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelArchStable: "unknown"}}),
			// Ignored, invalid operating system
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.LabelOSStable: "unknown"}}),
			// Ignored, invalid capacity type
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1alpha5.LabelCapacityType: "unknown"}}),
			// Ignored, label selector does not match
			test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{"foo": "bar"}}),
		}
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, schedulable...)
		for _, pod := range schedulable {
			ExpectScheduled(ctx, env.Client, pod)
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, unschedulable...)
		for _, pod := range unschedulable {
			ExpectNotScheduled(ctx, env.Client, pod)
		}
	})
	It("should provision nodes for pods with supported node affinities", func() {
		provisioner := test.Provisioner()
		schedulable := []*v1.Pod{
			// Constrained by provisioner
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1alpha5.ProvisionerNameLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{provisioner.Name}}}}),
			// Constrained by zone
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}}}}),
			// Constrained by instanceType
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"default-instance-type"}}}}),
			// Constrained by architecture
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{"arm64"}}}}),
			// Constrained by operatingSystem
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}}}}),
		}
		unschedulable := []*v1.Pod{
			// Ignored, matches another provisioner
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1alpha5.ProvisionerNameLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{"unknown"}}}}),
			// Ignored, invalid zone
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"unknown"}}}}),
			// Ignored, invalid instance type
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"unknown"}}}}),
			// Ignored, invalid architecture
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{"unknown"}}}}),
			// Ignored, invalid operating system
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{"unknown"}}}}),
			// Ignored, invalid capacity type
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: v1alpha5.LabelCapacityType, Operator: v1.NodeSelectorOpIn, Values: []string{"unknown"}}}}),
			// Ignored, label selector does not match
			test.UnschedulablePod(test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{Key: "foo", Operator: v1.NodeSelectorOpIn, Values: []string{"bar"}}}}),
		}
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, schedulable...)
		for _, pod := range schedulable {
			ExpectScheduled(ctx, env.Client, pod)
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, unschedulable...)
		for _, pod := range unschedulable {
			ExpectNotScheduled(ctx, env.Client, pod)
		}
	})
	It("should provision nodes for accelerators", func() {
		ExpectApplied(ctx, env.Client, test.Provisioner())
		pods := []*v1.Pod{
			test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Limits: v1.ResourceList{fake.ResourceGPUVendorA: resource.MustParse("1")}},
			}),
			test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Limits: v1.ResourceList{fake.ResourceGPUVendorB: resource.MustParse("1")}},
			}),
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		for _, pod := range pods {
			ExpectScheduled(ctx, env.Client, pod)
		}
	})
	It("should provision multiple nodes when maxPods is set", func() {
		// Kubelet is actually not observed here, the scheduler is relying on the
		// pods resource value which is statically set in the fake cloudprovider
		ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
			Kubelet: &v1alpha5.KubeletConfiguration{MaxPods: ptr.Int32(1)},
			Requirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"single-pod-instance-type"},
				},
			},
		}))
		pods := []*v1.Pod{
			test.UnschedulablePod(), test.UnschedulablePod(), test.UnschedulablePod(),
		}
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		nodes := &v1.NodeList{}
		Expect(env.Client.List(ctx, nodes)).To(Succeed())
		Expect(len(nodes.Items)).To(Equal(3))
		for _, pod := range pods {
			ExpectScheduled(ctx, env.Client, pod)
		}
	})
	It("should schedule all pods on one inflight node while node is in deleting state", func() {
		provisioner := test.Provisioner()
		its, err := cloudProvider.GetInstanceTypes(ctx, nodepoolutil.New(provisioner))
		Expect(err).To(BeNil())
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1.LabelInstanceTypeStable:       its[0].Name,
				},
				Finalizers: []string{v1alpha5.TerminationFinalizer},
			}},
		)
		ExpectApplied(ctx, env.Client, node, provisioner)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		// Schedule 3 pods to the node that currently exists
		for i := 0; i < 3; i++ {
			pod := test.UnschedulablePod()
			ExpectApplied(ctx, env.Client, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)
		}

		// Node shouldn't fully delete since it has a finalizer
		Expect(env.Client.Delete(ctx, node)).To(Succeed())
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		// Provision without a binding since some pods will already be bound
		// Should all schedule to the new node, ignoring the old node
		bindings := ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, test.UnschedulablePod(), test.UnschedulablePod())
		nodes := &v1.NodeList{}
		Expect(env.Client.List(ctx, nodes)).To(Succeed())
		Expect(len(nodes.Items)).To(Equal(2))

		// Scheduler should attempt to schedule all the pods to the new node
		for _, n := range bindings {
			Expect(n.Node.Name).ToNot(Equal(node.Name))
		}
	})
	It("should allow Provisioners to schedule pods that are using v1alpha5 labels but not v1beta1 labels", func() {
		// Inject a custom well-known label into v1alpha5 that doesn't exist in v1beta1
		v1alpha5.WellKnownLabels.Insert("karpenter.sh/custom-label")
		ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
			Requirements: []v1.NodeSelectorRequirement{
				{
					Key:      "karpenter.sh/custom-label",
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"custom-value"},
				},
			},
		}))
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectScheduled(ctx, env.Client, pod)
	})
	Context("Resource Limits", func() {
		It("should not schedule when limits are exceeded", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
				Limits: v1.ResourceList{v1.ResourceCPU: resource.MustParse("20")},
				Status: v1alpha5.ProvisionerStatus{
					Resources: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("100"),
					},
				},
			}))
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule if limits would be met", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
				Limits: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
			}))
			pod := test.UnschedulablePod(
				test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						// requires a 2 CPU node, but leaves room for overhead
						v1.ResourceCPU: resource.MustParse("1.75"),
					},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			// A 2 CPU node can be launched
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should partially schedule if limits would be exceeded", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
				Limits: v1.ResourceList{v1.ResourceCPU: resource.MustParse("3")},
			}))

			// prevent these pods from scheduling on the same node
			opts := test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "foo"},
				},
				PodAntiRequirements: []v1.PodAffinityTerm{
					{
						TopologyKey: v1.LabelHostname,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "foo",
							},
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("1.5"),
					}}}
			pods := []*v1.Pod{
				test.UnschedulablePod(opts),
				test.UnschedulablePod(opts),
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
			scheduledPodCount := 0
			unscheduledPodCount := 0
			pod0 := ExpectPodExists(ctx, env.Client, pods[0].Name, pods[0].Namespace)
			pod1 := ExpectPodExists(ctx, env.Client, pods[1].Name, pods[1].Namespace)
			if pod0.Spec.NodeName == "" {
				unscheduledPodCount++
			} else {
				scheduledPodCount++
			}
			if pod1.Spec.NodeName == "" {
				unscheduledPodCount++
			} else {
				scheduledPodCount++
			}
			Expect(scheduledPodCount).To(Equal(1))
			Expect(unscheduledPodCount).To(Equal(1))
		})
		It("should not schedule if limits would be exceeded", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
				Limits: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
			}))
			pod := test.UnschedulablePod(
				test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("2.1"),
					},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule if limits would be exceeded (GPU)", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
				Limits: v1.ResourceList{v1.ResourcePods: resource.MustParse("1")},
			}))
			pod := test.UnschedulablePod(
				test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
					Limits: v1.ResourceList{
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			// only available instance type has 2 GPUs which would exceed the limit
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule to a provisioner after a scheduling round if limits would be exceeded", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
				Limits: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
			}))
			pod := test.UnschedulablePod(
				test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						// requires a 2 CPU node, but leaves room for overhead
						v1.ResourceCPU: resource.MustParse("1.75"),
					},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			// A 2 CPU node can be launched
			ExpectScheduled(ctx, env.Client, pod)

			// This pod requests over the existing limit (would add to 3.5 CPUs) so this should fail
			pod = test.UnschedulablePod(
				test.PodOptions{ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						// requires a 2 CPU node, but leaves room for overhead
						v1.ResourceCPU: resource.MustParse("1.75"),
					},
				}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
	})
	Context("Daemonsets and Node Overhead", func() {
		It("should account for overhead", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(), test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				}},
			))
			pod := test.UnschedulablePod(
				test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)

			allocatable := instanceTypeMap[node.Labels[v1.LabelInstanceTypeStable]].Capacity
			Expect(*allocatable.Cpu()).To(Equal(resource.MustParse("4")))
			Expect(*allocatable.Memory()).To(Equal(resource.MustParse("4Gi")))
		})
		It("should account for overhead (with startup taint)", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{
				StartupTaints: []v1.Taint{{Key: "foo.com/taint", Effect: v1.TaintEffectNoSchedule}},
			})

			ExpectApplied(ctx, env.Client, provisioner, test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				}},
			))
			pod := test.UnschedulablePod(
				test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)

			allocatable := instanceTypeMap[node.Labels[v1.LabelInstanceTypeStable]].Capacity
			Expect(*allocatable.Cpu()).To(Equal(resource.MustParse("4")))
			Expect(*allocatable.Memory()).To(Equal(resource.MustParse("4Gi")))
		})
		It("should not schedule if overhead is too large", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(), test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("10000"), v1.ResourceMemory: resource.MustParse("10000Gi")}},
				}},
			))
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should account for overhead using daemonset pod spec instead of daemonset spec", func() {
			provisioner := test.Provisioner()
			// Create a daemonset with large resource requests
			daemonset := test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("4Gi")}},
				}},
			)
			ExpectApplied(ctx, env.Client, provisioner, daemonset)
			// Create the actual daemonSet pod with lower resource requests and expect to use the pod
			daemonsetPod := test.UnschedulablePod(
				test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "DaemonSet",
								Name:               daemonset.Name,
								UID:                daemonset.UID,
								Controller:         ptr.Bool(true),
								BlockOwnerDeletion: ptr.Bool(true),
							},
						},
					},
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				})
			ExpectApplied(ctx, env.Client, provisioner, daemonsetPod)
			ExpectReconcileSucceeded(ctx, daemonsetController, client.ObjectKeyFromObject(daemonset))
			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				NodeSelector:         map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)

			// We expect a smaller instance since the daemonset pod is smaller then daemonset spec
			allocatable := instanceTypeMap[node.Labels[v1.LabelInstanceTypeStable]].Capacity
			Expect(*allocatable.Cpu()).To(Equal(resource.MustParse("4")))
			Expect(*allocatable.Memory()).To(Equal(resource.MustParse("4Gi")))
		})
		It("should not schedule if resource requests are not defined and limits (requests) are too large", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(), test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{
						Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("10000"), v1.ResourceMemory: resource.MustParse("10000Gi")},
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					},
				}},
			))
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule based on the max resource requests of containers and initContainers", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(), test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{
						Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi")},
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2")},
					},
					InitImage: "pause",
					InitResourceRequirements: v1.ResourceRequirements{
						Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("10000"), v1.ResourceMemory: resource.MustParse("2Gi")},
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					},
				}},
			))
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			allocatable := instanceTypeMap[node.Labels[v1.LabelInstanceTypeStable]].Capacity
			Expect(*allocatable.Cpu()).To(Equal(resource.MustParse("4")))
			Expect(*allocatable.Memory()).To(Equal(resource.MustParse("4Gi")))
		})
		It("should not schedule if combined max resources are too large for any node", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(), test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{
						Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("10000"), v1.ResourceMemory: resource.MustParse("1Gi")},
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					},
					InitImage: "pause",
					InitResourceRequirements: v1.ResourceRequirements{
						Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("10000"), v1.ResourceMemory: resource.MustParse("10000Gi")},
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
					},
				}},
			))
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule if initContainer resources are too large", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(), test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					InitImage: "pause",
					InitResourceRequirements: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("10000"), v1.ResourceMemory: resource.MustParse("10000Gi")},
					},
				}},
			))
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should be able to schedule pods if resource requests and limits are not defined", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(), test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{}},
			))
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should ignore daemonsets without matching tolerations", func() {
			ExpectApplied(ctx, env.Client,
				test.Provisioner(test.ProvisionerOptions{Taints: []v1.Taint{{Key: "foo", Value: "bar", Effect: v1.TaintEffectNoSchedule}}}),
				test.DaemonSet(
					test.DaemonSetOptions{PodOptions: test.PodOptions{
						ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
					}},
				))
			pod := test.UnschedulablePod(
				test.PodOptions{
					Tolerations:          []v1.Toleration{{Operator: v1.TolerationOperator(v1.NodeSelectorOpExists)}},
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			allocatable := instanceTypeMap[node.Labels[v1.LabelInstanceTypeStable]].Capacity
			Expect(*allocatable.Cpu()).To(Equal(resource.MustParse("2")))
			Expect(*allocatable.Memory()).To(Equal(resource.MustParse("2Gi")))
		})
		It("should ignore daemonsets with an invalid selector", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(), test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					NodeSelector:         map[string]string{"node": "invalid"},
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				}},
			))
			pod := test.UnschedulablePod(
				test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			allocatable := instanceTypeMap[node.Labels[v1.LabelInstanceTypeStable]].Capacity
			Expect(*allocatable.Cpu()).To(Equal(resource.MustParse("2")))
			Expect(*allocatable.Memory()).To(Equal(resource.MustParse("2Gi")))
		})
		It("should account daemonsets with NotIn operator and unspecified key", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(), test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					NodeRequirements:     []v1.NodeSelectorRequirement{{Key: "foo", Operator: v1.NodeSelectorOpNotIn, Values: []string{"bar"}}},
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				}},
			))
			pod := test.UnschedulablePod(
				test.PodOptions{
					NodeRequirements:     []v1.NodeSelectorRequirement{{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-2"}}},
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			allocatable := instanceTypeMap[node.Labels[v1.LabelInstanceTypeStable]].Capacity
			Expect(*allocatable.Cpu()).To(Equal(resource.MustParse("4")))
			Expect(*allocatable.Memory()).To(Equal(resource.MustParse("4Gi")))
		})
		It("should account for daemonset spec affinity", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{
				Labels: map[string]string{
					"foo": "voo",
				},
				Limits: v1.ResourceList{
					v1.ResourceCPU: resource.MustParse("2"),
				},
			})
			provisionerDaemonset := test.Provisioner(test.ProvisionerOptions{
				Labels: map[string]string{
					"foo": "bar",
				},
			})
			// Create a daemonset with large resource requests
			daemonset := test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{
							Key:      "foo",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"bar"},
						},
					},
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("4Gi")}},
				}},
			)
			ExpectApplied(ctx, env.Client, provisionerDaemonset, daemonset)
			// Create the actual daemonSet pod with lower resource requests and expect to use the pod
			daemonsetPod := test.UnschedulablePod(
				test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion:         "apps/v1",
								Kind:               "DaemonSet",
								Name:               daemonset.Name,
								UID:                daemonset.UID,
								Controller:         ptr.Bool(true),
								BlockOwnerDeletion: ptr.Bool(true),
							},
						},
					},
					NodeRequirements: []v1.NodeSelectorRequirement{
						{
							Key:      metav1.ObjectNameField,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"node-name"},
						},
					},
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4"), v1.ResourceMemory: resource.MustParse("4Gi")}},
				})
			ExpectApplied(ctx, env.Client, daemonsetPod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, daemonsetPod)
			ExpectReconcileSucceeded(ctx, daemonsetController, client.ObjectKeyFromObject(daemonset))

			//Deploy pod
			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
				NodeSelector: map[string]string{
					"foo": "voo",
				},
			})
			ExpectApplied(ctx, env.Client, provisioner, pod)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
	})
	Context("Annotations", func() {
		It("should annotate nodes", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{
				Annotations: map[string]string{v1alpha5.DoNotConsolidateNodeAnnotationKey: "true"},
			})
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Annotations).To(HaveKeyWithValue(v1alpha5.DoNotConsolidateNodeAnnotationKey, "true"))
		})
	})
	Context("Labels", func() {
		It("should label nodes", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{
				Labels: map[string]string{"test-key-1": "test-value-1"},
				Requirements: []v1.NodeSelectorRequirement{
					{Key: "test-key-2", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value-2"}},
					{Key: "test-key-3", Operator: v1.NodeSelectorOpNotIn, Values: []string{"test-value-3"}},
					{Key: "test-key-4", Operator: v1.NodeSelectorOpLt, Values: []string{"4"}},
					{Key: "test-key-5", Operator: v1.NodeSelectorOpGt, Values: []string{"5"}},
					{Key: "test-key-6", Operator: v1.NodeSelectorOpExists},
					{Key: "test-key-7", Operator: v1.NodeSelectorOpDoesNotExist},
				},
			})
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, provisioner.Name))
			Expect(node.Labels).To(HaveKeyWithValue("test-key-1", "test-value-1"))
			Expect(node.Labels).To(HaveKeyWithValue("test-key-2", "test-value-2"))
			Expect(node.Labels).To(And(HaveKey("test-key-3"), Not(HaveValue(Equal("test-value-3")))))
			Expect(node.Labels).To(And(HaveKey("test-key-4"), Not(HaveValue(Equal("test-value-4")))))
			Expect(node.Labels).To(And(HaveKey("test-key-5"), Not(HaveValue(Equal("test-value-5")))))
			Expect(node.Labels).To(HaveKey("test-key-6"))
			Expect(node.Labels).ToNot(HaveKey("test-key-7"))
		})
		It("should label nodes with labels in the LabelDomainExceptions list", func() {
			for domain := range v1alpha5.LabelDomainExceptions {
				provisioner := test.Provisioner(test.ProvisionerOptions{Labels: map[string]string{domain + "/test": "test-value"}})
				ExpectApplied(ctx, env.Client, provisioner)
				pod := test.UnschedulablePod(
					test.PodOptions{
						NodeRequirements: []v1.NodeSelectorRequirement{{Key: domain + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test-value"}}},
					},
				)
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				node := ExpectScheduled(ctx, env.Client, pod)
				Expect(node.Labels).To(HaveKeyWithValue(domain+"/test", "test-value"))
			}
		})

	})
	Context("Taints", func() {
		It("should schedule pods that tolerate taints", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{Taints: []v1.Taint{{Key: "nvidia.com/gpu", Value: "true", Effect: v1.TaintEffectNoSchedule}}})
			ExpectApplied(ctx, env.Client, provisioner)
			pods := []*v1.Pod{
				test.UnschedulablePod(
					test.PodOptions{Tolerations: []v1.Toleration{
						{
							Key:      "nvidia.com/gpu",
							Operator: v1.TolerationOpEqual,
							Value:    "true",
							Effect:   v1.TaintEffectNoSchedule,
						},
					}}),
				test.UnschedulablePod(
					test.PodOptions{Tolerations: []v1.Toleration{
						{
							Key:      "nvidia.com/gpu",
							Operator: v1.TolerationOpExists,
							Effect:   v1.TaintEffectNoSchedule,
						},
					}}),
				test.UnschedulablePod(
					test.PodOptions{Tolerations: []v1.Toleration{
						{
							Key:      "nvidia.com/gpu",
							Operator: v1.TolerationOpExists,
						},
					}}),
				test.UnschedulablePod(
					test.PodOptions{Tolerations: []v1.Toleration{
						{
							Operator: v1.TolerationOpExists,
						},
					}}),
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
			for _, pod := range pods {
				ExpectScheduled(ctx, env.Client, pod)
			}
		})
	})
	Context("Machine Creation", func() {
		It("should create a machine request with expected requirements", func() {
			provisioner := test.Provisioner()
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			ExpectNodeClaimRequirements(cloudProvider.CreateCalls[0],
				v1.NodeSelectorRequirement{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   lo.Keys(instanceTypeMap),
				},
				v1.NodeSelectorRequirement{
					Key:      v1alpha5.ProvisionerNameLabelKey,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{provisioner.Name},
				},
			)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should create a machine request with additional expected requirements", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      "custom-requirement-key",
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"value"},
					},
					{
						Key:      "custom-requirement-key2",
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"value"},
					},
				},
			})
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			ExpectNodeClaimRequirements(cloudProvider.CreateCalls[0],
				v1.NodeSelectorRequirement{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   lo.Keys(instanceTypeMap),
				},
				v1.NodeSelectorRequirement{
					Key:      v1alpha5.ProvisionerNameLabelKey,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{provisioner.Name},
				},
				v1.NodeSelectorRequirement{
					Key:      "custom-requirement-key",
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"value"},
				},
				v1.NodeSelectorRequirement{
					Key:      "custom-requirement-key2",
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"value"},
				},
			)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should create a machine request restricting instance types on architecture", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      v1.LabelArchStable,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"arm64"},
					},
				},
			})
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))

			// Expect a more restricted set of instance types
			ExpectNodeClaimRequirements(cloudProvider.CreateCalls[0],
				v1.NodeSelectorRequirement{
					Key:      v1.LabelArchStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"arm64"},
				},
				v1.NodeSelectorRequirement{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"arm-instance-type"},
				},
			)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should create a machine request restricting instance types on operating system", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      v1.LabelOSStable,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"ios"},
					},
				},
			})
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))

			// Expect a more restricted set of instance types
			ExpectNodeClaimRequirements(cloudProvider.CreateCalls[0],
				v1.NodeSelectorRequirement{
					Key:      v1.LabelOSStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"ios"},
				},
				v1.NodeSelectorRequirement{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"arm-instance-type"},
				},
			)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should create a machine request restricting instance types based on pod resource requests", func() {
			provisioner := test.Provisioner()
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
					Limits: v1.ResourceList{
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
				},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))

			// Expect a more restricted set of instance types
			ExpectNodeClaimRequirements(cloudProvider.CreateCalls[0],
				v1.NodeSelectorRequirement{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"gpu-vendor-instance-type"},
				},
			)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should create a machine request with the correct owner reference", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{})
			ExpectApplied(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			Expect(cloudProvider.CreateCalls[0].OwnerReferences).To(ContainElement(
				metav1.OwnerReference{
					APIVersion:         "karpenter.sh/v1alpha5",
					Kind:               "Provisioner",
					Name:               provisioner.Name,
					UID:                provisioner.UID,
					BlockOwnerDeletion: lo.ToPtr(true),
				},
			))
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should create a machine request propagating the provider reference", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
				ProviderRef: &v1alpha5.MachineTemplateRef{
					APIVersion: "cloudprovider.karpenter.sh/v1alpha1",
					Kind:       "CloudProvider",
					Name:       "default",
				},
			}))
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			Expect(cloudProvider.CreateCalls[0].Spec.NodeClassRef).To(Equal(
				&v1beta1.NodeClassReference{
					APIVersion: "cloudprovider.karpenter.sh/v1alpha1",
					Kind:       "CloudProvider",
					Name:       "default",
				},
			))
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should create a machine request with the karpenter.sh/compatibility/provider annotation", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
				Provider: map[string]string{
					"providerField1": "value",
					"providerField2": "value",
				},
			}))
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			Expect(cloudProvider.CreateCalls[0].Annotations).To(HaveKey(v1alpha5.ProviderCompatabilityAnnotationKey))

			// Deserialize the provider into the expected format
			provider := map[string]string{}
			Expect(json.Unmarshal([]byte(cloudProvider.CreateCalls[0].Annotations[v1alpha5.ProviderCompatabilityAnnotationKey]), &provider)).To(Succeed())
			Expect(provider).To(HaveKeyWithValue("providerField1", "value"))
			Expect(provider).To(HaveKeyWithValue("providerField2", "value"))
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should create a machine with resource requests", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{
				Provider: map[string]string{
					"providerField1": "value",
					"providerField2": "value",
				},
			}))
			pod := test.UnschedulablePod(
				test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:          resource.MustParse("1"),
							v1.ResourceMemory:       resource.MustParse("1Mi"),
							fake.ResourceGPUVendorA: resource.MustParse("1"),
						},
						Limits: v1.ResourceList{
							fake.ResourceGPUVendorA: resource.MustParse("1"),
						},
					},
				})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			Expect(cloudProvider.CreateCalls[0].Spec.Resources.Requests).To(HaveLen(4))
			ExpectNodeClaimRequests(cloudProvider.CreateCalls[0], v1.ResourceList{
				v1.ResourceCPU:          resource.MustParse("1"),
				v1.ResourceMemory:       resource.MustParse("1Mi"),
				fake.ResourceGPUVendorA: resource.MustParse("1"),
				v1.ResourcePods:         resource.MustParse("1"),
			})
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should create a machine with resource requests with daemon overhead", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner(), test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Mi")}},
				}},
			))
			pod := test.UnschedulablePod(
				test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Mi")}},
				},
			)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			Expect(cloudProvider.CreateCalls).To(HaveLen(1))
			ExpectNodeClaimRequests(cloudProvider.CreateCalls[0], v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("2Mi"),
				v1.ResourcePods:   resource.MustParse("2"),
			})
			ExpectScheduled(ctx, env.Client, pod)
		})
	})
	Context("Volume Topology Requirements", func() {
		var storageClass *storagev1.StorageClass
		BeforeEach(func() {
			storageClass = test.StorageClass(test.StorageClassOptions{Zones: []string{"test-zone-2", "test-zone-3"}})
		})
		It("should not schedule if invalid pvc", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner())
			pod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{"invalid"},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule with an empty storage class", func() {
			storageClass := ""
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{StorageClassName: &storageClass})
			ExpectApplied(ctx, env.Client, test.Provisioner(), persistentVolumeClaim)
			pod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{persistentVolumeClaim.Name},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule valid pods when a pod with an invalid pvc is encountered (pvc)", func() {
			ExpectApplied(ctx, env.Client, test.Provisioner())
			invalidPod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{"invalid"},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, invalidPod)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, invalidPod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule valid pods when a pod with an invalid pvc is encountered (storage class)", func() {
			invalidStorageClass := "invalid-storage-class"
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{StorageClassName: &invalidStorageClass})
			ExpectApplied(ctx, env.Client, test.Provisioner(), persistentVolumeClaim)
			invalidPod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{persistentVolumeClaim.Name},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, invalidPod)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, invalidPod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule valid pods when a pod with an invalid pvc is encountered (volume name)", func() {
			invalidVolumeName := "invalid-volume-name"
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{VolumeName: invalidVolumeName})
			ExpectApplied(ctx, env.Client, test.Provisioner(), persistentVolumeClaim)
			invalidPod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{persistentVolumeClaim.Name},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, invalidPod)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, invalidPod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should schedule to storage class zones if volume does not exist", func() {
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{StorageClassName: &storageClass.Name})
			ExpectApplied(ctx, env.Client, test.Provisioner(), storageClass, persistentVolumeClaim)
			pod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{persistentVolumeClaim.Name},
				NodeRequirements: []v1.NodeSelectorRequirement{{
					Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-3"},
				}},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should schedule to storage class zones if volume does not exist (ephemeral volume)", func() {
			pod := test.UnschedulablePod(test.PodOptions{
				EphemeralVolumeTemplates: []test.EphemeralVolumeTemplateOptions{
					{
						StorageClassName: &storageClass.Name,
					},
				},
				NodeRequirements: []v1.NodeSelectorRequirement{{
					Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-3"},
				}},
			})
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-%s", pod.Name, pod.Spec.Volumes[0].Name),
				},
				StorageClassName: &storageClass.Name,
			})
			ExpectApplied(ctx, env.Client, test.Provisioner(), storageClass, persistentVolumeClaim)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should not schedule if storage class zones are incompatible", func() {
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{StorageClassName: &storageClass.Name})
			ExpectApplied(ctx, env.Client, test.Provisioner(), storageClass, persistentVolumeClaim)
			pod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{persistentVolumeClaim.Name},
				NodeRequirements: []v1.NodeSelectorRequirement{{
					Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"},
				}},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule if storage class zones are incompatible (ephemeral volume)", func() {
			pod := test.UnschedulablePod(test.PodOptions{
				EphemeralVolumeTemplates: []test.EphemeralVolumeTemplateOptions{
					{
						StorageClassName: &storageClass.Name,
					},
				},
				NodeRequirements: []v1.NodeSelectorRequirement{{
					Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"},
				}},
			})
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-%s", pod.Name, pod.Spec.Volumes[0].Name),
				},
				StorageClassName: &storageClass.Name,
			})
			ExpectApplied(ctx, env.Client, test.Provisioner(), storageClass, persistentVolumeClaim)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should schedule to volume zones if volume already bound", func() {
			persistentVolume := test.PersistentVolume(test.PersistentVolumeOptions{Zones: []string{"test-zone-3"}})
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{VolumeName: persistentVolume.Name, StorageClassName: &storageClass.Name})
			ExpectApplied(ctx, env.Client, test.Provisioner(), storageClass, persistentVolumeClaim, persistentVolume)
			pod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{persistentVolumeClaim.Name},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should schedule to volume zones if volume already bound (ephemeral volume)", func() {
			pod := test.UnschedulablePod(test.PodOptions{
				EphemeralVolumeTemplates: []test.EphemeralVolumeTemplateOptions{
					{
						StorageClassName: &storageClass.Name,
					},
				},
			})
			persistentVolume := test.PersistentVolume(test.PersistentVolumeOptions{Zones: []string{"test-zone-3"}})
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-%s", pod.Name, pod.Spec.Volumes[0].Name),
				},
				VolumeName:       persistentVolume.Name,
				StorageClassName: &storageClass.Name,
			})
			ExpectApplied(ctx, env.Client, test.Provisioner(), storageClass, pod, persistentVolumeClaim, persistentVolume)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
		It("should not schedule if volume zones are incompatible", func() {
			persistentVolume := test.PersistentVolume(test.PersistentVolumeOptions{Zones: []string{"test-zone-3"}})
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{VolumeName: persistentVolume.Name, StorageClassName: &storageClass.Name})
			ExpectApplied(ctx, env.Client, test.Provisioner(), storageClass, persistentVolumeClaim, persistentVolume)
			pod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{persistentVolumeClaim.Name},
				NodeRequirements: []v1.NodeSelectorRequirement{{
					Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"},
				}},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not schedule if volume zones are incompatible (ephemeral volume)", func() {
			pod := test.UnschedulablePod(test.PodOptions{
				EphemeralVolumeTemplates: []test.EphemeralVolumeTemplateOptions{
					{
						StorageClassName: &storageClass.Name,
					},
				},
				NodeRequirements: []v1.NodeSelectorRequirement{{
					Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"},
				}},
			})
			persistentVolume := test.PersistentVolume(test.PersistentVolumeOptions{Zones: []string{"test-zone-3"}})
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-%s", pod.Name, pod.Spec.Volumes[0].Name),
				},
				VolumeName:       persistentVolume.Name,
				StorageClassName: &storageClass.Name,
			})
			ExpectApplied(ctx, env.Client, test.Provisioner(), storageClass, pod, persistentVolumeClaim, persistentVolume)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should not relax an added volume topology zone node-selector away", func() {
			persistentVolume := test.PersistentVolume(test.PersistentVolumeOptions{Zones: []string{"test-zone-3"}})
			persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{VolumeName: persistentVolume.Name, StorageClassName: &storageClass.Name})
			ExpectApplied(ctx, env.Client, test.Provisioner(), storageClass, persistentVolumeClaim, persistentVolume)

			pod := test.UnschedulablePod(test.PodOptions{
				PersistentVolumeClaims: []string{persistentVolumeClaim.Name},
				NodeRequirements: []v1.NodeSelectorRequirement{
					{
						Key:      "example.com/label",
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"unsupported"},
					},
				},
			})

			// Add the second capacity type that is OR'd with the first. Previously we only added the volume topology requirement
			// to a single node selector term which would sometimes get relaxed away.  Now we add it to all of them to AND
			// it with each existing term.
			pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = append(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms,
				v1.NodeSelectorTerm{
					MatchExpressions: []v1.NodeSelectorRequirement{
						{
							Key:      v1alpha5.LabelCapacityType,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{v1alpha5.CapacityTypeOnDemand},
						},
					},
				})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-3"))
		})
	})
	Context("Preferential Fallback", func() {
		Context("Required", func() {
			It("should not relax the final term", func() {
				pod := test.UnschedulablePod()
				pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{
					{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}}, // Should not be relaxed
					}},
				}}}}
				// Don't relax
				ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{Requirements: []v1.NodeSelectorRequirement{{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}}}}))
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectNotScheduled(ctx, env.Client, pod)
			})
			It("should relax multiple terms", func() {
				pod := test.UnschedulablePod()
				pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{NodeSelectorTerms: []v1.NodeSelectorTerm{
					{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
					}},
					{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
					}},
					{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}},
					}},
					{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-2"}}, // OR operator, never get to this one
					}},
				}}}}
				// Success
				ExpectApplied(ctx, env.Client, test.Provisioner())
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				node := ExpectScheduled(ctx, env.Client, pod)
				Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-1"))
			})
		})
		Context("Preferences", func() {
			It("should relax all node affinity terms", func() {
				pod := test.UnschedulablePod()
				pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{
					{
						Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
							{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
						}},
					},
					{
						Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
							{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
						}},
					},
				}}}
				// Success
				ExpectApplied(ctx, env.Client, test.Provisioner())
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
			})
			It("should relax to use lighter weights", func() {
				pod := test.UnschedulablePod()
				pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{
					{
						Weight: 100, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
							{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-3"}},
						}},
					},
					{
						Weight: 50, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
							{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-2"}},
						}},
					},
					{
						Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{ // OR operator, never get to this one
							{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1"}},
						}},
					},
				}}}
				// Success
				ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{Requirements: []v1.NodeSelectorRequirement{{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-2"}}}}))
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				node := ExpectScheduled(ctx, env.Client, pod)
				Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-2"))
			})
			It("should tolerate PreferNoSchedule taint only after trying to relax Affinity terms", func() {
				pod := test.UnschedulablePod()
				pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{
					{
						Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
							{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
						}},
					},
					{
						Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
							{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"invalid"}},
						}},
					},
				}}}
				// Success
				ExpectApplied(ctx, env.Client, test.Provisioner(test.ProvisionerOptions{Taints: []v1.Taint{{Key: "foo", Value: "bar", Effect: v1.TaintEffectPreferNoSchedule}}}))
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				node := ExpectScheduled(ctx, env.Client, pod)
				Expect(node.Spec.Taints).To(ContainElement(v1.Taint{Key: "foo", Value: "bar", Effect: v1.TaintEffectPreferNoSchedule}))
			})
		})
	})
	Context("Multiple Provisioners", func() {
		It("should schedule to an explicitly selected provisioner", func() {
			provisioner := test.Provisioner()
			ExpectApplied(ctx, env.Client, provisioner, test.Provisioner())
			pod := test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels[v1alpha5.ProvisionerNameLabelKey]).To(Equal(provisioner.Name))
		})
		It("should schedule to a provisioner by labels", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{Labels: map[string]string{"foo": "bar"}})
			ExpectApplied(ctx, env.Client, provisioner, test.Provisioner())
			pod := test.UnschedulablePod(test.PodOptions{NodeSelector: provisioner.Spec.Labels})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels[v1alpha5.ProvisionerNameLabelKey]).To(Equal(provisioner.Name))
		})
		It("should not match provisioner with PreferNoSchedule taint when other provisioner match", func() {
			provisioner := test.Provisioner(test.ProvisionerOptions{Taints: []v1.Taint{{Key: "foo", Value: "bar", Effect: v1.TaintEffectPreferNoSchedule}}})
			ExpectApplied(ctx, env.Client, provisioner, test.Provisioner())
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels[v1alpha5.ProvisionerNameLabelKey]).ToNot(Equal(provisioner.Name))
		})
		Context("Weighted Provisioners", func() {
			It("should schedule to the provisioner with the highest priority always", func() {
				provisioners := []client.Object{
					test.Provisioner(),
					test.Provisioner(test.ProvisionerOptions{Weight: ptr.Int32(20)}),
					test.Provisioner(test.ProvisionerOptions{Weight: ptr.Int32(100)}),
				}
				ExpectApplied(ctx, env.Client, provisioners...)
				pods := []*v1.Pod{
					test.UnschedulablePod(), test.UnschedulablePod(), test.UnschedulablePod(),
				}
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
				for _, pod := range pods {
					node := ExpectScheduled(ctx, env.Client, pod)
					Expect(node.Labels[v1alpha5.ProvisionerNameLabelKey]).To(Equal(provisioners[2].GetName()))
				}
			})
			It("should schedule to explicitly selected provisioner even if other provisioners are higher priority", func() {
				targetedProvisioner := test.Provisioner()
				provisioners := []client.Object{
					targetedProvisioner,
					test.Provisioner(test.ProvisionerOptions{Weight: ptr.Int32(20)}),
					test.Provisioner(test.ProvisionerOptions{Weight: ptr.Int32(100)}),
				}
				ExpectApplied(ctx, env.Client, provisioners...)
				pod := test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1alpha5.ProvisionerNameLabelKey: targetedProvisioner.Name}})
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				node := ExpectScheduled(ctx, env.Client, pod)
				Expect(node.Labels[v1alpha5.ProvisionerNameLabelKey]).To(Equal(targetedProvisioner.Name))
			})
		})
	})
})
