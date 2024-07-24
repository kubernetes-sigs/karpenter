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

	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/podevents"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	"sigs.k8s.io/karpenter/pkg/test"
)

var ctx context.Context
var podEventsController *podevents.Controller
var env *test.Environment
var fakeClock *clock.FakeClock
var cp *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...), test.WithFieldIndexers(func(c cache.Cache) error {
		return c.IndexField(ctx, &v1.NodeClaim{}, "status.providerID", func(obj client.Object) []string {
			return []string{obj.(*v1.NodeClaim).Status.ProviderID}
		})
	}))
	ctx = options.ToContext(ctx, test.Options())
	cp = fake.NewCloudProvider()
	podEventsController = podevents.NewController(fakeClock, env.Client)
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
		})
		pod = test.Pod(test.PodOptions{
			NodeName: node.Name,
		})
	})
	It("should set the nodeclaim lastPodEvent", func() {
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		fakeClock.SetTime(pod.CreationTimestamp.Time)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEvent).To(BeEquivalentTo(pod.CreationTimestamp))
	})
	It("should not set the nodeclaim lastPodEvent when the node does not exist", func() {
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, pod)
		fakeClock.SetTime(pod.CreationTimestamp.Time)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEvent).To(BeEquivalentTo(pod.CreationTimestamp))
	})
	It("should not set the nodeclaim lastPodEvent when the nodeclaim does not exist", func() {
		ExpectApplied(ctx, env.Client, nodePool, node, pod)
		fakeClock.SetTime(pod.CreationTimestamp.Time)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEvent).To(BeEquivalentTo(pod.CreationTimestamp))
	})
	It("should only set the nodeclaim lastPodEvent when it hasn't been set before", func() {
		ExpectApplied(ctx, env.Client, nodePool, node, pod)
		fakeClock.SetTime(pod.CreationTimestamp.Time)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEvent.Time).ToNot(BeZero())

		fakeClock.Step(5 * time.Second)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)
	})
	It("should only set the nodeclaim lastPodEvent once within the dedupe timeframe", func() {
		ExpectApplied(ctx, env.Client, nodePool, node, pod)
		fakeClock.SetTime(pod.CreationTimestamp.Time)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEvent).To(BeEquivalentTo(pod.CreationTimestamp.Time))
		lastPodEventTime := nodeClaim.Status.LastPodEvent

		fakeClock.Step(5 * time.Second)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEvent).To(BeEquivalentTo(lastPodEventTime))

		fakeClock.Step(5 * time.Second)
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.LastPodEvent).ToNot(BeEquivalentTo(lastPodEventTime))
	})

	It("should mark NodeClaims as empty that have only pods in terminating state", func() {
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)

		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)

		// Pod owned by a Deployment
		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					},
				},
			},
			NodeName:   node.Name,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		})
		ExpectApplied(ctx, env.Client, lo.Map(pods, func(p *corev1.Pod, _ int) client.Object { return p })...)

		for _, p := range pods {
			// Trigger an eviction to set the deletion timestamp but not delete the pod
			ExpectEvicted(ctx, env.Client, p)
			ExpectExists(ctx, env.Client, p)
		}

		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable).IsTrue()).To(BeTrue())
	})
	It("should mark NodeClaims as empty that have only DaemonSet pods", func() {
		ds := test.DaemonSet()
		ExpectApplied(ctx, env.Client, ds)

		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)

		// Pod owned by a DaemonSet
		pod = test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "DaemonSet",
						Name:               ds.Name,
						UID:                ds.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					},
				},
			},
			NodeName:   node.Name,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		})
		ExpectApplied(ctx, env.Client, pod)

		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable).IsTrue()).To(BeTrue())
	})
	It("should remove the status condition from the nodeClaim when emptiness is disabled", func() {
		nodePool.Spec.Disruption.ConsolidateAfter.Duration = nil
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the nodeClaim initialization condition is unknown", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)
		nodeClaim.StatusConditions().SetUnknown(v1.ConditionTypeInitialized)
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the nodeClaim initialization condition is false", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)
		nodeClaim.StatusConditions().SetFalse(v1.ConditionTypeInitialized, "NotInitialized", "NotInitialized")
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the node doesn't exist", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
	It("should remove the status condition from non-empty NodeClaims", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)

		ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{
			NodeName:   node.Name,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		}))

		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
	It("should remove the status condition from NodeClaims that have a StatefulSet pod in terminating state", func() {
		ss := test.StatefulSet()
		ExpectApplied(ctx, env.Client, ss)

		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)

		// Pod owned by a StatefulSet
		pod = test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "StatefulSet",
						Name:               ss.Name,
						UID:                ss.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					},
				},
			},
			NodeName:   node.Name,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		})
		ExpectApplied(ctx, env.Client, pod)

		// Trigger an eviction to set the deletion timestamp but not delete the pod
		ExpectEvicted(ctx, env.Client, pod)
		ExpectExists(ctx, env.Client, pod)

		// The node isn't empty even though it only has terminating pods
		ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
	It("should remove the status condition when the cluster state node is nominated", func() {
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)

		result := ExpectObjectReconciled(ctx, env.Client, podEventsController, pod)
		Expect(result.RequeueAfter).To(Equal(time.Second * 30))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeConsolidatable)).To(BeNil())
	})
})
