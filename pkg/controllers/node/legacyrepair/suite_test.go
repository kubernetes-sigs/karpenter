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

package legacyrepair_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/node/legacyrepair"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var healthController *legacyrepair.Controller
var env *test.Environment
var cloudProvider *fake.CloudProvider
var recorder *test.EventRecorder
var queue *terminator.Queue

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "LegacyRepair")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(
		test.WithCRDs(apis.CRDs...),
		test.WithCRDs(v1alpha1.CRDs...),
		test.WithFieldIndexers(test.NodeClaimProviderIDFieldIndexer(ctx), test.VolumeAttachmentFieldIndexer(ctx), test.NodeProviderIDFieldIndexer(ctx)),
	)
	cloudProvider = fake.NewCloudProvider()
	recorder = test.NewEventRecorder()
	queue = terminator.NewQueue(env.Clock, env.Client, recorder)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Node Health", func() {
	var node *corev1.Node
	var nodeClaim *v1.NodeClaim
	var nodePool *v1.NodePool

	BeforeEach(func() {
		env.Clock.SetTime(time.Now())
		cloudProvider.Reset()
		healthController = legacyrepair.NewController(env.Client, cloudProvider, env.Clock, recorder)

		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{v1.TerminationFinalizer}}})
		node.Labels[v1.NodePoolLabelKey] = nodePool.Name
		nodeClaim.Labels[v1.NodePoolLabelKey] = nodePool.Name
		cloudProvider.CreatedNodeClaims[node.Spec.ProviderID] = nodeClaim
	})

	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)

		// Reset the metrics collectors
		metrics.NodeClaimsDisruptedTotal.Reset()
		disruption.NodeClaimsUnhealthyDisruptedTotal.Reset()
	})

	Context("Reconciliation", func() {
		It("should delete nodes that are unhealthy by the cloud provider", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "BadNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).ToNot(BeNil())
		})
		It("should delete nodes that registered unhealthy before the NodeClass label was synced", func() {
			nodeClassLabelKey := v1.NodeClassLabelKey(nodeClaim.Spec.NodeClassRef.GroupKind())
			nodeClassName := node.Labels[nodeClassLabelKey]
			delete(node.Labels, nodeClassLabelKey)
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "BadNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			// Sync the NodeClass label onto the Node without changing its conditions
			node = ExpectExists(ctx, env.Client, node)
			conditions := node.Status.Conditions
			node.Labels[nodeClassLabelKey] = nodeClassName
			ExpectApplied(ctx, env.Client, node)
			node = ExpectExists(ctx, env.Client, node)
			Expect(node.Status.Conditions).To(Equal(conditions))

			env.Clock.Step(60 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, healthController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).ToNot(BeNil())
		})
		It("should not delete node when unhealthy type does not match cloud provider passed in value", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "FakeHealthyNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())
		})
		It("should not delete node when health status does not match cloud provider passed in value", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "BadNode",
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())
		})
		It("should not delete node when health duration is not reached", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:   "BadNode",
				Status: corev1.ConditionFalse,
				// We expect the last transition for HealthyNode condition to wait 30 minutes
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			env.Clock.Step(20 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())
		})
		It("should set annotation termination grace period when force termination is started", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:   "BadNode",
				Status: corev1.ConditionFalse,
				// We expect the last transition for HealthyNode condition to wait 30 minutes
				LastTransitionTime: metav1.Time{Time: time.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Format(time.RFC3339)))
		})
		It("should not respect termination grace period if set on the nodepool", func() {
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: env.Clock.Now().Add(120 * time.Minute).Format(time.RFC3339)})
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:   "BadNode",
				Status: corev1.ConditionFalse,
				// We expect the last transition for HealthyNode condition to wait 30 minutes
				LastTransitionTime: metav1.Time{Time: time.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, env.Clock.Now().Format(time.RFC3339)))
		})
		It("should not update termination grace period if set before the current time", func() {
			terminationTime := env.Clock.Now().Add(-3 * time.Minute).Format(time.RFC3339)
			nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{v1.NodeClaimTerminationTimestampAnnotationKey: terminationTime})
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:   "BadNode",
				Status: corev1.ConditionFalse,
				// We expect the last transition for HealthyNode condition to wait 30 minutes
				LastTransitionTime: metav1.Time{Time: time.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, terminationTime))
		})
		It("should return the requeue interval for the condition closest to its terminationDuration", func() {
			cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
				{
					ConditionType:      "BadNode",
					ConditionStatus:    corev1.ConditionFalse,
					TolerationDuration: 60 * time.Minute,
					Action:             cloudprovider.ReplaceNode,
				},
				{
					ConditionType:      "ValidUnhealthyCondition",
					ConditionStatus:    corev1.ConditionFalse,
					TolerationDuration: 30 * time.Minute,
					Action:             cloudprovider.ReplaceNode,
				},
			}
			healthController = legacyrepair.NewController(env.Client, cloudProvider, env.Clock, recorder)
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:   "ValidUnhealthyCondition",
				Status: corev1.ConditionFalse,
				// We expect the last transition for HealthyNode condition to wait 30 minutes
				LastTransitionTime: metav1.Time{Time: time.Now()},
			}, corev1.NodeCondition{
				Type:   "BadNode",
				Status: corev1.ConditionFalse,
				// We expect the last transition for HealthyNode condition to wait 30 minutes
				LastTransitionTime: metav1.Time{Time: time.Now()},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			env.Clock.Step(27 * time.Minute)

			result := ExpectObjectReconciled(ctx, env.Client, healthController, node)
			Expect(result.RequeueAfter).To(BeNumerically("~", time.Minute*3, time.Second))
		})
		It("should return the requeue interval for the time between now and when the nodeClaim termination time", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:   "BadNode",
				Status: corev1.ConditionFalse,
				// We expect the last transition for HealthyNode condition to wait 30 minutes
				LastTransitionTime: metav1.Time{Time: time.Now()},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			env.Clock.Step(27 * time.Minute)

			result := ExpectObjectReconciled(ctx, env.Client, healthController, node)
			Expect(result.RequeueAfter).To(BeNumerically("~", time.Minute*3, time.Second))
		})
	})

	Context("Repair policies", func() {
		It("should ignore policies with a non-replace action", func() {
			cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{{
				ConditionType:      "BadNode",
				ConditionStatus:    corev1.ConditionFalse,
				TolerationDuration: 30 * time.Minute,
				Action:             cloudprovider.RebootNode,
			}}
			healthController = legacyrepair.NewController(env.Client, cloudProvider, env.Clock, recorder)
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "BadNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())
		})
		It("should replace with the replace policy when a reboot policy also matches", func() {
			cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
				{
					ConditionType:      "BadNode",
					ConditionStatus:    corev1.ConditionFalse,
					ReasonRegex:        "Transient",
					TolerationDuration: 10 * time.Minute,
					Action:             cloudprovider.RebootNode,
				},
				{
					ConditionType:      "BadNode",
					ConditionStatus:    corev1.ConditionFalse,
					TolerationDuration: 30 * time.Minute,
					Action:             cloudprovider.ReplaceNode,
				},
			}
			healthController = legacyrepair.NewController(env.Client, cloudProvider, env.Clock, recorder)
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "BadNode",
				Status:             corev1.ConditionFalse,
				Reason:             "Transient",
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			// The reboot policy's 10m toleration doesn't apply; the replace policy waits its own 30m.
			env.Clock.Step(15 * time.Minute)
			result := ExpectObjectReconciled(ctx, env.Client, healthController, node)
			Expect(result.RequeueAfter).To(BeNumerically("~", 15*time.Minute, time.Second))
			Expect(ExpectExists(ctx, env.Client, nodeClaim).DeletionTimestamp).To(BeNil())

			env.Clock.Step(15 * time.Minute)
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			Expect(ExpectExists(ctx, env.Client, nodeClaim).DeletionTimestamp).ToNot(BeNil())
		})
		It("should only match a policy with a reason regex when the condition reason matches", func() {
			cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{{
				ConditionType:      "BadNode",
				ConditionStatus:    corev1.ConditionFalse,
				ReasonRegex:        "^Fatal$",
				TolerationDuration: 30 * time.Minute,
				Action:             cloudprovider.ReplaceNode,
			}}
			healthController = legacyrepair.NewController(env.Client, cloudProvider, env.Clock, recorder)
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "BadNode",
				Status:             corev1.ConditionFalse,
				Reason:             "Other",
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			Expect(ExpectExists(ctx, env.Client, nodeClaim).DeletionTimestamp).To(BeNil())

			node.Status.Conditions[len(node.Status.Conditions)-1].Reason = "Fatal"
			ExpectApplied(ctx, env.Client, node)
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			Expect(ExpectExists(ctx, env.Client, nodeClaim).DeletionTimestamp).ToNot(BeNil())
		})
	})

	Context("Forceful termination", func() {
		It("should ignore node disruption budgets", func() {
			// Blocking disruption budgets
			nodePool.Spec.Disruption = v1.Disruption{
				Budgets: []v1.Budget{
					{
						Nodes: "0",
					},
				},
			}
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "BadNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).ToNot(BeNil())
		})
		It("should ignore do-not-disrupt on a node", func() {
			node.Annotations = map[string]string{v1.DoNotDisruptAnnotationKey: "true"}
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "BadNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).ToNot(BeNil())
		})
		It("should ignore unhealthy nodes if more then 20% of the nodes are unhealthy in a nodepool", func() {
			ExpectApplied(ctx, env.Client, nodePool)
			nodeClaims, nodes := test.NodeClaimsAndNodes(10, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{v1.TerminationFinalizer}}})
			for i := range 3 {
				nodes[i].Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
					Type:               "BadNode",
					Status:             corev1.ConditionFalse,
					LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
				})
			}
			for i := range nodes {
				nodes[i].Labels[v1.NodePoolLabelKey] = nodePool.Name
				nodeClaims[i].Labels[v1.NodePoolLabelKey] = nodePool.Name
			}
			for i := range 10 {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			env.Clock.Step(60 * time.Minute)

			// Determine if we should delete unhealthy nodes
			nodeOne := nodes[0]
			nodeClaimOne := nodeClaims[0]
			result := ExpectObjectReconciled(ctx, env.Client, healthController, nodeOne)
			Expect(result.RequeueAfter).To(BeNumerically("~", time.Minute*5, time.Second))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaimOne)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())

			nodeTwo := nodes[1]
			nodeClaimTwo := nodeClaims[1]
			result = ExpectObjectReconciled(ctx, env.Client, healthController, nodeTwo)
			Expect(result.RequeueAfter).To(BeNumerically("~", time.Minute*5, time.Second))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaimTwo)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())
		})
		It("should ignore unhealthy nodes if more then 20% of the nodes are unhealthy in a cluster", func() {
			nodeClaims, nodes := test.NodeClaimsAndNodes(10, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{v1.TerminationFinalizer}}})
			for i := range 3 {
				nodes[i].Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
					Type:               "BadNode",
					Status:             corev1.ConditionFalse,
					LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
				})
			}
			for i := range 10 {
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}

			env.Clock.Step(60 * time.Minute)

			// Determine if we should delete unhealthy nodes
			nodeOne := nodes[0]
			nodeClaimOne := nodeClaims[0]
			result := ExpectObjectReconciled(ctx, env.Client, healthController, nodeOne)
			Expect(result.RequeueAfter).To(BeNumerically("~", time.Minute*5, time.Second))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaimOne)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())

			nodeTwo := nodes[1]
			nodeClaimTwo := nodeClaims[1]
			result = ExpectObjectReconciled(ctx, env.Client, healthController, nodeTwo)
			Expect(result.RequeueAfter).To(BeNumerically("~", time.Minute*5, time.Second))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaimTwo)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())

			nodeThree := nodes[2]
			nodeClaimThree := nodeClaims[2]
			result = ExpectObjectReconciled(ctx, env.Client, healthController, nodeThree)
			Expect(result.RequeueAfter).To(BeNumerically("~", time.Minute*5, time.Second))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaimThree)
			Expect(nodeClaim.DeletionTimestamp).To(BeNil())
		})
		It("should consider round up when there is a low number of nodes for a nodepool", func() {
			nodeClaims := []*v1.NodeClaim{}
			nodes := []*corev1.Node{}
			for i := range 3 {
				nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{v1.TerminationFinalizer}}})
				if i == 0 {
					node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
						Type:               "BadNode",
						Status:             corev1.ConditionFalse,
						LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
					})
				}
				node.Labels[v1.NodePoolLabelKey] = nodePool.Name
				nodeClaim.Labels[v1.NodePoolLabelKey] = nodePool.Name
				nodeClaims = append(nodeClaims, nodeClaim)
				nodes = append(nodes, node)
				ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			}

			env.Clock.Step(60 * time.Minute)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, nodes[0])
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaims[0])
			Expect(nodeClaim.DeletionTimestamp).ToNot(BeNil())
		})
	})
	// useReplaceAndRebootPolicies replaces on BadNode, replaces on FatalNode only for the Fatal reason, and reboots on
	// KernelFault.
	useReplaceAndRebootPolicies := func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{ConditionType: "BadNode", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
			{ConditionType: "FatalNode", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Fatal$", TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
			{ConditionType: "KernelFault", ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.RebootNode},
		}
		healthController = legacyrepair.NewController(env.Client, cloudProvider, env.Clock, recorder)
	}
	Context("Breaker", func() {
		var logCtx context.Context
		var blockedLogs func() []string
		// applyNodes applies 10 NodeClaims and Nodes, in the NodePool if inNodePool is set. Node i gets conditions[i].
		applyNodes := func(inNodePool bool, conditions ...corev1.NodeCondition) ([]*v1.NodeClaim, []*corev1.Node) {
			ExpectApplied(ctx, env.Client, nodePool)
			nodeClaims, nodes := test.NodeClaimsAndNodes(10, v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{v1.TerminationFinalizer}}})
			for i := range nodes {
				if i < len(conditions) {
					condition := conditions[i]
					condition.LastTransitionTime = metav1.Time{Time: env.Clock.Now()}
					nodes[i].Status.Conditions = append(nodes[i].Status.Conditions, condition)
				}
				if inNodePool {
					nodes[i].Labels[v1.NodePoolLabelKey] = nodePool.Name
					nodeClaims[i].Labels[v1.NodePoolLabelKey] = nodePool.Name
				}
				ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
			}
			return nodeClaims, nodes
		}
		badNode := corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionFalse}

		BeforeEach(func() {
			logCtx, blockedLogs = CaptureLogs(ctx, "blocking node repair")
		})

		It("should only count nodes that a replace policy matches toward the 20% limit", func() {
			useReplaceAndRebootPolicies()
			// Nodes 1 to 4 would trip the breaker if they counted, but a reboot policy or a reason that doesn't match leaves
			// them healthy here.
			nodeClaims, nodes := applyNodes(true,
				badNode,
				corev1.NodeCondition{Type: "KernelFault", Status: corev1.ConditionFalse},
				corev1.NodeCondition{Type: "KernelFault", Status: corev1.ConditionFalse},
				corev1.NodeCondition{Type: "FatalNode", Status: corev1.ConditionFalse, Reason: "Other"},
				corev1.NodeCondition{Type: "FatalNode", Status: corev1.ConditionFalse, Reason: "Other"},
			)
			env.Clock.Step(60 * time.Minute)

			ExpectObjectReconciled(logCtx, env.Client, healthController, nodes[0])
			Expect(ExpectExists(ctx, env.Client, nodeClaims[0]).DeletionTimestamp).ToNot(BeNil())
			Expect(blockedLogs()).To(BeEmpty())
		})
		It("should log a NodePool that the breaker blocks once, not on every reconcile", func() {
			nodeClaims, nodes := applyNodes(true, badNode, badNode, badNode)
			env.Clock.Step(60 * time.Minute)

			ExpectObjectReconciled(logCtx, env.Client, healthController, nodes[0])
			ExpectObjectReconciled(logCtx, env.Client, healthController, nodes[1])
			Expect(ExpectExists(ctx, env.Client, nodeClaims[0]).DeletionTimestamp).To(BeNil())
			Expect(blockedLogs()).To(HaveLen(1))
			Expect(blockedLogs()[0]).To(ContainSubstring(nodePool.Name))
			Expect(blockedLogs()[0]).To(ContainSubstring(`"nodes"=10 "unhealthy-nodes"=3`))

			env.Clock.Step(health.BlockedRepeatInterval)
			ExpectObjectReconciled(logCtx, env.Client, healthController, nodes[0])
			Expect(blockedLogs()).To(HaveLen(2))
		})
		It("should log once when more than 20% of the cluster's nodes are unhealthy", func() {
			nodeClaims, nodes := applyNodes(false, badNode, badNode, badNode)
			env.Clock.Step(60 * time.Minute)

			ExpectObjectReconciled(logCtx, env.Client, healthController, nodes[0])
			ExpectObjectReconciled(logCtx, env.Client, healthController, nodes[1])
			Expect(ExpectExists(ctx, env.Client, nodeClaims[0]).DeletionTimestamp).To(BeNil())
			Expect(blockedLogs()).To(HaveLen(1))
			Expect(blockedLogs()[0]).To(ContainSubstring(`"nodes"=10 "unhealthy-nodes"=3`))
		})
	})
	Context("UnhealthySince", func() {
		BeforeEach(func() {
			useReplaceAndRebootPolicies()
		})
		It("should be the earliest condition that a replace policy matches", func() {
			now := env.Clock.Now()
			node.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
			node.Status.Conditions = append(node.Status.Conditions,
				corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Minute))},
				corev1.NodeCondition{Type: "FatalNode", Status: corev1.ConditionFalse, Reason: "Fatal", LastTransitionTime: metav1.NewTime(now.Add(-20 * time.Minute))},
				// The legacy controller only replaces nodes, so a reboot policy's condition doesn't count.
				corev1.NodeCondition{Type: "KernelFault", Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-30 * time.Minute))},
			)
			since, unhealthy := healthController.UnhealthySince(node)
			Expect(unhealthy).To(BeTrue())
			Expect(since).To(BeTemporally("==", now.Add(-20*time.Minute)))
		})
		It("should report a node that no replace policy matches as healthy", func() {
			node.Status.Conditions = append(node.Status.Conditions,
				corev1.NodeCondition{Type: "FatalNode", Status: corev1.ConditionFalse, Reason: "Other", LastTransitionTime: metav1.NewTime(env.Clock.Now())},
				corev1.NodeCondition{Type: "KernelFault", Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(env.Clock.Now())},
			)
			_, unhealthy := healthController.UnhealthySince(node)
			Expect(unhealthy).To(BeFalse())
		})
		It("should be no earlier than the node's creation", func() {
			created := env.Clock.Now().Add(-time.Hour)
			node.CreationTimestamp = metav1.NewTime(created)
			// An unset transition time would otherwise date the node's failure to year one.
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{Type: "BadNode", Status: corev1.ConditionFalse})
			since, unhealthy := healthController.UnhealthySince(node)
			Expect(unhealthy).To(BeTrue())
			Expect(since).To(BeTemporally("==", created))
		})
	})
	Context("Metrics", func() {
		It("should fire a karpenter_nodeclaims_disrupted_total metric when unhealthy", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "BadNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: env.Clock.Now()},
			})
			env.Clock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.DeletionTimestamp).ToNot(BeNil())

			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
				metrics.ReasonLabel:              metrics.UnhealthyReason,
				metrics.NodePoolLabel:            nodePool.Name,
				metrics.ConsolidationPolicyLabel: "",
				metrics.TerminationModeLabel:     metrics.TerminationModeGraceful,
			})
			ExpectMetricCounterValue(disruption.NodeClaimsUnhealthyDisruptedTotal, 1, map[string]string{
				disruption.RepairCondition.Name: pretty.ToSnakeCase(string(cloudProvider.RepairPolicies()[0].ConditionType)),
				metrics.NodePoolLabel:           nodePool.Name,
				metrics.TerminationModeLabel:    metrics.TerminationModeGraceful,
			})
		})
	})
})
