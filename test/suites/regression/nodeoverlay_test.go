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

package integration_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwokv1alpha1 "sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/test"
)

const (
	overlayHugePages2Mi corev1.ResourceName = "hugepages-2Mi"
	overlayDevice       corev1.ResourceName = "example.com/device"
	// kwokKubeReservedMemory is the memory the KWOK provider reserves for the kubelet on every instance type.
	kwokKubeReservedMemory = "10Mi"
)

var _ = Describe("NodeOverlay", Ordered, func() {
	var restoreFeatureGates func()

	BeforeAll(func() {
		if !env.IsDefaultNodeClassKWOK() {
			Skip("NodeOverlay specs check node shapes that only the KWOK provider builds from the overlay annotation")
		}
		restoreFeatureGates = env.ExpectFeatureGatesEnabled("NodeOverlay")
	})
	AfterAll(func() {
		if restoreFeatureGates != nil {
			restoreFeatureGates()
		}
	})
	BeforeEach(func() {
		// Pin the instance types so that the expected sizes are known: the c family has 2Gi of memory per cpu.
		test.ReplaceRequirements(nodePool,
			v1.NodeSelectorRequirementWithMinValues{Key: kwokv1alpha1.InstanceFamilyLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"c"}},
			v1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.ArchitectureAmd64}},
			v1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux)}},
			v1.NodeSelectorRequirementWithMinValues{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}},
		)
	})

	newOverlay := func(name string, capacity, maxCapacity corev1.ResourceList) *v1alpha1.NodeOverlay {
		return test.NodeOverlay(v1alpha1.NodeOverlay{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.NodeOverlaySpec{
				Requirements: []v1alpha1.NodeSelectorRequirement{
					{Key: v1.NodePoolLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{nodePool.Name}},
				},
				Weight:      new(int32(10)),
				Capacity:    capacity,
				MaxCapacity: maxCapacity,
			},
		})
	}
	maxHugePagesOverlay := func(ceiling string) *v1alpha1.NodeOverlay {
		return newOverlay(test.RandomName(), nil, corev1.ResourceList{overlayHugePages2Mi: resource.MustParse(ceiling)})
	}
	expectOverlayValidation := func(overlay *v1alpha1.NodeOverlay, succeeded bool) *v1alpha1.NodeOverlay {
		GinkgoHelper()
		By(fmt.Sprintf("waiting for NodeOverlay %s to report ValidationSucceeded=%t", overlay.Name, succeeded))
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(overlay), overlay)).To(Succeed())
			condition := overlay.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded)
			g.Expect(condition).ToNot(BeNil())
			g.Expect(condition.IsTrue()).To(Equal(succeeded))
		}).Should(Succeed())
		return overlay
	}
	// podOptions returns a pod that requests the given resources. Kubernetes requires extended resource and hugepages
	// requests to equal their limits, and a pod that requests hugepages must also request cpu or memory.
	podOptions := func(memory string, extended corev1.ResourceList) test.PodOptions {
		return test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: testLabels},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: lo.Assign(corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse(memory),
				}, extended),
				Limits: extended,
			},
		}
	}
	hugePages := func(amount string) corev1.ResourceList {
		return corev1.ResourceList{overlayHugePages2Mi: resource.MustParse(amount)}
	}
	expectNodeClaimForNode := func(node corev1.Node) *v1.NodeClaim {
		GinkgoHelper()
		nodeClaims := &v1.NodeClaimList{}
		Expect(env.Client.List(env.Context, nodeClaims)).To(Succeed())
		nodeClaim, ok := lo.Find(nodeClaims.Items, func(nc v1.NodeClaim) bool { return nc.Status.NodeName == node.Name })
		Expect(ok).To(BeTrue(), "expected a NodeClaim for node %s", node.Name)
		return &nodeClaim
	}
	expectNodeForPod := func(pod *corev1.Pod) corev1.Node {
		GinkgoHelper()
		env.EventuallyExpectHealthy(pod)
		env.ExpectExists(pod)
		return env.GetNode(pod.Spec.NodeName)
	}
	// expectAllocatableMemory checks the node's allocatable memory against its instance type's memory, less the
	// kubelet reservation and the hugepages the node was given.
	expectAllocatableMemory := func(node corev1.Node, hugePages string) {
		GinkgoHelper()
		expected := resource.MustParse(node.Labels[kwokv1alpha1.InstanceMemoryLabelKey])
		expected.Sub(resource.MustParse(kwokKubeReservedMemory))
		expected.Sub(resource.MustParse(hugePages))
		Expect(node.Status.Allocatable.Memory().Value()).To(Equal(expected.Value()))
	}

	It("should keep fixed capacity overlays working", func() {
		overlay := newOverlay(test.RandomName(), corev1.ResourceList{overlayDevice: resource.MustParse("10")}, nil)
		env.ExpectCreated(nodeClass, nodePool, overlay)
		expectOverlayValidation(overlay, true)

		pod := test.Pod(podOptions("256Mi", corev1.ResourceList{overlayDevice: resource.MustParse("2")}))
		env.ExpectCreated(pod)
		node := expectNodeForPod(pod)

		nodeClaim := expectNodeClaimForNode(node)
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1alpha1.CapacityOverlayAppliedAnnotationKey, "true"))
		Expect(nodeClaim.Annotations).ToNot(HaveKey(v1.FlexibleCapacityAnnotationKey))
		Expect(node.Status.Allocatable).To(HaveKey(overlayDevice))
	})
	It("should give a node exactly the hugepages its pod requests, up to the ceiling", func() {
		overlay := maxHugePagesOverlay("4Gi")
		env.ExpectCreated(nodeClass, nodePool, overlay)
		expectOverlayValidation(overlay, true)

		pod := test.Pod(podOptions("256Mi", hugePages("1Gi")))
		env.ExpectCreated(pod)
		node := expectNodeForPod(pod)

		nodeClaim := expectNodeClaimForNode(node)
		Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"1Gi"}`))
		Expect(nodeClaim.Spec.Resources.Requests).To(HaveKeyWithValue(overlayHugePages2Mi, resource.MustParse("1Gi")))
		Expect(node.Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"1Gi"}`))
		Expect(node.Status.Allocatable).To(HaveKeyWithValue(overlayHugePages2Mi, resource.MustParse("1Gi")))
		expectAllocatableMemory(node, "1Gi")
	})
	It("should not launch a node for a pod that requests more hugepages than the ceiling", func() {
		overlay := maxHugePagesOverlay("1Gi")
		env.ExpectCreated(nodeClass, nodePool, overlay)
		expectOverlayValidation(overlay, true)

		pod := test.Pod(podOptions("256Mi", hugePages("2Gi")))
		env.ExpectCreated(pod)
		env.ConsistentlyExpectNodeClaimCountNotExceed(30*time.Second, 0)
		Expect(env.ExpectExists(pod).(*corev1.Pod).Spec.NodeName).To(BeEmpty())
	})
	It("should choose a larger instance type when memory and hugepages together do not fit", func() {
		overlay := maxHugePagesOverlay("4Gi")
		env.ExpectCreated(nodeClass, nodePool, overlay)
		expectOverlayValidation(overlay, true)

		// c-1x has 2Gi of memory: 1Gi of memory plus 1536Mi of hugepages only fits on c-2x
		pod := test.Pod(podOptions("1Gi", hugePages("1536Mi")))
		env.ExpectCreated(pod)
		node := expectNodeForPod(pod)

		Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, "c-2x-amd64-linux"))
		expectAllocatableMemory(node, "1536Mi")
	})
	It("should size each node to the hugepages of its own pods", func() {
		overlay := maxHugePagesOverlay("4Gi")
		env.ExpectCreated(nodeClass, nodePool, overlay)
		expectOverlayValidation(overlay, true)

		// env.ExpectCreated overwrites the value of testLabels, so the pods spread on a label of their own
		spreadLabels := map[string]string{"overlay-test/spread": "hugepages"}
		spread := []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: spreadLabels},
			TopologyKey:   corev1.LabelHostname,
		}}
		small := podOptions("256Mi", hugePages("512Mi"))
		small.Labels = lo.Assign(small.Labels, spreadLabels)
		small.PodAntiRequirements = spread
		large := podOptions("256Mi", hugePages("1Gi"))
		large.Labels = lo.Assign(large.Labels, spreadLabels)
		large.PodAntiRequirements = spread
		// Create the pods one at a time. kube-scheduler does not follow Karpenter's nominations, so with both pods
		// pending it can bind the small pod to the node that Karpenter sized for the large pod.
		largePod := test.Pod(large)
		env.ExpectCreated(largePod)
		largeNode := expectNodeForPod(largePod)
		smallPod := test.Pod(small)
		env.ExpectCreated(smallPod)
		smallNode := expectNodeForPod(smallPod)

		Expect(smallNode.Name).ToNot(Equal(largeNode.Name))
		Expect(expectNodeClaimForNode(smallNode).Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"512Mi"}`))
		Expect(expectNodeClaimForNode(largeNode).Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"1Gi"}`))
		Expect(smallNode.Status.Allocatable).To(HaveKeyWithValue(overlayHugePages2Mi, resource.MustParse("512Mi")))
		Expect(largeNode.Status.Allocatable).To(HaveKeyWithValue(overlayHugePages2Mi, resource.MustParse("1Gi")))
	})
	It("should not reserve hugepages on a node whose pods request none", func() {
		overlay := maxHugePagesOverlay("4Gi")
		env.ExpectCreated(nodeClass, nodePool, overlay)
		expectOverlayValidation(overlay, true)

		pod := test.Pod(podOptions("256Mi", nil))
		env.ExpectCreated(pod)
		node := expectNodeForPod(pod)

		Expect(expectNodeClaimForNode(node).Annotations).ToNot(HaveKey(v1.FlexibleCapacityAnnotationKey))
		Expect(node.Status.Allocatable).ToNot(HaveKey(overlayHugePages2Mi))
		expectAllocatableMemory(node, "0")
	})
	It("should size a consolidation replacement to the hugepages of the pods that remain", func() {
		nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
		overlay := maxHugePagesOverlay("4Gi")
		env.ExpectCreated(nodeClass, nodePool, overlay)
		expectOverlayValidation(overlay, true)

		// Three pods that need 3Gi of hugepages together fit on one c-2x node
		dep := test.Deployment(test.DeploymentOptions{
			Replicas:   3,
			PodOptions: podOptions("256Mi", hugePages("1Gi")),
		})
		env.ExpectCreated(dep)
		env.EventuallyExpectHealthyPodCount(labels.SelectorFromSet(dep.Spec.Selector.MatchLabels), 3)
		original := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		Expect(original.Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"3Gi"}`))

		// With one pod left, a cheaper node with only 1Gi of hugepages replaces the original
		dep.Spec.Replicas = new(int32(1))
		env.ExpectUpdated(dep)
		env.EventuallyExpectNotFound(original)
		replacement := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		Expect(replacement.Annotations).To(HaveKeyWithValue(v1.FlexibleCapacityAnnotationKey, `{"hugepages-2Mi":"1Gi"}`))
		env.EventuallyExpectHealthyPodCount(labels.SelectorFromSet(dep.Spec.Selector.MatchLabels), 1)
	})
	It("should mark a conflict when overlays of the same weight set the same resource in capacity and maxCapacity", func() {
		suffix := test.RandomName()
		// Overlays of the same weight are evaluated in reverse name order, so the "a-" overlay is the one in conflict
		fixed := newOverlay("a-"+suffix, hugePages("2Gi"), nil)
		flexible := newOverlay("b-"+suffix, nil, hugePages("4Gi"))
		env.ExpectCreated(nodeClass, nodePool, fixed, flexible)

		expectOverlayValidation(flexible, true)
		conflicting := expectOverlayValidation(fixed, false)
		Expect(conflicting.StatusConditions().Get(v1alpha1.ConditionTypeValidationSucceeded).Reason).To(Equal("Conflict"))
	})
})
