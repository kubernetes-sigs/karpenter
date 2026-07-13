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

package expectations

import (
	"context"
	"time"

	. "github.com/onsi/gomega" //nolint:stylecheck
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
)

func EventuallyExpectCapacityBufferReady(ctx context.Context, c client.Client, buffer *autoscalingv1beta1.CapacityBuffer) {
	Eventually(func(g Gomega) {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
		cond := findCapacityBufferCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
		g.Expect(cond).ToNot(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	}).WithContext(ctx).WithTimeout(30 * time.Second).Should(Succeed())
}

func EventuallyExpectCapacityBufferProvisioned(ctx context.Context, c client.Client, buffer *autoscalingv1beta1.CapacityBuffer) {
	Eventually(func(g Gomega) {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
		cond := findCapacityBufferCondition(cb.Status.Conditions, autoscalingv1beta1.ProvisioningCondition)
		g.Expect(cond).ToNot(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	}).WithContext(ctx).WithTimeout(2 * time.Minute).Should(Succeed())
}

func EventuallyExpectCapacityBufferReplicas(ctx context.Context, c client.Client, buffer *autoscalingv1beta1.CapacityBuffer, replicas int32) {
	Eventually(func(g Gomega) {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
		cond := findCapacityBufferCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
		g.Expect(cond).ToNot(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cb.Status.Replicas).ToNot(BeNil())
		g.Expect(*cb.Status.Replicas).To(Equal(replicas))
	}).WithContext(ctx).WithTimeout(30 * time.Second).Should(Succeed())
}

func EventuallyExpectCapacityBufferNotReady(ctx context.Context, c client.Client, buffer *autoscalingv1beta1.CapacityBuffer, reason string) {
	Eventually(func(g Gomega) {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
		cond := findCapacityBufferCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
		g.Expect(cond).ToNot(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(reason))
	}).WithContext(ctx).WithTimeout(30 * time.Second).Should(Succeed())
}

func EventuallyExpectCapacityBufferNotProvisioned(ctx context.Context, c client.Client, buffer *autoscalingv1beta1.CapacityBuffer) {
	Eventually(func(g Gomega) {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
		cond := findCapacityBufferCondition(cb.Status.Conditions, autoscalingv1beta1.ProvisioningCondition)
		g.Expect(cond).ToNot(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	}).WithContext(ctx).WithTimeout(30 * time.Second).Should(Succeed())
}

func EventuallyExpectCapacityBufferProvisionedWithReason(ctx context.Context, c client.Client, buffer *autoscalingv1beta1.CapacityBuffer, reason string) {
	Eventually(func(g Gomega) {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
		cond := findCapacityBufferCondition(cb.Status.Conditions, autoscalingv1beta1.ProvisioningCondition)
		g.Expect(cond).ToNot(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(reason))
	}).WithContext(ctx).WithTimeout(2 * time.Minute).Should(Succeed())
}

func EventuallyExpectCapacityBufferGenerationUpdated(ctx context.Context, c client.Client, buffer *autoscalingv1beta1.CapacityBuffer, minGeneration int64) {
	Eventually(func(g Gomega) {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(buffer), cb)).To(Succeed())
		g.Expect(cb.Status.PodTemplateGeneration).ToNot(BeNil())
		g.Expect(*cb.Status.PodTemplateGeneration).To(BeNumerically(">=", minGeneration))
	}).WithContext(ctx).WithTimeout(60 * time.Second).Should(Succeed())
}

func findCapacityBufferCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
