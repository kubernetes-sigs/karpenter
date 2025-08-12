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

package static_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/deprovisioning/static"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx                      context.Context
	fakeClock                *clock.FakeClock
	cluster                  *state.Cluster
	nodeController           *informer.NodeController
	daemonsetController      *informer.DaemonSetController
	cloudProvider            *fake.CloudProvider
	controller               *static.Controller
	env                      *test.Environment
	nodeClaimStateController *informer.NodeClaimController
	recorder                 events.Recorder
)

type failingClient struct {
	client.Client
}

func (f *failingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*v1.NodeClaim); ok {
		return fmt.Errorf("simulated error deleting nodeclaims")
	}
	return f.Client.Delete(ctx, obj, opts...)
}

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers/Deprovisioning/Static")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	ctx = options.ToContext(ctx, test.Options())
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeController = informer.NewNodeController(env.Client, cluster)
	daemonsetController = informer.NewDaemonSetController(env.Client, cluster)
	recorder = events.NewRecorder(&record.FakeRecorder{})
	controller = static.NewController(env.Client, cluster, recorder, cloudProvider)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cloudProvider, cluster)
})

var _ = BeforeEach(func() {
	ctx = options.ToContext(ctx, test.Options())
	cloudProvider.Reset()
	cluster.Reset()

	// ensure any waiters on our clock are allowed to proceed before resetting our clock time
	for fakeClock.HasWaiters() {
		fakeClock.Step(1 * time.Minute)
	}
	fakeClock.SetTime(time.Now())
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cloudProvider.Reset()
	cluster.Reset()
})

