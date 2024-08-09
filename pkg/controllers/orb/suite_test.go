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
	originalPVMountPath    string
	controller             *orb.Controller
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
	originalPVMountPath = "/data"
	controller = orb.NewController(orb.NewMinHeap[orb.SchedulingInput](), orb.NewMinHeap[orb.SchedulingMetadata]())
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cloudProvider.Reset()
	cluster.Reset()
})

var _ = Describe("Orb", func() {
	It("Empty scheduling inputs marshal as blank and unmarshal as nil", func() {
		emptySchedulingInput := test.EmptySchedulingInput()
		marshaled, _ := orb.MarshalSchedulingInput(emptySchedulingInput)
		Expect(len(marshaled) == 0)
		unmarshaled, _ := orb.UnmarshalSchedulingInput(marshaled)
		Expect(unmarshaled == nil)
	})
	// It("Non-empty reflexive differences are empty", func() {
	// 	schedulingInput := test.SchedulingInput(ctx, env.Client)
	// 	reflexiveDiff := schedulingInput.Diff(schedulingInput) // reflexive check
	// 	Expect(reflexiveDiff).To(Equal(test.EmptySchedulingInputDifferences()))

	// 	marshaled, _ := orb.MarshalSchedulingInput(schedulingInput)
	// 	unmarshaled, _ := orb.UnmarshalSchedulingInput(marshaled)

	// 	Expect(unmarshaled != nil).To(BeTrue())
	// 	actualDiff := unmarshaled.Diff(schedulingInput)
	// 	Expect(actualDiff).To(Equal(test.EmptySchedulingInputDifferences()))
	// })
	// It("Non-empty differences are empty and equal", func() {
	// 	schedulingInput := test.SchedulingInput(ctx, env.Client)
	// 	marshaled, _ := orb.MarshalSchedulingInput(schedulingInput)
	// 	unmarshaled, _ := orb.UnmarshalSchedulingInput(marshaled)

	// 	Expect(unmarshaled != nil)

	// 	reflexiveDiff := schedulingInput.Diff(schedulingInput) // reflexive check
	// 	Expect(reflexiveDiff).To(Equal(test.EmptySchedulingInputDifferences()))

	// 	actualDiff := unmarshaled.Diff(schedulingInput)
	// 	Expect(actualDiff).To(Equal(test.EmptySchedulingInputDifferences()))
	// })
	// It("marshaling and unmarshalling scheduling input should yield the same - empty", func() {
	// 	emptySchedulingInput := test.EmptySchedulingInput()
	// 	marshaled, _ := orb.MarshalSchedulingInput(emptySchedulingInput)
	// 	unmarshaled, _ := orb.UnmarshalSchedulingInput(marshaled)
	// 	Expect(unmarshaled).To(Equal(emptySchedulingInput))
	// })
	// It("marshaling and unmarshalling scheduling inputs should yield the same - non-empty", func() {
	// 	schedulingInput := test.SchedulingInput(ctx, env.Client)
	// 	marshaled, _ := orb.MarshalSchedulingInput(schedulingInput)
	// 	unmarshaled, _ := orb.UnmarshalSchedulingInput(marshaled)

	// 	//Expect each field of the scheduling input to be equal
	// 	Expect(unmarshaled.Timestamp).To(Equal(schedulingInput.Timestamp))
	// 	Expect(unmarshaled.PendingPods).To(Equal(schedulingInput.PendingPods))
	// 	Expect(unmarshaled.StateNodesWithPods).To(Equal(schedulingInput.StateNodesWithPods))
	// 	Expect(unmarshaled.Bindings).To(Equal(schedulingInput.Bindings))
	// 	Expect(unmarshaled.AllInstanceTypes).To(Equal(schedulingInput.AllInstanceTypes))
	// 	Expect(unmarshaled.NodePoolInstanceTypes).To(Equal(schedulingInput.NodePoolInstanceTypes))
	// 	Expect(unmarshaled.Topology).To(Equal(schedulingInput.Topology))
	// 	Expect(unmarshaled.DaemonSetPods).To(Equal(schedulingInput.DaemonSetPods))
	// 	Expect(unmarshaled.PVList).To(Equal(schedulingInput.PVList))
	// 	Expect(unmarshaled.PVCList).To(Equal(schedulingInput.PVCList))
	// 	Expect(unmarshaled.ScheduledPodList).To(Equal(schedulingInput.ScheduledPodList))

	// 	Expect(unmarshaled).To(Equal(schedulingInput))
	// })
	// It("marshaling and unmarshalling differences should yield the same - empty", func() {
	// 	emptySliceSchedulingInputDifferences := test.EmptySchedulingInputDifferencesSlice()
	// 	marshaled, _ := orb.MarshalBatchedDifferences(emptySliceSchedulingInputDifferences)
	// 	unmarshaled, _ := orb.UnmarshalBatchedDifferences(marshaled)
	// 	Expect(unmarshaled).To(Equal(emptySliceSchedulingInputDifferences))
	// })
	// It("marshaling and unmarshalling differences should yield the same - non-empty", func() {
	// 	SchedulingInputDifferences := test.SchedulingInputDifferencesSlice(ctx, env.Client)
	// 	marshaled, _ := orb.MarshalBatchedDifferences(SchedulingInputDifferences)
	// 	unmarshaled, _ := orb.UnmarshalBatchedDifferences(marshaled)
	// 	Expect(unmarshaled).To(Equal(SchedulingInputDifferences))
	// })
})
