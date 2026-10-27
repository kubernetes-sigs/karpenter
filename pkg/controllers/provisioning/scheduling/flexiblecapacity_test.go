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

package scheduling_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

const hugePages2Mi corev1.ResourceName = "hugepages-2Mi"

// hugePagesPodOptions returns pod options that request hugepages and memory. Kubernetes requires hugepages requests
// to equal their limits, and a pod that requests hugepages must also request cpu or memory.
func hugePagesPodOptions(hugePages, memory string) test.PodOptions {
	requests := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)}
	limits := corev1.ResourceList{}
	if hugePages != "" {
		requests[hugePages2Mi] = resource.MustParse(hugePages)
		limits[hugePages2Mi] = resource.MustParse(hugePages)
	}
	return test.PodOptions{ResourceRequirements: corev1.ResourceRequirements{Requests: requests, Limits: limits}}
}

func flexibleInstanceType(name, memory, maxHugePages string) *cloudprovider.InstanceType {
	it := fake.NewInstanceType(name, fake.WithResources(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse(memory),
		corev1.ResourcePods:   resource.MustParse("10"),
	}))
	it.ApplyMaxCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse(maxHugePages)})
	return it
}

func expectSingleNodeClaim() *v1.NodeClaim {
	GinkgoHelper()
	nodeClaims := &v1.NodeClaimList{}
	Expect(env.Client.List(ctx, nodeClaims)).To(Succeed())
	Expect(nodeClaims.Items).To(HaveLen(1))
	return &nodeClaims.Items[0]
}

// instanceTypeOptions returns the instance types that a NodeClaim can launch as
func instanceTypeOptions(nodeClaim *v1.NodeClaim) []string {
	GinkgoHelper()
	requirement, ok := lo.Find(nodeClaim.Spec.Requirements, func(r v1.NodeSelectorRequirementWithMinValues) bool {
		return r.Key == corev1.LabelInstanceTypeStable
	})
	Expect(ok).To(BeTrue())
	return requirement.Values
}

func quantity(s string) int64 {
	q := resource.MustParse(s)
	return q.Value()
}

