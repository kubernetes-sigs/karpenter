/*
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

package controller_test

import (
	"context"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Typed", func() {
	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
	})

	It("should pass in expected node into reconcile", func() {
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, node)
		fakeController := &FakeTypedController[*v1.Node]{
			ReconcileAssertions: []TypedReconcileAssertion[*v1.Node]{
				func(ctx context.Context, n *v1.Node) {
					Expect(n.Name).To(Equal(node.Name))
					Expect(n.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, "default"))
				},
			},
		}
		typedController := controller.Typed[*v1.Node](env.Client, fakeController)
		ExpectReconcileSucceeded(ctx, typedController, client.ObjectKeyFromObject(node))
	})
	It("should call finalizer func when finalizing", func() {
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: "default",
				},
				Finalizers: []string{
					v1alpha5.TestingGroup + "/finalizer",
				},
			},
		})
		ExpectApplied(ctx, env.Client, node)
		Expect(env.Client.Delete(ctx, node)).To(Succeed())

		called := false
		fakeController := &FakeTypedController[*v1.Node]{
			FinalizeAssertions: []TypedReconcileAssertion[*v1.Node]{
				func(ctx context.Context, n *v1.Node) {
					called = true
				},
			},
		}
		typedController := controller.Typed[*v1.Node](env.Client, fakeController)
		ExpectReconcileSucceeded(ctx, typedController, client.ObjectKeyFromObject(node))
		Expect(called).To(BeTrue())
	})
})

type TypedReconcileAssertion[T client.Object] func(context.Context, T)

type FakeTypedController[T client.Object] struct {
	ReconcileAssertions []TypedReconcileAssertion[T]
	FinalizeAssertions  []TypedReconcileAssertion[T]
}

func (c *FakeTypedController[T]) Name() string {
	return ""
}

func (c *FakeTypedController[T]) Reconcile(ctx context.Context, obj T) (reconcile.Result, error) {
	for _, elem := range c.ReconcileAssertions {
		elem(ctx, obj)
	}
	return reconcile.Result{}, nil
}

func (c *FakeTypedController[T]) Finalize(ctx context.Context, obj T) (reconcile.Result, error) {
	for _, elem := range c.FinalizeAssertions {
		elem(ctx, obj)
	}
	return reconcile.Result{}, nil
}

func (c *FakeTypedController[T]) Builder(_ context.Context, _ manager.Manager) controller.Builder {
	return nil
}
