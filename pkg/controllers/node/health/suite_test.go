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

package health_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var healthController *health.Controller
var terminationController *termination.Controller
var env *test.Environment
var fakeClock *clock.FakeClock
var cloudProvider *fake.CloudProvider
var recorder *test.EventRecorder
var queue *terminator.Queue
var defaultOwnerRefs = []metav1.OwnerReference{{Kind: "ReplicaSet", APIVersion: "appsv1", Name: "rs", UID: "1234567890"}}

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Termination")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...), test.WithFieldIndexers(test.NodeClaimFieldIndexer(ctx), test.VolumeAttachmentFieldIndexer(ctx)))

	cloudProvider = fake.NewCloudProvider()
	cloudProvider = fake.NewCloudProvider()
	recorder = test.NewEventRecorder()
	queue = terminator.NewTestingQueue(env.Client, recorder)
	healthController = health.NewController(env.Client, cloudProvider, fakeClock)
	terminationController = termination.NewController(fakeClock, env.Client, cloudProvider, terminator.NewTerminator(fakeClock, env.Client, queue, recorder), recorder)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Health", func() {
	var node *corev1.Node
	var nodeClaim *v1.NodeClaim
	var nodePool *v1.NodePool

	BeforeEach(func() {
		fakeClock.SetTime(time.Now())
		cloudProvider.Reset()

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
	})

	Context("Reconcilation", func() {
		It("should delete nodes that are unhealthy by the cloud proivder", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "HealthyNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: fakeClock.Now()},
			})
			fakeClock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			// Let the termination controller terminate the node
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should not reconcile when a node has delation timestamp set", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "HealthyNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: fakeClock.Now()},
			})
			fakeClock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectDeletionTimestampSet(ctx, env.Client, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			// Let the termination controller terminate the node
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectExists(ctx, env.Client, node)
		})
		It("should not delete node when unhealthy type does not match cloudprovider passed in value", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "FakeHealthyNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: fakeClock.Now()},
			})
			fakeClock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			// Let the termination controller terminate the node
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectExists(ctx, env.Client, node)
		})
		It("should not delete node when health status does not match cloudprovider passed in value", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "HealthyNode",
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.Time{Time: fakeClock.Now()},
			})
			fakeClock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			// Let the termination controller terminate the node
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectExists(ctx, env.Client, node)
		})
		It("should not delete node when health duration is not reached", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:   "HealthyNode",
				Status: corev1.ConditionFalse,
				// We expect the last transition for HealthyNode condition to wait 30 minites
				LastTransitionTime: metav1.Time{Time: fakeClock.Now()},
			})
			fakeClock.Step(20 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			// Let the termination controller terminate the node
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectExists(ctx, env.Client, node)
		})
		It("should set termination grace period to now when not defined ", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:   "HealthyNode",
				Status: corev1.ConditionFalse,
				// We expect the last transition for HealthyNode condition to wait 30 minites
				LastTransitionTime: metav1.Time{Time: time.Now()},
			})
			fakeClock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			// Let the termination controller terminate the node
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, fakeClock.Now().Format(time.RFC3339)))
		})
		It("should respect termination grace period if set on the nodepool", func() {
			nodeClaim.Spec.TerminationGracePeriod = &metav1.Duration{Duration: time.Minute}
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:   "HealthyNode",
				Status: corev1.ConditionFalse,
				// We expect the last transition for HealthyNode condition to wait 30 minites
				LastTransitionTime: metav1.Time{Time: time.Now()},
			})
			fakeClock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			// Let the termination controller terminate the node
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.NodeClaimTerminationTimestampAnnotationKey, fakeClock.Now().Format(time.RFC3339)))
		})
	})

	Context("Forceful termination", func() {
		It("should not respect node disruption budgets ", func() {
			// Blocking disruption budgets
			nodePool.Spec.Disruption = v1.Disruption{
				Budgets: []v1.Budget{
					{
						Nodes: "0",
					},
				},
			}
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "HealthyNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: fakeClock.Now()},
			})
			fakeClock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			// Let the termination controller terminate the node
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should not respect do-not-disrupt on node ", func() {
			node.Annotations = map[string]string{v1.DoNotDisruptAnnotationKey: "true"}
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "HealthyNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: fakeClock.Now()},
			})
			fakeClock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			// Determine to delete unhealthy nodes
			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			// Let the termination controller terminate the node
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectNotFound(ctx, env.Client, node)
		})
	})
	Context("Metrics", func() {
		It("should fire a karpenter_nodeclaims_disrupted_total metric when unhealthy", func() {
			node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
				Type:               "HealthyNode",
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: fakeClock.Now()},
			})
			fakeClock.Step(60 * time.Minute)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)

			ExpectObjectReconciled(ctx, env.Client, healthController, node)
			// Let the termination controller terminate the node
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectObjectReconciled(ctx, env.Client, terminationController, node)
			ExpectNotFound(ctx, env.Client, node)

			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
				metrics.ReasonLabel:   metrics.UnhealthyReason,
				metrics.NodePoolLabel: nodePool.Name,
			})
		})
	})
})
