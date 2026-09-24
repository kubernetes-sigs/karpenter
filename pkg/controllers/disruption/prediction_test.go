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

package disruption_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/state/prediction"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Prediction", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node

	BeforeEach(func() {

		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			fake.NewInstanceType("expensive",
				fake.WithResources(corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("100"),
				}),
				fake.WithOfferings(cloudprovider.Offering{
					Available:    true,
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
					Price:        1.0,
				}),
			),
			fake.NewInstanceType("cheap",
				fake.WithResources(corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
					corev1.ResourcePods:   resource.MustParse("100"),
				}),
				fake.WithOfferings(cloudprovider.Offering{
					Available:    true,
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
					Price:        0.5,
				}),
			),
		}
		ExpectSingletonReconciled(ctx, pricingController)

		nodePool = test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
					Budgets:             []v1.Budget{{Nodes: "100%"}},
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
				},
			},
		})

		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: "expensive",
					v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
					corev1.LabelTopologyZone:       "test-zone-1a",
				},
			},
			Status: v1.NodeClaimStatus{
				Allocatable: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("100"),
				},
			},
		})
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
	})

	It("should not consolidate to a cheaper node when predicted workload requests exceed its capacity", func() {
		// Pod currently requests 1 CPU, VPA predicts 3 CPU
		// Cheap node (2 CPU): predicted 3 > 2 won't fit
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": "test"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID,
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-app"},
			rs.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("3")},
			}},
			env.Clock.Now(),
		)

		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)

		dc := disruption.NewController(env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(NewMethodsWithNopValidator()...))
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectSingletonReconciled(ctx, dc)

		Expect(queue.GetCommands()).To(BeEmpty())
	})

	It("should consolidate to a cheaper node when both workload and daemon predictions fit within capacity", func() {
		// Workload pod: current 1 CPU, VPA predicts 800m
		// DaemonSet: current 100m, VPA predicts 500m
		// Total on cheap node (2 CPU): workload 800m + daemon 500m = 1300m < 2000m fits
		ds := test.DaemonSet(test.DaemonSetOptions{PodOptions: test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		}})
		ExpectApplied(ctx, env.Client, ds)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(ds), ds)).To(Succeed())

		dsPod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID,
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-ds"},
			ds.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				ds.Spec.Template.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("500m")},
			}},
			env.Clock.Now(),
		)

		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		workloadPod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": "test"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID,
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-app"},
			rs.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				workloadPod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("800m")},
			}},
			env.Clock.Now(),
		)

		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, dsPod, workloadPod)
		ExpectManualBinding(ctx, env.Client, dsPod, node)
		ExpectManualBinding(ctx, env.Client, workloadPod, node)

		dc := disruption.NewController(env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(NewMethodsWithNopValidator()...))
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectSingletonReconciled(ctx, dc)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Replacements).To(HaveLen(1))
		Expect(cmds[0].Replacements[0].InstanceTypeOptions).To(ContainElement(HaveField("Name", "cheap")))
	})

	It("should consolidate more aggressively when VPA predicts lower requests than current", func() {
		// Pod currently requests 3 CPU, VPA predicts only 1 CPU
		// Without predictions: 3 CPU > 2 CPU (cheap) can't consolidate
		// With predictions: 1 CPU < 2 CPU (cheap) can consolidate
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": "test"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID,
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")},
			},
		})

		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-app"},
			rs.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("1")},
			}},
			env.Clock.Now(),
		)

		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)

		dc := disruption.NewController(env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(NewMethodsWithNopValidator()...))
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectSingletonReconciled(ctx, dc)

		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Replacements).To(HaveLen(1))
		Expect(cmds[0].Replacements[0].InstanceTypeOptions).To(ContainElement(HaveField("Name", "cheap")))
	})

	It("should use current requests for pods without predictions and predicted requests for those with", func() {
		// Pod A: 1 CPU, VPA predicts 1.5 CPU
		// Pod B: 1 CPU, no VPA prediction
		// Total predicted: 1.5 + 1 = 2.5 > 2 (cheap) can't consolidate
		rsA := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rsA)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rsA), rsA)).To(Succeed())

		rsB := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rsB)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rsB), rsB)).To(Succeed())

		podA := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": "a"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rsA.Name, UID: rsA.UID,
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})

		podB := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": "b"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rsB.Name, UID: rsB.UID,
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})

		// Only podA has a VPA prediction
		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-a"},
			rsA.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				podA.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("1500m")},
			}},
			env.Clock.Now(),
		)

		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, podA, podB)
		ExpectManualBinding(ctx, env.Client, podA, node)
		ExpectManualBinding(ctx, env.Client, podB, node)

		dc := disruption.NewController(env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(NewMethodsWithNopValidator()...))
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectSingletonReconciled(ctx, dc)

		Expect(queue.GetCommands()).To(BeEmpty())
	})

	It("should replace a drifted node using predicted requests for replacement node sizing", func() {
		// Node is drifted, pod requests 1 CPU, VPA predicts 3 CPU
		// Expensive instance (4 CPU) can fit 3 CPU drift replacement succeeds
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeDrifted)

		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": "test"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID,
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})

		// VPA predicts 3 CPU — fits on expensive (4 CPU) but not cheap (2 CPU)
		store.Set(
			types.NamespacedName{Namespace: "default", Name: "vpa-app"},
			rs.UID,
			&prediction.Prediction{Containers: map[string]corev1.ResourceList{
				pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("3")},
			}},
			env.Clock.Now(),
		)

		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)

		dc := disruption.NewController(env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(NewMethodsWithNopValidator()...))
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectSingletonReconciled(ctx, dc)

		// Drift should produce a replacement command using the expensive instance
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Replacements).To(HaveLen(1))
		Expect(cmds[0].Replacements[0].InstanceTypeOptions).To(ContainElement(HaveField("Name", "expensive")))
	})
})
