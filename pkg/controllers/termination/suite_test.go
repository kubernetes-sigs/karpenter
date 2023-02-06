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

package termination_test

import (
	"context"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/machine/terminator"
	"github.com/aws/karpenter-core/pkg/controllers/termination"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var terminationController controller.Controller
var env *test.Environment
var fakeClock *clock.FakeClock
var defaultOwnerRefs = []metav1.OwnerReference{{Kind: "ReplicaSet", APIVersion: "appsv1", Name: "rs", UID: "1234567890"}}

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Termination")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...), test.WithFieldIndexers(func(c cache.Cache) error {
		return c.IndexField(ctx, &v1alpha5.Machine{}, "status.providerID", func(obj client.Object) []string {
			return []string{obj.(*v1alpha5.Machine).Status.ProviderID}
		})
	}))
	evictionQueue := terminator.NewEvictionQueue(ctx, env.KubernetesInterface.CoreV1(), events.NewRecorder(&record.FakeRecorder{}))
	terminator := terminator.NewTerminator(fakeClock, env.Client, evictionQueue)
	terminationController = termination.NewController(env.Client, terminator, events.NewRecorder(&record.FakeRecorder{}))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Termination", func() {
	var machine *v1alpha5.Machine
	var node *v1.Node

	BeforeEach(func() {
		machine = test.Machine(v1alpha5.Machine{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{v1alpha5.TerminationFinalizer}}, Status: v1alpha5.MachineStatus{ProviderID: test.RandomProviderID()}})
		node = test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{v1alpha5.TerminationFinalizer}}, ProviderID: machine.Status.ProviderID})
	})
	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
		fakeClock.SetTime(time.Now())
	})
	Context("Reconciliation", func() {
		It("should not delete node if machine still exists", func() {
			ExpectApplied(ctx, env.Client, machine, node)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectExists(ctx, env.Client, node)
		})
		It("should delete nodes if machine doesn't exist", func() {
			ExpectApplied(ctx, env.Client, node)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should not race if deleting nodes in parallel if machines don't exist", func() {
			var nodes []*v1.Node
			for i := 0; i < 10; i++ {
				node = test.Node(test.NodeOptions{
					ObjectMeta: metav1.ObjectMeta{
						Finalizers: []string{v1alpha5.TerminationFinalizer},
					},
				})
				ExpectApplied(ctx, env.Client, node)
				Expect(env.Client.Delete(ctx, node)).To(Succeed())
				node = ExpectNodeExists(ctx, env.Client, node.Name)
				nodes = append(nodes, node)
			}

			var wg sync.WaitGroup
			// this is enough to trip the race detector
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func(node *v1.Node) {
					defer GinkgoRecover()
					defer wg.Done()
					ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
				}(nodes[i])
			}
			wg.Wait()
			ExpectNotFound(ctx, env.Client, lo.Map(nodes, func(n *v1.Node, _ int) client.Object { return n })...)
		})
	})
})
