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

package capacitybuffer

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	autoscalingv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	testv1alpha1 "sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx          context.Context
	env          *test.Environment
	cbController *Controller
)

func TestCapacityBuffer(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "CapacityBuffer Controller")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(testv1alpha1.CRDs...))
	cbController = NewController(env.Client)
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("CapacityBuffer Controller", func() {
	It("should set ReadyForProvisioning to True when reconciled with podTemplateRef", func() {
		cb := &autoscalingv1alpha1.CapacityBuffer{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-buffer",
				Namespace: "default",
			},
			Spec: autoscalingv1alpha1.CapacityBufferSpec{
				ProvisioningStrategy: lo.ToPtr("buffer.x-k8s.io/active-capacity"),
				PodTemplateRef:       &autoscalingv1alpha1.LocalObjectRef{Name: "test-template"},
				Replicas:             lo.ToPtr(int32(5)),
			},
		}
		ExpectApplied(ctx, env.Client, cb)
		ExpectObjectReconciled(ctx, env.Client, cbController, cb)

		cb = ExpectExists(ctx, env.Client, cb)
		cond := findCondition(cb.Status.Conditions, ConditionReadyForProvisioning)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Resolved"))
		Expect(cb.Status.ProvisioningStrategy).ToNot(BeNil())
		Expect(*cb.Status.ProvisioningStrategy).To(Equal("buffer.x-k8s.io/active-capacity"))
	})
})

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
