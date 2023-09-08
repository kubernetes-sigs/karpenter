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

package garbagecollection_test

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	nodeclaimgarbagecollection "github.com/aws/karpenter-core/pkg/controllers/nodeclaim/garbagecollection"
	nodeclaimlifcycle "github.com/aws/karpenter-core/pkg/controllers/nodeclaim/lifecycle"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var machineController controller.Controller
var nodeClaimController controller.Controller
var garbageCollectionController controller.Controller
var env *test.Environment
var fakeClock *clock.FakeClock
var cloudProvider *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Machine")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...), test.WithFieldIndexers(func(c cache.Cache) error {
		return c.IndexField(ctx, &v1.Node{}, "spec.providerID", func(obj client.Object) []string {
			return []string{obj.(*v1.Node).Spec.ProviderID}
		})
	}))
	ctx = settings.ToContext(ctx, test.Settings())
	cloudProvider = fake.NewCloudProvider()
	garbageCollectionController = nodeclaimgarbagecollection.NewController(fakeClock, env.Client, cloudProvider)
	machineController = nodeclaimlifcycle.NewMachineController(fakeClock, env.Client, cloudProvider, events.NewRecorder(&record.FakeRecorder{}))
	nodeClaimController = nodeclaimlifcycle.NewNodeClaimController(fakeClock, env.Client, cloudProvider, events.NewRecorder(&record.FakeRecorder{}))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	fakeClock.SetTime(time.Now())
	ExpectCleanedUp(ctx, env.Client)
	cloudProvider.Reset()
})

var _ = Describe("Combined/GarbageCollection", func() {
	var provisioner *v1alpha5.Provisioner
	var nodePool *v1beta1.NodePool

	BeforeEach(func() {
		provisioner = test.Provisioner()
		nodePool = test.NodePool()
	})

	It("should delete both NodeClaims and Machines when the Node never appears and the instance is gone", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
		})
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		machine = ExpectExists(ctx, env.Client, machine)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		// Step forward to move past the cache eventual consistency timeout
		fakeClock.SetTime(time.Now().Add(time.Second * 20))

		// Delete the NodeClaim and Machine from the cloudprovider
		Expect(cloudProvider.Delete(ctx, nodeclaimutil.New(machine))).To(Succeed())
		Expect(cloudProvider.Delete(ctx, nodeClaim)).To(Succeed())

		// Expect the NodeClaim to be removed now that the Instance is gone
		ExpectReconcileSucceeded(ctx, garbageCollectionController, client.ObjectKey{})
		ExpectFinalizersRemoved(ctx, env.Client, machine)
		ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
		ExpectNotFound(ctx, env.Client, machine, nodeClaim)
	})
})
