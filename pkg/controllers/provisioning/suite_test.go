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

package provisioning_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"
)

var ctx context.Context
var fakeClock *clock.FakeClock
var cluster *state.Cluster
var nodeController controller.Controller
var daemonsetController controller.Controller
var cloudProvider *fake.CloudProvider
var prov *provisioning.Provisioner
var env *test.Environment
var instanceTypeMap map[string]*cloudprovider.InstanceType

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers/Provisioning")
}

var _ = BeforeSuite(func() {
	nodeclaimutil.EnableNodeClaims = true
	nodepoolutil.EnableNodePools = true
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	ctx = settings.ToContext(ctx, test.Settings())
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeController = informer.NewNodeController(env.Client, cluster)
	prov = provisioning.NewProvisioner(env.Client, corev1.NewForConfigOrDie(env.Config), events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster)
	daemonsetController = informer.NewDaemonSetController(env.Client, cluster)
	instanceTypes, _ := cloudProvider.GetInstanceTypes(ctx, nil)
	instanceTypeMap = map[string]*cloudprovider.InstanceType{}
	for _, it := range instanceTypes {
		instanceTypeMap[it.Name] = it
	}
})

var _ = BeforeEach(func() {
	ctx = settings.ToContext(ctx, test.Settings())
	cloudProvider.Reset()
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cluster.Reset()
})

func ExpectNodeClaimRequirements(nodeClaim *v1beta1.NodeClaim, requirements ...v1.NodeSelectorRequirement) {
	for _, requirement := range requirements {
		req, ok := lo.Find(nodeClaim.Spec.Requirements, func(r v1.NodeSelectorRequirement) bool {
			return r.Key == requirement.Key && r.Operator == requirement.Operator
		})
		ExpectWithOffset(1, ok).To(BeTrue())

		have := sets.New(req.Values...)
		expected := sets.New(requirement.Values...)
		ExpectWithOffset(1, have.Len()).To(Equal(expected.Len()))
		ExpectWithOffset(1, have.Intersection(expected).Len()).To(Equal(expected.Len()))
	}
}

func ExpectNodeClaimRequests(nodeClaim *v1beta1.NodeClaim, resources v1.ResourceList) {
	for name, value := range resources {
		v := nodeClaim.Spec.Resources.Requests[name]
		ExpectWithOffset(1, v.AsApproximateFloat64()).To(BeNumerically("~", value.AsApproximateFloat64(), 10))
	}
}
