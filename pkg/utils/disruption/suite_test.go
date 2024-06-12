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
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/ptr"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	. "knative.dev/pkg/logging/testing"

	coreapis "sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/utils/disruption"
)

var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "DisruptionUtils")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(coreapis.CRDs...))
	ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{Drift: lo.ToPtr(true)}}))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Pod Eviction Cost", func() {
	const standardPodCost = 1.0
	It("should have a standard disruptionCost for a pod with no priority or disruptionCost specified", func() {
		cost := disruption.EvictionCost(ctx, &v1.Pod{})
		Expect(cost).To(BeNumerically("==", standardPodCost))
	})
	It("should have a higher disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := disruption.EvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := disruption.EvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "-100",
			}},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
	It("should have higher costs for higher deletion costs", func() {
		cost1 := disruption.EvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "101",
			}},
		})
		cost2 := disruption.EvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		cost3 := disruption.EvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "99",
			}},
		})
		Expect(cost1).To(BeNumerically(">", cost2))
		Expect(cost2).To(BeNumerically(">", cost3))
	})
	It("should have a higher disruptionCost for a pod with a higher priority", func() {
		cost := disruption.EvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: ptr.Int32(1)},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a lower priority", func() {
		cost := disruption.EvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: ptr.Int32(-1)},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
})
