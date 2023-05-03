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

package disruption_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/machine/disruption"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/test"
)

var ctx context.Context
var disruptionController controller.Controller
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
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...), test.WithFieldIndexers(func(c cache.Cache) error {
		return c.IndexField(ctx, &v1.Node{}, "spec.providerID", func(obj client.Object) []string {
			return []string{obj.(*v1.Node).Spec.ProviderID}
		})
	}))
	ctx = settings.ToContext(ctx, test.Settings())
	cp = fake.NewCloudProvider()
	disruptionController = disruption.NewController(fakeClock, env.Client, cp)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: true}))
})

var _ = AfterEach(func() {
	fakeClock.SetTime(time.Now())
	cp.Reset()
	ExpectCleanedUp(ctx, env.Client)
})

//var _ = Describe("Filters", func() {
//	var provisioner *v1alpha5.Provisioner
//	BeforeEach(func() {
//		provisioner = test.Provisioner()
//	})
//
//	Context("Filters", func() {
//		BeforeEach(func() {
//			innerCtx, cancel := context.WithCancel(ctx)
//			DeferCleanup(func() {
//				cancel()
//			})
//			mgr, err := controllerruntime.NewManager(env.Config, controllerruntime.Options{
//				Scheme:             env.Scheme,
//				MetricsBindAddress: "0",
//			})
//			Expect(err).ToNot(HaveOccurred())
//			Expect(nodeController.Builder(innerCtx, mgr).Complete(nodeController)).To(Succeed())
//			go func() {
//				defer GinkgoRecover()
//				Expect(mgr.Start(innerCtx)).To(Succeed())
//			}()
//		})
//		It("should do nothing if the not owned by a provisioner", func() {
//			n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
//				Finalizers: []string{"fake.com/finalizer"},
//			}})
//			ExpectApplied(ctx, env.Client, provisioner, n)
//
//			// Node shouldn't reconcile anything onto it
//			Consistently(func(g Gomega) {
//				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: n.Name}, &v1.Node{})).To(Succeed())
//				g.Expect(n.Finalizers).To(Equal(n.Finalizers))
//			})
//		})
//		It("should do nothing if deletion timestamp is set", func() {
//			n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
//				Finalizers: []string{"fake.com/finalizer"},
//			}})
//			ExpectApplied(ctx, env.Client, provisioner, n)
//			Expect(env.Client.Delete(ctx, n)).To(Succeed())
//
//			// Update the node to be provisioned by the provisioner through labels
//			n.Labels = map[string]string{
//				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
//			}
//			ExpectApplied(ctx, env.Client, n)
//
//			// Node shouldn't reconcile anything onto it
//			Consistently(func(g Gomega) {
//				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: n.Name}, &v1.Node{})).To(Succeed())
//				g.Expect(n.Finalizers).To(Equal(n.Finalizers))
//			})
//		})
//	})
//})
