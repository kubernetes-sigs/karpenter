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

package podevents_test

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
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/podevents"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx                 context.Context
	podEventsController *podevents.Controller
	env                 *test.Environment
	fakeClock           *clock.FakeClock
	cp                  *fake.CloudProvider
)

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(
		test.WithCRDs(apis.CRDs...),
		test.WithCRDs(v1alpha1.CRDs...),
		test.WithFieldIndexers(test.NodeClaimProviderIDFieldIndexer(ctx), test.NodeProviderIDFieldIndexer(ctx)),
	)
	ctx = options.ToContext(ctx, test.Options())
	cp = fake.NewCloudProvider()
	podEventsController = podevents.NewController(fakeClock, env.Client, cp)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	ctx = options.ToContext(ctx, test.Options())
	fakeClock.SetTime(time.Now())
})

var _ = AfterEach(func() {
	cp.Reset()
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("PodEvents", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	var pod *corev1.Pod

	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: "default-instance-type", // need the instance type for the cluster state update
				},
			},
			Status: v1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
			},
		})
		pod = test.Pod(test.PodOptions{
			NodeName: node.Name,
		})
	})
	It("should set the nodeclaim lastPodEvent based on PodScheduled condition", func() {
		scheduledTime := fakeClock.Now().Truncate(time.Second)
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: scheduledTime},
		}}
		ExpectApplied(ctx, env.Client, nodePool, node, nodeClaim, pod)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEventTime.Time).To(BeEquivalentTo(scheduledTime))

		// Update the PodScheduled condition's lastTransitionTime
		newScheduledTime := fakeClock.Now().Add(time.Minute).Truncate(time.Second)
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: newScheduledTime},
		}}
		ExpectApplied(ctx, env.Client, pod)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEventTime.Time).To(BeEquivalentTo(newScheduledTime))
	})
	It("should not set the nodeclaim lastPodEvent when the node does not exist", func() {
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, pod)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEventTime.Time).To(BeZero())
	})
	It("should succeed when the nodeclaim lastPodEvent when the nodeclaim does not exist", func() {
		ExpectApplied(ctx, env.Client, nodePool, node, pod)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)
	})
	It("should set the nodeclaim lastPodEvent when pod becomes terminal", func() {
		// First set up a regular pod
		scheduledTime := fakeClock.Now().Truncate(time.Second)
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: scheduledTime},
		}}
		ExpectApplied(ctx, env.Client, nodePool, node, nodeClaim, pod)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		// Make pod terminal now
		timeToCheck := fakeClock.Now().Add(time.Minute).Truncate(time.Second)
		fakeClock.SetTime(timeToCheck)
		pod.Status.Phase = corev1.PodSucceeded // Setting pod as terminal directly
		ExpectApplied(ctx, env.Client, pod)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEventTime.Time).To(BeEquivalentTo(timeToCheck))
	})
	It("should set the nodeclaim lastPodEvent when pod becomes terminating", func() {
		ExpectApplied(ctx, env.Client, nodePool, node, nodeClaim, pod)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		timeToCheck := fakeClock.Now().Truncate(time.Second)
		fakeClock.SetTime(timeToCheck)
		// Make pod terminating by deleting it
		ExpectDeletionTimestampSet(ctx, env.Client, pod)
		// Reconcile for the terminating pod
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEventTime.Time).To(BeEquivalentTo(timeToCheck))
	})
	It("should not update lastPodEvent when pod has no PodScheduled condition", func() {
		// Pod with no conditions
		ExpectApplied(ctx, env.Client, nodePool, node, nodeClaim, pod)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEventTime.Time.IsZero()).To(BeTrue())
	})
	It("should not update lastPodEvent when PodScheduled condition is not True", func() {
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionFalse,
			LastTransitionTime: metav1.Time{Time: fakeClock.Now()},
		}}
		ExpectApplied(ctx, env.Client, nodePool, node, nodeClaim, pod)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEventTime.Time.IsZero()).To(BeTrue())
	})
	It("should set the nodeclaim lastPodEvent when pod is already in a terminal state", func() {
		// Setup time
		timeToCheck := fakeClock.Now().Truncate(time.Second)
		fakeClock.SetTime(timeToCheck)

		// Set the pod to a terminal state directly - mocks podutils.IsTerminal() return true
		pod.Status.Phase = corev1.PodSucceeded

		// Apply objects and reconcile
		ExpectApplied(ctx, env.Client, nodePool, node, nodeClaim, pod)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		// Verify the last pod event time is set
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEventTime.Time).To(BeEquivalentTo(timeToCheck))
	})
})