var _ = Describe("Flexible Capacity", func() {
	var nodePool *v1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool()
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			flexibleInstanceType("small", "4Gi", "8Gi"),
			flexibleInstanceType("large", "16Gi", "8Gi"),
		}
	})
	It("should give the node the summed hugepages requests of its pods", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pods := test.UnschedulablePods(hugePagesPodOptions("512Mi", "256Mi"), 2)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		node := ExpectScheduled(ctx, env.Client, pods[0])
		Expect(ExpectScheduled(ctx, env.Client, pods[1]).Name).To(Equal(node.Name))

		nodeClaim := expectSingleNodeClaim()
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"1Gi"}`))
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1alpha1.CapacityOverlayAppliedAnnotationKey, "true"))
		Expect(nodeClaim.Spec.Resources.Requests).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("1Gi")))
		// The node gets the chosen amount, not the ceiling, and its memory is reduced by it
		Expect(node.Status.Allocatable).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("1Gi")))
		Expect(node.Status.Allocatable.Memory().Value()).To(Equal(quantity("4Gi") - quantity("10Mi") - quantity("1Gi")))
	})
	It("should not annotate a NodeClaim whose pods request no flexible resources", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(hugePagesPodOptions("", "1Gi"))
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)

		nodeClaim := expectSingleNodeClaim()
		Expect(nodeClaim.Annotations).ToNot(HaveKey(v1.FlexibleCapacityAnnotationKey))
		Expect(nodeClaim.Spec.Resources.Requests).ToNot(HaveKey(hugePages2Mi))
		Expect(node.Status.Allocatable).ToNot(HaveKey(hugePages2Mi))
		Expect(node.Status.Allocatable.Memory().Value()).To(Equal(quantity("4Gi") - quantity("10Mi")))
	})
	It("should not schedule a pod that requests more than the ceiling", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(hugePagesPodOptions("10Gi", "256Mi"))
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
	})
	It("should choose a larger instance type when memory and hugepages together do not fit", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(hugePagesPodOptions("2Gi", "3Gi"))
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, "large"))
	})
	It("should include the largest daemonset hugepages overhead in the chosen amount", func() {
		ds := test.DaemonSet(test.DaemonSetOptions{PodOptions: hugePagesPodOptions("256Mi", "64Mi")})
		ExpectApplied(ctx, env.Client, nodePool, ds)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(ds), ds)).To(Succeed())
		pod := test.UnschedulablePod(hugePagesPodOptions("512Mi", "256Mi"))
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectScheduled(ctx, env.Client, pod)

		nodeClaim := expectSingleNodeClaim()
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"768Mi"}`))
		Expect(nodeClaim.Spec.Resources.Requests).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("768Mi")))
	})
	Context("Daemonsets on some instance types", func() {
		// largeOnlyDaemonSet returns a daemonset that runs only on the large instance type
		largeOnlyDaemonSet := func(hugePages string) *appsv1.DaemonSet {
			podOptions := hugePagesPodOptions(hugePages, "64Mi")
			podOptions.NodeSelector = map[string]string{corev1.LabelInstanceTypeStable: "large"}
			return test.DaemonSet(test.DaemonSetOptions{PodOptions: podOptions})
		}
		It("should not launch an instance type that cannot hold another instance type's daemonset hugepages", func() {
			// The small instance type fits the pod alone, but not with the 3Gi that the large instance type's
			// daemonset adds to the chosen amount
			ds := largeOnlyDaemonSet("3Gi")
			ExpectApplied(ctx, env.Client, nodePool, ds)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(ds), ds)).To(Succeed())
			pod := test.UnschedulablePod(hugePagesPodOptions("1Gi", "2Gi"))
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, "large"))

			nodeClaim := expectSingleNodeClaim()
			Expect(instanceTypeOptions(nodeClaim)).To(ConsistOf("large"))
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"4Gi"}`))
		})
		It("should not launch an instance type whose ceiling is below another instance type's daemonset hugepages", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				flexibleInstanceType("small", "16Gi", "2Gi"),
				flexibleInstanceType("large", "32Gi", "8Gi"),
			}
			ds := largeOnlyDaemonSet("3Gi")
			ExpectApplied(ctx, env.Client, nodePool, ds)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(ds), ds)).To(Succeed())
			pod := test.UnschedulablePod(hugePagesPodOptions("1Gi", "256Mi"))
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, "large"))

			nodeClaim := expectSingleNodeClaim()
			Expect(instanceTypeOptions(nodeClaim)).To(ConsistOf("large"))
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"4Gi"}`))
		})
		It("should keep fixed instance types when only a daemonset on an excluded instance type requests the resource", func() {
			fixed := fake.NewInstanceType("fixed", fake.WithResources(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
				corev1.ResourcePods:   resource.MustParse("10"),
			}))
			fixed.ApplyCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")})
			cloudProvider.InstanceTypes = append(cloudProvider.InstanceTypes, fixed)
			ds := largeOnlyDaemonSet("256Mi")
			ExpectApplied(ctx, env.Client, nodePool, ds)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(ds), ds)).To(Succeed())
			// The pod cannot run on the large instance type, so the daemonset's request does not apply to it
			podOptions := hugePagesPodOptions("", "256Mi")
			podOptions.NodeRequirements = []corev1.NodeSelectorRequirement{{
				Key:      corev1.LabelInstanceTypeStable,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"small", "fixed"},
			}}
			pod := test.UnschedulablePod(podOptions)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, "fixed"))
			Expect(expectSingleNodeClaim().Annotations).ToNot(HaveKey(v1.FlexibleCapacityAnnotationKey))
		})
	})
	It("should not add pods to a launched node beyond its chosen amount", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		first := test.UnschedulablePod(hugePagesPodOptions("1Gi", "256Mi"))
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, first)
		firstNode := ExpectScheduled(ctx, env.Client, first)

		// A pod without hugepages still fits in the first node's remaining memory
		second := test.UnschedulablePod(hugePagesPodOptions("", "256Mi"))
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, second)
		Expect(ExpectScheduled(ctx, env.Client, second).Name).To(Equal(firstNode.Name))

		// The first node has no hugepages left, so a pod that needs them gets a new node
		third := test.UnschedulablePod(hugePagesPodOptions("512Mi", "256Mi"))
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, third)
		Expect(ExpectScheduled(ctx, env.Client, third).Name).ToNot(Equal(firstNode.Name))
	})
	Context("Mixed fixed and flexible instance types", func() {
		BeforeEach(func() {
			// The fixed instance type is the cheapest, and always has 2Gi of hugepages
			fixed := fake.NewInstanceType("fixed", fake.WithResources(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
				corev1.ResourcePods:   resource.MustParse("10"),
			}))
			fixed.ApplyCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")})
			cloudProvider.InstanceTypes = append(cloudProvider.InstanceTypes, fixed)
		})
		It("should only launch as flexible instance types when a pod requests the resource", func() {
			ExpectApplied(ctx, env.Client, nodePool)
			pod := test.UnschedulablePod(hugePagesPodOptions("1Gi", "256Mi"))
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels[corev1.LabelInstanceTypeStable]).ToNot(Equal("fixed"))

			nodeClaim := expectSingleNodeClaim()
			Expect(instanceTypeOptions(nodeClaim)).To(ConsistOf("small", "large"))
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"1Gi"}`))
		})
		It("should keep fixed instance types when no pod requests the resource", func() {
			ExpectApplied(ctx, env.Client, nodePool)
			pod := test.UnschedulablePod(hugePagesPodOptions("", "256Mi"))
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, "fixed"))
			Expect(expectSingleNodeClaim().Annotations).ToNot(HaveKey(v1.FlexibleCapacityAnnotationKey))
		})
	})
	It("should keep today's behavior for fixed capacity overlays", func() {
		fixed := fake.NewInstanceType("fixed", fake.WithResources(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
			corev1.ResourcePods:   resource.MustParse("10"),
		}))
		fixed.ApplyCapacityOverlay(corev1.ResourceList{hugePages2Mi: resource.MustParse("2Gi")})
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fixed}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(hugePagesPodOptions("1Gi", "256Mi"))
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)

		nodeClaim := expectSingleNodeClaim()
		Expect(nodeClaim.Annotations).ToNot(HaveKey(v1.FlexibleCapacityAnnotationKey))
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1alpha1.CapacityOverlayAppliedAnnotationKey, "true"))
		Expect(node.Status.Allocatable).To(HaveKeyWithValue(hugePages2Mi, resource.MustParse("2Gi")))
		Expect(node.Status.Allocatable.Memory().Value()).To(Equal(quantity("16Gi") - quantity("10Mi") - quantity("2Gi")))
	})
})
