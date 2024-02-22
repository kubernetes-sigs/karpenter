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
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/samber/lo"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/node/termination"
	"github.com/aws/karpenter-core/pkg/controllers/node/termination/terminator"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	. "knative.dev/pkg/logging/testing"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var terminationController controller.Controller
var env *test.Environment
var defaultOwnerRefs = []metav1.OwnerReference{{Kind: "ReplicaSet", APIVersion: "appsv1", Name: "rs", UID: "1234567890"}}
var fakeClock *clock.FakeClock
var cloudProvider *fake.CloudProvider
var recorder *test.EventRecorder
var queue *terminator.Queue

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Termination")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...), test.WithFieldIndexers(test.MachineFieldIndexer(ctx), test.NodeClaimFieldIndexer(ctx)))

	cloudProvider = fake.NewCloudProvider()
	recorder = test.NewEventRecorder()
	queue = terminator.NewQueue(env.Client, recorder)
	terminationController = termination.NewController(env.Client, cloudProvider, terminator.NewTerminator(fakeClock, env.Client, queue), recorder)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

func ExpectNotEnqueuedForEviction(e *terminator.Queue, pods ...*v1.Pod) {
	GinkgoHelper()
	for _, pod := range pods {
		Expect(e.Has(pod)).To(BeFalse())
	}
}

func ExpectEvicted(c client.Client, pods ...*v1.Pod) {
	GinkgoHelper()
	for _, pod := range pods {
		Eventually(func() bool {
			return ExpectPodExists(ctx, c, pod.Name, pod.Namespace).GetDeletionTimestamp().IsZero()
		}, ReconcilerPropagationTime, RequestInterval).Should(BeFalse(), func() string {
			return fmt.Sprintf("expected %s/%s to be evicting, but it isn't", pod.Namespace, pod.Name)
		})
	}
}

func ExpectNodeWithNodeClaimDraining(c client.Client, nodeName string) *v1.Node {
	GinkgoHelper()
	node := ExpectNodeExists(ctx, c, nodeName)
	Expect(node.Spec.Taints).To(ContainElement(v1beta1.DisruptionNoScheduleTaint))
	Expect(lo.Contains(node.Finalizers, v1beta1.TerminationFinalizer)).To(BeTrue())
	Expect(node.DeletionTimestamp.IsZero()).To(BeFalse())
	return node
}

func ExpectNodeWithMachineDraining(c client.Client, nodeName string) *v1.Node {
	GinkgoHelper()
	node := ExpectNodeExists(ctx, c, nodeName)
	Expect(node.Spec.Unschedulable).To(BeTrue())
	Expect(lo.Contains(node.Finalizers, v1alpha5.TerminationFinalizer)).To(BeTrue())
	Expect(node.DeletionTimestamp.IsZero()).To(BeFalse())
	return node
}
