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
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// The node fits one of these pods but not two, so the replacement for a pod evicted by the reboot's drain only fits
// on the rebooting node once the evicted pod no longer counts against it.
var _ = Describe("Rebooting Nodes", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	var evicted *corev1.Pod
	podOptions := test.PodOptions{ResourceRequirements: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
	}}

	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}},
			Status: v1.NodeClaimStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("110"),
				},
			},
		})
		node.Labels[corev1.LabelHostname] = node.Name
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, env.Clock, nodeClaim)
		ExpectMakeNodesInitialized(ctx, env.Client, env.Clock, node)

		evicted = test.Pod(podOptions)
		ExpectApplied(ctx, env.Client, evicted)
		ExpectManualBinding(ctx, env.Client, evicted, node)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(evicted))
	})

	rebootNode := func() {
		GinkgoHelper()
		nodeClaim, node = ExpectExists(ctx, env.Client, nodeClaim), ExpectExists(ctx, env.Client, node)
		nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonRequested, "reboot requested")
		node.Spec.Taints = append(node.Spec.Taints, v1.RebootingNoScheduleTaint)
		ExpectApplied(ctx, env.Client, nodeClaim, node)
	}
	syncState := func() {
		GinkgoHelper()
		ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(evicted))
	}

	It("should schedule a pod to a rebooting node that fits once the node's terminating pods are gone", func() {
		rebootNode()
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()

		pod := test.UnschedulablePod(podOptions)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).To(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	})
	It("should keep excluding a rebooting node's terminating pods after its NodeClaim is updated", func() {
		rebootNode()
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		nodeClaim.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonIssued, "reboot issued")
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))

		pod := test.UnschedulablePod(podOptions)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).To(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	})
	It("should ignore the required anti-affinity of a rebooting node's terminating pods", func() {
		antiAffinity := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ha"}},
			TopologyKey:   corev1.LabelHostname,
		}}}}
		evicted = ExpectExists(ctx, env.Client, evicted)
		ExpectDeleted(ctx, env.Client, evicted)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(evicted))
		evicted = test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ha"}}})
		evicted.Spec.Affinity = antiAffinity
		ExpectApplied(ctx, env.Client, evicted)
		ExpectManualBinding(ctx, env.Client, evicted, node)
		rebootNode()
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()

		pod := test.UnschedulablePod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ha"}}})
		pod.Spec.Affinity = antiAffinity
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).To(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	})
	It("should keep the required anti-affinity of a terminating pod on a node that isn't rebooting", func() {
		antiAffinity := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ha"}},
			TopologyKey:   corev1.LabelHostname,
		}}}}
		evicted = ExpectExists(ctx, env.Client, evicted)
		ExpectDeleted(ctx, env.Client, evicted)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(evicted))
		evicted = test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ha"}}})
		evicted.Spec.Affinity = antiAffinity
		ExpectApplied(ctx, env.Client, evicted)
		ExpectManualBinding(ctx, env.Client, evicted, node)
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()

		pod := test.UnschedulablePod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ha"}}})
		pod.Spec.Affinity = antiAffinity
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).ToNot(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
	})
	It("should keep the zone anti-affinity of a rebooting node's terminating pods", func() {
		antiAffinity := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ha"}},
			TopologyKey:   corev1.LabelTopologyZone,
		}}}}
		evicted = ExpectExists(ctx, env.Client, evicted)
		ExpectDeleted(ctx, env.Client, evicted)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(evicted))
		evicted = test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ha"}}})
		evicted.Spec.Affinity = antiAffinity
		ExpectApplied(ctx, env.Client, evicted)
		ExpectManualBinding(ctx, env.Client, evicted, node)
		rebootNode()
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()

		zone := node.Labels[corev1.LabelTopologyZone]
		pod := test.UnschedulablePod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ha"}}})
		pod.Spec.Affinity = antiAffinity
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		scheduled := ExpectScheduled(ctx, env.Client, pod)
		Expect(scheduled.Name).ToNot(Equal(node.Name))
		Expect(scheduled.Labels[corev1.LabelTopologyZone]).ToNot(Equal(zone))
	})
	It("should ignore the host ports of a rebooting node's terminating pods", func() {
		ports := []corev1.ContainerPort{{ContainerPort: 18080, HostPort: 18080, Protocol: corev1.ProtocolTCP}}
		evicted = ExpectExists(ctx, env.Client, evicted)
		ExpectDeleted(ctx, env.Client, evicted)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(evicted))
		evicted = test.Pod()
		evicted.Spec.Containers[0].Ports = ports
		ExpectApplied(ctx, env.Client, evicted)
		ExpectManualBinding(ctx, env.Client, evicted, node)
		rebootNode()
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()

		pod := test.UnschedulablePod()
		pod.Spec.Containers[0].Ports = ports
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).To(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	})
	It("should keep the host ports of a terminating pod on a node that isn't rebooting", func() {
		ports := []corev1.ContainerPort{{ContainerPort: 18080, HostPort: 18080, Protocol: corev1.ProtocolTCP}}
		evicted = ExpectExists(ctx, env.Client, evicted)
		ExpectDeleted(ctx, env.Client, evicted)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(evicted))
		evicted = test.Pod()
		evicted.Spec.Containers[0].Ports = ports
		ExpectApplied(ctx, env.Client, evicted)
		ExpectManualBinding(ctx, env.Client, evicted, node)
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()

		pod := test.UnschedulablePod()
		pod.Spec.Containers[0].Ports = ports
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).ToNot(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
	})
	// The node can attach one volume, so a pod with a different volume only fits once the evicted pod's volume is gone.
	bindVolumePod := func() {
		GinkgoHelper()
		ExpectApplied(ctx, env.Client, &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{Name: node.Name},
			Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{
				Name:        csiProvider,
				NodeID:      "fake-node-id",
				Allocatable: &storagev1.VolumeNodeResources{Count: new(int32(1))},
			}}},
		}, test.StorageClass(test.StorageClassOptions{
			ObjectMeta:        metav1.ObjectMeta{Name: "reboot-storage-class"},
			Provisioner:       new(csiProvider),
			VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer),
		}))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		evicted = ExpectExists(ctx, env.Client, evicted)
		ExpectDeleted(ctx, env.Client, evicted)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(evicted))
		pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{StorageClassName: new("reboot-storage-class")})
		ExpectApplied(ctx, env.Client, pvc)
		evicted = test.Pod(test.PodOptions{PersistentVolumeClaims: []string{pvc.Name}})
		ExpectApplied(ctx, env.Client, evicted)
		ExpectManualBinding(ctx, env.Client, evicted, node)
	}
	volumePod := func() *corev1.Pod {
		GinkgoHelper()
		pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{StorageClassName: new("reboot-storage-class")})
		ExpectApplied(ctx, env.Client, pvc)
		return test.UnschedulablePod(test.PodOptions{PersistentVolumeClaims: []string{pvc.Name}})
	}
	It("should ignore the volumes of a rebooting node's terminating pods", func() {
		bindVolumePod()
		rebootNode()
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()

		pod := volumePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).To(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	})
	It("should keep the volumes of a terminating pod on a node that isn't rebooting", func() {
		bindVolumePod()
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()

		pod := volumePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).ToNot(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
	})
	It("should count pods that stay on a rebooting node", func() {
		rebootNode()
		syncState()

		pod := test.UnschedulablePod(podOptions)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).ToNot(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
	})
	It("should count terminating daemonset pods on a rebooting node", func() {
		evicted = ExpectExists(ctx, env.Client, evicted)
		evicted.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "ds", UID: "ds-uid", Controller: new(true)}}
		ExpectApplied(ctx, env.Client, evicted)
		rebootNode()
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()

		pod := test.UnschedulablePod(podOptions)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).ToNot(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
	})
	It("should count terminating pods on a node that isn't rebooting", func() {
		ExpectDeletionTimestampSet(ctx, env.Client, evicted)
		syncState()

		pod := test.UnschedulablePod(podOptions)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).ToNot(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
	})
})