var _ = Describe("Static Deprovisioning Controller", func() {
	Context("Reconcile", func() {
		It("should return early if nodepool is not managed by cloud provider", func() {
			nodePool := test.StaticNodePool()
			nodePool.Spec.Replicas = lo.ToPtr(int64(1))
			nodePool.Spec.Template.Spec.NodeClassRef = &v1.NodeClassReference{
				Group: "test.group",
				Kind:  "UnmanagedNodeClass",
				Name:  "test",
			}
			ExpectApplied(ctx, env.Client, nodePool)

			result := ExpectObjectReconciled(ctx, env.Client, controller, nodePool)

			Expect(result.RequeueAfter).To(BeZero())
		})

		It("should return early if nodepool root condition is not true", func() {
			nodePool := test.StaticNodePool()
			nodePool.Spec.Replicas = lo.ToPtr(int64(1))
			nodePool.StatusConditions().SetFalse(v1.ConditionTypeValidationSucceeded, "ValidationFailed", "Validation failed")
			ExpectApplied(ctx, env.Client, nodePool)

			result := ExpectObjectReconciled(ctx, env.Client, controller, nodePool)

			Expect(result.RequeueAfter).To(BeZero())
		})

		It("should return early if nodepool replicas is nil", func() {
			nodePool := test.StaticNodePool()
			nodePool.Spec.Replicas = nil
			ExpectApplied(ctx, env.Client, nodePool)

			result := ExpectObjectReconciled(ctx, env.Client, controller, nodePool)

			Expect(result.RequeueAfter).To(BeZero())
		})

		It("should return early if current node count is less than or equal to desired replicas", func() {
			nodePool := test.StaticNodePool()
			nodePool.Spec.Replicas = lo.ToPtr(int64(3))

			// Create 2 nodes (less than desired 3)
			nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						v1.NodeInitializedLabelKey:     "true",
						corev1.LabelInstanceTypeStable: "stable.instance",
					},
				},
				Status: v1.NodeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], nodeClaims[1], nodes[0], nodes[1])

			// Update cluster state to track the nodes
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})
			Expect(cluster.Nodes()).To(HaveLen(2))

			result := ExpectObjectReconciled(ctx, env.Client, controller, nodePool)

			Expect(result.RequeueAfter).To(BeZero())

			// Should not delete any NodeClaims
			existingNodeClaims := &v1.NodeClaimList{}
			Expect(env.Client.List(ctx, existingNodeClaims)).To(Succeed())
			Expect(existingNodeClaims.Items).To(HaveLen(2))
		})

		It("should terminate excess nodeclaims when current count exceeds desired replicas", func() {
			nodePool := test.StaticNodePool()
			nodePool.Spec.Replicas = lo.ToPtr(int64(2))

			nodeClaims, nodes := test.NodeClaimsAndNodes(4, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						v1.NodeInitializedLabelKey:     "true",
						corev1.LabelInstanceTypeStable: "stable.instance",
					},
				},
				Status: v1.NodeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := 0; i < 4; i++ {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Update cluster state to track the nodes
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimStateController, nodes, nodeClaims)
			Expect(cluster.Nodes()).To(HaveLen(4))

			result := ExpectObjectReconciled(ctx, env.Client, controller, nodePool)

			Expect(result.RequeueAfter).To(BeZero())

			// Should terminate 2 NodeClaims (4 current - 2 desired = 2 to terminate)
			remainingNodeClaims := &v1.NodeClaimList{}
			Expect(env.Client.List(ctx, remainingNodeClaims)).To(Succeed())
			Expect(remainingNodeClaims.Items).To(HaveLen(2))
		})

		It("should prioritize empty nodes (with only reschedulable pods) for termination", func() {
			nodePool := test.StaticNodePool()
			nodePool.Spec.Replicas = lo.ToPtr(int64(2))

			nodeClaims, nodes := test.NodeClaimsAndNodes(4, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						v1.NodeInitializedLabelKey:     "true",
						corev1.LabelInstanceTypeStable: "stable.instance",
					},
				},
				Status: v1.NodeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool)

			// Nodes 0 and 2: Add only DaemonSet pods (reschedulable)
			for i := range 4 {
				pod := test.Pod(test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: lo.Ternary(i == 0 || i == 2, "kube-system", "default"),
						OwnerReferences: lo.Ternary(i == 0 || i == 2,
							[]metav1.OwnerReference{{
								APIVersion: "apps/v1",
								Kind:       "DaemonSet",
								Name:       "test-daemonset",
								UID:        "test-uid",
							}},
							nil),
					},
					NodeName: nodes[i].Name,
				})
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				ExpectApplied(ctx, env.Client, pod)
			}

			// Update cluster state to track the nodes
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimStateController, nodes, nodeClaims)
			Expect(cluster.Nodes()).To(HaveLen(4))

			result := ExpectObjectReconciled(ctx, env.Client, controller, nodePool)

			Expect(result.RequeueAfter).To(BeZero())

			// Should terminate 2 NodeClaims (4 current - 2 desired = 2 to terminate)
			remainingNodeClaims := &v1.NodeClaimList{}
			Expect(env.Client.List(ctx, remainingNodeClaims)).To(Succeed())

			activeNodeClaims := lo.Filter(remainingNodeClaims.Items, func(nc v1.NodeClaim, _ int) bool {
				return nc.DeletionTimestamp.IsZero()
			})
			activeNodeClaimNames := lo.Map(activeNodeClaims, func(nc v1.NodeClaim, _ int) string {
				return nc.Name
			})
			Expect(activeNodeClaimNames).To(HaveLen(2))
			Expect(activeNodeClaimNames).To(ContainElements(nodeClaims[0].Name, nodeClaims[2].Name))
		})

		It("should terminate non-empty nodes when empty nodes are insufficient", func() {
			nodePool := test.StaticNodePool()
			nodePool.Spec.Replicas = lo.ToPtr(int64(1))

			nodeClaims, nodes := test.NodeClaimsAndNodes(4, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						v1.NodeInitializedLabelKey:     "true",
						corev1.LabelInstanceTypeStable: "stable.instance",
					},
				},
				Status: v1.NodeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool)
			for i := range 4 {
				ExpectApplied(ctx, env.Client, nodes[i], nodeClaims[i])
			}

			pod1 := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{},
				NodeName:   nodes[0].Name,
			})
			pod2 := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{},
				NodeName:   nodes[2].Name,
			})

			ExpectApplied(ctx, env.Client, pod1, pod2)

			// Update cluster state to track the nodes
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimStateController, nodes, nodeClaims)
			Expect(cluster.Nodes()).To(HaveLen(4))

			result := ExpectObjectReconciled(ctx, env.Client, controller, nodePool)

			Expect(result.RequeueAfter).To(BeZero())

			// Should terminate 3 NodeClaims (4 current - 1 desired = 3 to terminate)
			remainingNodeClaims := &v1.NodeClaimList{}
			Expect(env.Client.List(ctx, remainingNodeClaims)).To(Succeed())
			Expect(remainingNodeClaims.Items).To(HaveLen(1))
		})

		It("should handle zero replicas by terminating all nodeclaims", func() {
			nodePool := test.StaticNodePool()
			nodePool.Spec.Replicas = lo.ToPtr(int64(0))

			nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						v1.NodeInitializedLabelKey:     "true",
						corev1.LabelInstanceTypeStable: "stable.instance",
					},
				},
				Status: v1.NodeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, nodePool)
			for i := range 3 {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			// Update cluster state to track the nodes
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimStateController, nodes, nodeClaims)
			Expect(cluster.Nodes()).To(HaveLen(3))

			result := ExpectObjectReconciled(ctx, env.Client, controller, nodePool)

			Expect(result.RequeueAfter).To(BeZero())

			// Should terminate all NodeClaims
			remainingNodeClaims := &v1.NodeClaimList{}
			Expect(env.Client.List(ctx, remainingNodeClaims)).To(Succeed())
			Expect(remainingNodeClaims.Items).To(HaveLen(0))
		})

		It("should handle nodes with mixed pod types correctly", func() {
			nodePool := test.StaticNodePool()
			nodePool.Spec.Replicas = lo.ToPtr(int64(1))
			ExpectApplied(ctx, env.Client, nodePool)

			pods := []*corev1.Pod{}
			nodes := []*corev1.Node{}
			nodeClaims := []*v1.NodeClaim{}

			for i := 0; i < 3; i++ {
				nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:        nodePool.Name,
							v1.NodeInitializedLabelKey: "true",
						},
					},
					Status: v1.NodeClaimStatus{
						ProviderID: test.RandomProviderID(),
						Capacity: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10"),
							corev1.ResourceMemory: resource.MustParse("1000Mi"),
						},
					},
				})

				switch i {
				case 0:
					pods = append(pods, test.Pod(test.PodOptions{
						ObjectMeta: metav1.ObjectMeta{Name: "system-pod-0", Namespace: "kube-system"},
						NodeName:   node.Name,
					}))
				case 1:
					pods = append(pods,
						test.Pod(test.PodOptions{
							ObjectMeta: metav1.ObjectMeta{Name: "system-pod-1", Namespace: "kube-system"},
							NodeName:   node.Name,
						}),
						test.Pod(test.PodOptions{
							ObjectMeta: metav1.ObjectMeta{Name: "app-pod-1", Namespace: "default"},
							NodeName:   node.Name,
						}),
					)
				case 2:
					pods = append(pods, test.Pod(test.PodOptions{
						ObjectMeta: metav1.ObjectMeta{Name: "app-pod-2", Namespace: "default"},
						NodeName:   node.Name,
					}))
				}

				nodes = append(nodes, node)
				nodeClaims = append(nodeClaims, nodeClaim)
				ExpectApplied(ctx, env.Client, nodeClaim, node)
			}

			for _, pod := range pods {
				ExpectApplied(ctx, env.Client, pod)
			}

			// Update cluster state to track the nodes
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimStateController, nodes, nodeClaims)
			Expect(cluster.Nodes()).To(HaveLen(3))

			result := ExpectObjectReconciled(ctx, env.Client, controller, nodePool)

			Expect(result.RequeueAfter).To(BeZero())

			// Should terminate 2 NodeClaims (3 current - 1 desired = 2 to terminate)
			remainingNodeClaims := &v1.NodeClaimList{}
			Expect(env.Client.List(ctx, remainingNodeClaims)).To(Succeed())
			Expect(remainingNodeClaims.Items).To(HaveLen(1))
		})

		It("should handle no active nodeclaims gracefully", func() {
			nodePool := test.StaticNodePool()
			nodePool.Spec.Replicas = lo.ToPtr(int64(0))
			ExpectApplied(ctx, env.Client, nodePool)

			// Update cluster state with no nodes
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimStateController, []*corev1.Node{}, []*v1.NodeClaim{})

			result := ExpectObjectReconciled(ctx, env.Client, controller, nodePool)

			Expect(result.RequeueAfter).To(BeZero())
		})

		Context("Requeue Scenarios", func() {
			It("should requeue when there is a failure while deleting nodeclaims", func() {
				nodePool := test.StaticNodePool()
				nodePool.Spec.Replicas = lo.ToPtr(int64(1))
				ExpectApplied(ctx, env.Client, nodePool)

				failingController := static.NewController(&failingClient{Client: env.Client}, cluster, events.NewRecorder(&record.FakeRecorder{}), cloudProvider)

				// Create 3 nodeclaims, so 2 need to be terminated
				nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							v1.NodeInitializedLabelKey:     "true",
							corev1.LabelInstanceTypeStable: "stable.instance",
						},
					},
					Status: v1.NodeClaimStatus{
						Capacity: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10"),
							corev1.ResourceMemory: resource.MustParse("1000Mi"),
						},
					},
				})

				ExpectApplied(ctx, env.Client, nodePool)
				for i := range 3 {
					ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
				}

				// Update cluster state to track the nodes
				ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimStateController, nodes, nodeClaims)
				Expect(cluster.Nodes()).To(HaveLen(3))
				ExpectApplied(ctx, env.Client, nodeClaims[0])

				result, err := failingController.Reconcile(ctx, nodePool)
				Expect(err).To(HaveOccurred())

				// Should requeue because not all nodeclaims were successfully deleted
				Expect(result.RequeueAfter).To(Equal(time.Second))

				// Verify that some nodeclaims still exist (deletion didn't complete as expected)
				remainingNodeClaims := &v1.NodeClaimList{}
				Expect(env.Client.List(ctx, remainingNodeClaims)).To(Succeed())

				// At least one nodeclaim should still be active due to the finalizer
				activeNodeClaims := lo.Filter(remainingNodeClaims.Items, func(nc v1.NodeClaim, _ int) bool {
					return nc.DeletionTimestamp.IsZero()
				})
				Expect(len(activeNodeClaims)).To(BeNumerically(">", 1)) // More than desired replicas (1)
			})
		})

		Context("Helper Functions", func() {
			DescribeTable("should detect replica or status changes",
				func(oldReplicas, newReplicas *int64, oldReady, newReady bool, expected bool) {
					old := &v1.NodePool{Spec: v1.NodePoolSpec{Replicas: oldReplicas}}
					new := &v1.NodePool{Spec: v1.NodePoolSpec{Replicas: newReplicas}}

					if oldReady {
						old.StatusConditions().SetTrue(v1.ConditionTypeValidationSucceeded)
						old.StatusConditions().SetTrue(v1.ConditionTypeNodeClassReady)
					} else {
						old.StatusConditions().SetFalse(v1.ConditionTypeValidationSucceeded, "reason", "old not ready")
					}

					if newReady {
						new.StatusConditions().SetTrue(v1.ConditionTypeValidationSucceeded)
						new.StatusConditions().SetTrue(v1.ConditionTypeNodeClassReady)
					} else {
						new.StatusConditions().SetFalse(v1.ConditionTypeValidationSucceeded, "reason", "new not ready")
					}

					Expect(static.HasNodePoolReplicaOrStatusChanged(old, new)).To(Equal(expected))
				},

				Entry("replica changed", lo.ToPtr(int64(5)), lo.ToPtr(int64(3)), false, false, true),
				Entry("replica same, false → true", lo.ToPtr(int64(5)), lo.ToPtr(int64(5)), false, true, true),
				Entry("replica same, true → false", lo.ToPtr(int64(5)), lo.ToPtr(int64(5)), true, false, false),
				Entry("replica same, both true", lo.ToPtr(int64(5)), lo.ToPtr(int64(5)), true, true, false),
				Entry("replica same, both false", lo.ToPtr(int64(5)), lo.ToPtr(int64(5)), false, false, false),
			)
		})

	})
})
