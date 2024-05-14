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

package consistency_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/consistency"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
	"sigs.k8s.io/karpenter/pkg/test"
)

var ctx context.Context
var nodeClaimConsistencyController *consistency.Controller
var env *test.Environment
var fakeClock *clock.FakeClock
var cp *fake.CloudProvider
var recorder *test.EventRecorder

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Consistency")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...), test.WithFieldIndexers(func(c cache.Cache) error {
		return c.IndexField(ctx, &v1.Node{}, "spec.providerID", func(obj client.Object) []string {
			return []string{obj.(*v1.Node).Spec.ProviderID}
		})
	}))
	ctx = options.ToContext(ctx, test.Options())
	cp = &fake.CloudProvider{}
	recorder = test.NewEventRecorder()
	nodeClaimConsistencyController = consistency.NewController(fakeClock, env.Client, recorder)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {

	recorder.Reset()
})

var _ = AfterEach(func() {
	fakeClock.SetTime(time.Now())
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("NodeClaimController", func() {
	var nodePool *v1beta1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
	})
	Context("Termination failure", func() {
		It("should detect issues with a node that is stuck deleting due to a PDB", func() {
			nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:   nodePool.Name,
						v1.LabelInstanceTypeStable: "default-instance-type",
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
						v1.ResourcePods:   resource.MustParse("10"),
					},
				},
			})
			podsLabels := map[string]string{"myapp": "deleteme"}
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         podsLabels,
				MaxUnavailable: &intstr.IntOrString{IntVal: 0, Type: intstr.Int},
			})
			nodeClaim.Finalizers = []string{"prevent.deletion/now"}
			p := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: podsLabels}})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, p, pdb)
			ExpectManualBinding(ctx, env.Client, p, node)
			_ = env.Client.Delete(ctx, nodeClaim)
			ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimConsistencyController), client.ObjectKeyFromObject(nodeClaim))
			Expect(recorder.DetectedEvent(fmt.Sprintf("can't drain node, PDB %q is blocking evictions", client.ObjectKeyFromObject(pdb)))).To(BeTrue())
		})
	})

	Context("Node Shape", func() {
		It("should detect issues that launch with much fewer resources than expected", func() {
			nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:        nodePool.Name,
						v1.LabelInstanceTypeStable:      "arm-instance-type",
						v1beta1.NodeInitializedLabelKey: "true",
					},
				},
				Spec: v1beta1.NodeClaimSpec{
					Resources: v1beta1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("8"),
							v1.ResourceMemory: resource.MustParse("64Gi"),
							v1.ResourcePods:   resource.MustParse("5"),
						},
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("16"),
						v1.ResourceMemory: resource.MustParse("128Gi"),
						v1.ResourcePods:   resource.MustParse("10"),
					},
				},
			})
			node.Status.Capacity = v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("16"),
				v1.ResourceMemory: resource.MustParse("64Gi"),
				v1.ResourcePods:   resource.MustParse("10"),
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
			ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, reconcile.AsReconciler(env.Client, nodeClaimConsistencyController), client.ObjectKeyFromObject(nodeClaim))
			Expect(recorder.DetectedEvent("expected 128Gi of resource memory, but found 64Gi (50.0% of expected)")).To(BeTrue())
		})
	})
})
