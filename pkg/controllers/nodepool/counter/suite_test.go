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

package counter_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/nodepool/counter"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"
)

var provisionerController controller.Controller
var provisionerInformerController controller.Controller
var nodePoolController controller.Controller
var nodePoolInformerController controller.Controller
var nodeClaimController controller.Controller
var machineController controller.Controller
var nodeController controller.Controller
var podController controller.Controller
var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var fakeClock *clock.FakeClock
var cloudProvider *fake.CloudProvider
var node, node2 *v1.Node

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "ProvisionerCounterController")
}

var _ = BeforeSuite(func() {
	cloudProvider = fake.NewCloudProvider()
	nodeclaimutil.EnableNodeClaims = true
	nodepoolutil.EnableNodePools = true
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeClaimController = informer.NewNodeClaimController(env.Client, cluster)
	machineController = informer.NewMachineController(env.Client, cluster)
	nodeController = informer.NewNodeController(env.Client, cluster)
	podController = informer.NewPodController(env.Client, cluster)
	provisionerInformerController = informer.NewProvisionerController(env.Client, cluster)
	nodePoolInformerController = informer.NewNodePoolController(env.Client, cluster)
	provisionerController = counter.NewProvisionerController(env.Client, cluster)
	nodePoolController = counter.NewNodePoolController(env.Client, cluster)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})
