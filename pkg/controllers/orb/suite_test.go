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

package orb_test

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"

	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/orb"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var (
	ctx                    context.Context
	fakeClock              *clock.FakeClock
	cluster                *state.Cluster
	nodeController         *informer.NodeController
	daemonsetController    *informer.DaemonSetController
	cloudProvider          *fake.CloudProvider
	prov                   *provisioning.Provisioner
	env                    *test.Environment
	schedulingInputHeap    *orb.SchedulingInputHeap
	schedulingMetadataHeap *orb.SchedulingMetadataHeap
	instanceTypeMap        map[string]*cloudprovider.InstanceType
)

var (
	reducedPodConditions = []v1.PodCondition{
		{
			Type:    v1.PodReady,
			Status:  v1.ConditionTrue,
			Reason:  "PodTestReason",
			Message: "Testing Pod Condition",
		},
	}
)

func ReducedPodOptions(name, uid string) test.PodOptions {
	return test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)},
		Phase:      v1.PodRunning,
		Conditions: reducedPodConditions,
	}
}

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers/Provisioning")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	ctx = options.ToContext(ctx, test.Options())
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client)
	nodeController = informer.NewNodeController(env.Client, cluster)
	schedulingInputHeap = orb.NewMinHeap[orb.SchedulingInput]()
	prov = provisioning.NewProvisioner(env.Client, events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster, schedulingInputHeap, schedulingMetadataHeap)
	daemonsetController = informer.NewDaemonSetController(env.Client, cluster)
	instanceTypes, _ := cloudProvider.GetInstanceTypes(ctx, nil)
	instanceTypeMap = map[string]*cloudprovider.InstanceType{}
	for _, it := range instanceTypes {
		instanceTypeMap[it.Name] = it
	}
})

var _ = BeforeEach(func() {
	ctx = options.ToContext(ctx, test.Options())
	cloudProvider.Reset()
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cloudProvider.Reset()
	cluster.Reset()
})

// var _ = Describe("Orb", func() {
// 	It("should reduce pods and pod conditions", func() {
// 		ExpectApplied(ctx, env.Client, test.NodePool())

// 		pods := []*v1.Pod{}
// 		for i := 1; i <= 2; i++ {
// 			pods = append(pods, test.Pod(ReducedPodOptions("pod"+string(rune(i)), string(rune(i)))))
// 		}

// 		reducedPods := orb.reducePods(pods)

// 		Expect(len(reducedPods)).To(Equal(2))
// 		for i := 1; i <= 2; i++ {
// 			// Positive case: Check that essential fields are preserved
// 			Expect(reducedPods[i].Name).To(Equal(pods[i].Name))
// 			Expect(reducedPods[i].Namespace).To(Equal(pods[i].Namespace))
// 			Expect(reducedPods[i].UID).To(Equal(pods[i].UID))
// 			Expect(reducedPods[i].Status.Phase).To(Equal(v1.PodRunning))
// 			Expect(len(reducedPods[i].Status.Conditions)).To(Equal(4))
// 			Expect(reducedPods[i].Status.Conditions[0].Type).To(Equal(pods[i].Status.Conditions[0].Type))
// 			Expect(reducedPods[i].Status.Conditions[0].Status).To(Equal(pods[i].Status.Conditions[0].Status))
// 			Expect(reducedPods[i].Status.Conditions[0].Reason).To(Equal(pods[i].Status.Conditions[0].Reason))
// 			Expect(reducedPods[i].Status.Conditions[0].Message).To(Equal(pods[i].Status.Conditions[0].Message))

// 			// Negative case: Spot-check that some non-essential fields are removed
// 			Expect(reducedPods[i].Spec).To(BeEmpty())
// 		}
// 	})

// })
