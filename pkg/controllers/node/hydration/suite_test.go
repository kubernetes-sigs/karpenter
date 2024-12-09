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

package hydration_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/node/hydration"
	"sigs.k8s.io/karpenter/pkg/operator"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var hydrationController *hydration.Controller
var env *test.Environment
var cloudProvider *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Lifecycle")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...), test.WithFieldIndexers(test.NodeProviderIDFieldIndexer(ctx), test.NodeClaimProviderIDFieldIndexer(ctx)))
	ctx = options.ToContext(ctx, test.Options())

	cloudProvider = fake.NewCloudProvider()
	hydrationController = hydration.NewController(env.Client, cloudProvider)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cloudProvider.Reset()
})

var _ = Describe("Hydration", func() {
	It("should hydrate the NodeClass label", func() {
		nc, n := test.NodeClaimAndNode()
		delete(n.Labels, v1.NodeClassLabelKey(nc.Spec.NodeClassRef.GroupKind()))
		ExpectApplied(ctx, env.Client, nc, n)
		ExpectObjectReconciled(ctx, env.Client, hydrationController, n)
		n = ExpectExists(ctx, env.Client, n)
		value := n.Labels[v1.NodeClassLabelKey(nc.Spec.NodeClassRef.GroupKind())]
		Expect(value).To(Equal(nc.Spec.NodeClassRef.Name))
	})
	DescribeTable(
		"Finalizers",
		func(nodeClaimConditions []string, expectedFinailzers []string) {
			nc, n := test.NodeClaimAndNode()
			for _, cond := range nodeClaimConditions {
				nc.StatusConditions().SetTrue(cond)
			}
			ExpectApplied(ctx, env.Client, nc, n)
			ExpectObjectReconciled(ctx, env.Client, hydrationController, n)
			n = ExpectExists(ctx, env.Client, n)
			Expect(len(n.Finalizers)).To(Equal(len(expectedFinailzers)))
			for _, finalizer := range expectedFinailzers {
				Expect(controllerutil.ContainsFinalizer(n, finalizer))
			}
		},
		Entry("should hydrate all finalizers when none of the requisite status conditions are true", nil, []string{v1.DrainFinalizer, v1.VolumeFinalizer}),
		Entry("should hydrate the volume finalizer when only the drain status condition is true", []string{v1.ConditionTypeDrained}, []string{v1.VolumeFinalizer}),
		Entry("should hydrate the drain finalizer when only the volume status condition is true", []string{v1.ConditionTypeVolumesDetached}, []string{v1.VolumeFinalizer}),
		Entry("shouldn't hydrate finalizers when all requisite conditions are true", []string{v1.ConditionTypeDrained, v1.ConditionTypeVolumesDetached}, nil),
	)

	It("shouldn't hydrate nodes which have already been hydrated", func() {
		nc, n := test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1.HydrationAnnotationKey: operator.Version,
				},
			},
		})
		delete(n.Labels, v1.NodeClassLabelKey(nc.Spec.NodeClassRef.GroupKind()))
		ExpectApplied(ctx, env.Client, nc, n)
		ExpectObjectReconciled(ctx, env.Client, hydrationController, n)
		n = ExpectExists(ctx, env.Client, n)
		Expect(lo.Keys(n.Labels)).ToNot(ContainElement(v1.NodeClassLabelKey(nc.Spec.NodeClassRef.GroupKind())))
		Expect(len(n.Finalizers)).To(Equal(0))
	})
	It("shouldn't hydrate nodes which are not managed by this instance of Karpenter", func() {
		nc, n := test.NodeClaimAndNode(v1.NodeClaim{
			Spec: v1.NodeClaimSpec{
				NodeClassRef: &v1.NodeClassReference{
					Group: "karpenter.test.sh",
					Kind: "UnmanagedNodeClass",
					Name: "default",
				},
			},
		})
		delete(n.Labels, v1.NodeClassLabelKey(nc.Spec.NodeClassRef.GroupKind()))
		ExpectApplied(ctx, env.Client, nc, n)
		ExpectObjectReconciled(ctx, env.Client, hydrationController, n)
		n = ExpectExists(ctx, env.Client, n)
		Expect(lo.Keys(n.Labels)).ToNot(ContainElement(v1.NodeClassLabelKey(nc.Spec.NodeClassRef.GroupKind())))
		Expect(len(n.Finalizers)).To(Equal(0))
	})
})
