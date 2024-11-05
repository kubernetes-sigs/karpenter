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

package pod_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/controllers/metrics/pod"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var podController *pod.Controller
var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "PodMetrics")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment()
	podController = pod.NewController(env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Pod Metrics", func() {
	It("should update the pod state metrics", func() {
		p := test.Pod()
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))

		_, found := FindMetricWithLabelValues("karpenter_pods_state", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())
	})
	It("should update the pod state metrics with pod phase", func() {
		p := test.Pod()
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))

		_, found := FindMetricWithLabelValues("karpenter_pods_state", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())

		p.Status.Phase = corev1.PodRunning
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))

		_, found = FindMetricWithLabelValues("karpenter_pods_state", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
			"phase":     string(p.Status.Phase),
		})
		Expect(found).To(BeTrue())
	})
	It("should update the pod bound and unbound time metrics", func() {
		p := test.Pod()
		p.Status.Phase = corev1.PodPending

		// PodScheduled condition does not exist, emit pods_current_unbound_time_seconds metric
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p)) //This will add pod to pending pods and unscheduled pods set
		_, found := FindMetricWithLabelValues("karpenter_pods_current_unbound_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())

		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionUnknown, LastTransitionTime: metav1.Now()}}
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p)) //This will add pod to pending pods and unscheduled pods set
		metric, found := FindMetricWithLabelValues("karpenter_pods_current_unbound_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())
		unboundTime := metric.GetGauge().Value

		// Pod is still pending but has bound. At this step pods_unbound_duration should not change.
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p)) //This will check if the pod was scheduled or not
		metric, found = FindMetricWithLabelValues("karpenter_pods_current_unbound_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())
		Expect(metric.GetGauge().Value).To(Equal(unboundTime))

		// Pod is still running and has bound. At this step pods_bound_duration should be fired and pods_current_unbound_time_seconds should be deleted
		p.Status.Phase = corev1.PodRunning
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p)) //This will check if the pod was scheduled or not
		_, found = FindMetricWithLabelValues("karpenter_pods_current_unbound_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeFalse())
		_, found = FindMetricWithLabelValues("karpenter_pods_bound_duration_seconds", map[string]string{})
		Expect(found).To(BeTrue())
	})
	It("should update the pod startup and unstarted time metrics", func() {
		p := test.Pod()
		p.Status.Phase = corev1.PodPending
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p)) //This will add pod to pending pods and unscheduled pods set
		_, found := FindMetricWithLabelValues("karpenter_pods_unstarted_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())

		// Pod is now running but readiness condition is not set
		p.Status.Phase = corev1.PodRunning
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p)) //This will check if the pod was scheduled or not
		_, found = FindMetricWithLabelValues("karpenter_pods_unstarted_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())

		// Pod is now running but readiness is unknown
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionUnknown, LastTransitionTime: metav1.Now()}}
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p)) //This will check if the pod was scheduled or not
		_, found = FindMetricWithLabelValues("karpenter_pods_unstarted_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())

		// Pod is now running and ready. At this step pods_startup_duration should be fired and pods_unstarted_time should be deleted
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p)) //This will check if the pod was scheduled or not
		_, found = FindMetricWithLabelValues("karpenter_pods_unstarted_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeFalse())
		_, found = FindMetricWithLabelValues("karpenter_pods_startup_duration_seconds", nil)
		Expect(found).To(BeTrue())
	})
	It("should delete pod unstarted time and pod unbound duration metric on pod delete", func() {
		p := test.Pod()
		p.Status.Phase = corev1.PodPending
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))
		_, found := FindMetricWithLabelValues("karpenter_pods_current_unbound_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())
		_, found = FindMetricWithLabelValues("karpenter_pods_unstarted_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeTrue())

		ExpectDeleted(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))
		_, found = FindMetricWithLabelValues("karpenter_pods_current_unbound_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeFalse())
		_, found = FindMetricWithLabelValues("karpenter_pods_unstarted_time_seconds", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeFalse())
	})
	It("should delete the pod state metric on pod delete", func() {
		p := test.Pod()
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))

		ExpectDeleted(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(p))

		_, found := FindMetricWithLabelValues("karpenter_pods_state", map[string]string{
			"name":      p.GetName(),
			"namespace": p.GetNamespace(),
		})
		Expect(found).To(BeFalse())
	})
})
