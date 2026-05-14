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

package v1alpha1_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/test"
	testexpectations "sigs.k8s.io/karpenter/pkg/test/expectations"
	testv1alpha1 "sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "v1alpha1")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(testv1alpha1.CRDs...))
})

var _ = AfterEach(func() {
	testexpectations.ExpectCleanedUp(ctx, env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("NodeOverlay", func() {
	Context("OrderByWeight", func() {
		It("should order the NodeOverlay by weight", func() {
			nos := lo.Times(10, func(i int) v1alpha1.NodeOverlay {
				return v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("overlay-%d", i)},
					Spec: v1alpha1.NodeOverlaySpec{
						Weight: lo.ToPtr[int32](int32(rand.IntN(100) + 1)), //nolint:gosec
						Requirements: []v1alpha1.NodeSelectorRequirement{
							{
								Key:      "test",
								Operator: corev1.NodeSelectorOpExists,
							},
						},
					},
				}
			})
			overlayList := &v1alpha1.NodeOverlayList{Items: nos}
			overlayList.OrderByWeight()

			lastWeight := 101 // This is above the allowed weight values
			for _, overlay := range overlayList.Items {
				Expect(lo.FromPtr(overlay.Spec.Weight)).To(BeNumerically("<=", lastWeight))
				lastWeight = int(lo.FromPtr(overlay.Spec.Weight))
			}
		})
		It("should order the NodeOverlay by name when the weights are the same", func() {
			nos := lo.Times(10, func(i int) v1alpha1.NodeOverlay {
				return v1alpha1.NodeOverlay{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("overlay-%d", i)},
					Spec: v1alpha1.NodeOverlaySpec{
						Weight: lo.ToPtr[int32](10),
						Requirements: []v1alpha1.NodeSelectorRequirement{
							{
								Key:      "test",
								Operator: corev1.NodeSelectorOpExists,
							},
						},
					},
				}
			})
			overlayList := &v1alpha1.NodeOverlayList{Items: nos}
			overlayList.OrderByWeight()

			lastName := "zzzzzzzzzzzzzzzzzzzzzzzz" // large string value
			for _, overlay := range overlayList.Items {
				Expect(overlay.Name < lastName).To(BeTrue())
				lastName = overlay.Name
			}
		})
	})
})
