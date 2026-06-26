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

package lifecycle_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/state/nodepoolhealth"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	nodeclaimlifecycle "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

func init() {
	log.SetLogger(logging.NopLogger)
}

var (
	ctx                 context.Context
	nodeClaimController *nodeclaimlifecycle.Controller
	env                 *test.Environment
	cloudProvider       *fake.CloudProvider
	recorder            *test.EventRecorder
	npState             *nodepoolhealth.State
)

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Lifecycle")
}

func removeNodeClaimImmutabilityValidation(crds ...*apiextensionsv1.CustomResourceDefinition) []*apiextensionsv1.CustomResourceDefinition {
	for _, crd := range crds {
		if crd.Name != "nodeclaims.karpenter.sh" {
			continue
		}
		overrideProperties := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
		overrideProperties.XValidations = []apiextensionsv1.ValidationRule{}
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = overrideProperties
	}
	return crds
}

var _ = BeforeSuite(func() {
	recorder = test.NewEventRecorder()
	env = test.NewEnvironment(test.WithCRDs(removeNodeClaimImmutabilityValidation(apis.CRDs...)...), test.WithCRDs(v1alpha1.CRDs...), test.WithFieldIndexers(test.NodeProviderIDFieldIndexer(ctx)))
	ctx = options.ToContext(ctx, test.Options())

	cloudProvider = fake.NewCloudProvider()
	npState = nodepoolhealth.NewState()
	nodeClaimController = nodeclaimlifecycle.NewController(env.Clock, env.Client, cloudProvider, recorder, npState, nil)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	env.Clock.SetTime(time.Now())
	ExpectCleanedUp(ctx, env.Client)
	cloudProvider.Reset()
})

var _ = Describe("Finalizer", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		recorder.Reset() // Reset the events that we captured during the run
		nodePool = test.NodePool()
	})
	Context("TerminationFinalizer", func() {
		It("should add the finalizer if it doesn't exist", func() {
			nodeClaim := test.NodeClaim(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey: nodePool.Name,
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			_, ok := lo.Find(nodeClaim.Finalizers, func(f string) bool {
				return f == v1.TerminationFinalizer
			})
			Expect(ok).To(BeTrue())
		})
		It("shouldn't add the finalizer to NodeClaims not managed by this instance of Karpenter", func() {
			nodeClaim := test.NodeClaim(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey: nodePool.Name,
					},
				},
				Spec: v1.NodeClaimSpec{
					NodeClassRef: &v1.NodeClassReference{
						Group: "karpenter.test.sh",
						Kind:  "UnmanagedNodeClass",
						Name:  "default",
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			_, ok := lo.Find(nodeClaim.Finalizers, func(f string) bool {
				return f == v1.TerminationFinalizer
			})
			Expect(ok).To(BeFalse())
		})
		It("should update status conditions to the latest generation when finalizing", func() {
			nodeClaim := test.NodeClaim(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey: nodePool.Name,
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectMakeNodeClaimsInitialized(ctx, env.Client, env.Clock, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			// add a finalizer so we can make assertions about all status conditions
			ExpectDeletionTimestampSet(ctx, env.Client, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeLaunched).IsTrue()).To(BeTrue())
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeLaunched).ObservedGeneration).To(Equal(nodeClaim.Generation))
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).IsTrue()).To(BeTrue())
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).ObservedGeneration).To(Equal(nodeClaim.Generation))
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).ObservedGeneration).To(Equal(nodeClaim.Generation))
			Expect(nodeClaim.StatusConditions().Get(status.ConditionReady).IsTrue()).To(BeTrue())
			Expect(nodeClaim.StatusConditions().Get(status.ConditionReady).ObservedGeneration).To(Equal(nodeClaim.Generation))

			ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).IsTrue()).To(BeTrue())
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).ObservedGeneration).To(Equal(nodeClaim.Generation))
			ExpectFinalizersRemoved(ctx, env.Client, nodeClaim)
			ExpectDeleted(ctx, env.Client, nodeClaim)
		})
	})
	It("should update observedGeneration if generation increases after all conditions are marked True", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1.NodeClaimSpec{
				Resources: v1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("50Mi"),
						corev1.ResourcePods:   resource.MustParse("5"),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
			Taints:     []corev1.Taint{v1.UnregisteredNoExecuteTaint},
		})
		ExpectApplied(ctx, env.Client, node)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		ExpectMakeNodesReady(ctx, env.Client, env.Clock, node) // Remove the not-ready taint

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsUnknown()).To(BeTrue())

		node = ExpectExists(ctx, env.Client, node)
		node.Status.Capacity = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}
		node.Status.Allocatable = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: resource.MustParse("80Mi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}
		ExpectApplied(ctx, env.Client, node)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())

		// Change a field to increase the generation
		nodeClaim.Spec.Taints = append(nodeClaim.Spec.Taints, corev1.Taint{Key: "test", Value: "value", Effect: corev1.TaintEffectNoSchedule})
		ExpectApplied(ctx, env.Client, nodeClaim)

		// Expect that when the object re-reconciles, all of the observedGenerations across all status condition match
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeLaunched).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeLaunched).ObservedGeneration).To(Equal(nodeClaim.Generation))
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).ObservedGeneration).To(Equal(nodeClaim.Generation))
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).ObservedGeneration).To(Equal(nodeClaim.Generation))

		Expect(nodeClaim.StatusConditions().Get(status.ConditionReady).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(status.ConditionReady).ObservedGeneration).To(Equal(nodeClaim.Generation))
	})
})

var _ = Describe("DRA Initialization Gating", func() {
	var nodePool *v1.NodePool

	const (
		driverA = "driver-a.example.com"
		driverB = "driver-b.example.com"
	)

	BeforeEach(func() {
		// The DRA initialization gate relies on the resource.k8s.io/v1 ResourceSlice API, which only exists on k8s
		// >= 1.34. Skip below that — there is no DRA to gate on.
		if env.Version.Minor() < 34 {
			Skip("DRA is only available in K8s versions >= 1.34.x")
		}
		// Enable DRA so the initialization gate is active.
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{IgnoreDRARequests: lo.ToPtr(false)}))
		nodePool = test.NodePool()
	})

	AfterEach(func() {
		// ResourceSlices are cluster-scoped and not handled by ExpectCleanedUp; remove them between tests. Guard on the
		// version since the ResourceSlice API (and thus DeleteAllOf) isn't served before 1.34.
		if env.Version.Minor() < 34 {
			return
		}
		Expect(env.Client.DeleteAllOf(ctx, &resourcev1.ResourceSlice{})).To(Succeed())
	})

	// registeredReadyNodeClaim creates a NodeClaim with the given requested-dra-drivers annotation, drives it through launch and
	// registration, and returns the NodeClaim and its Ready node. At this point the only thing blocking initialization
	// is the DRA gate (node is Ready, registered, no taints, and requests no extended resources).
	registeredReadyNodeClaim := func(drivers string) (*v1.NodeClaim, *corev1.Node) {
		GinkgoHelper()
		ncOpts := v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}}}
		if drivers != "" {
			ncOpts.Annotations = map[string]string{v1.DRADriversAnnotationKey: drivers}
		}
		nodeClaim := test.NodeClaim(ncOpts)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

		node := test.Node(test.NodeOptions{
			ProviderID: nodeClaim.Status.ProviderID,
			Taints:     []corev1.Taint{v1.UnregisteredNoExecuteTaint},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		ExpectMakeNodesReady(ctx, env.Client, env.Clock, node)
		return nodeClaim, ExpectExists(ctx, env.Client, node)
	}

	// nodeSlice builds a ResourceSlice pinned to the node, declaring a pool with the given driver, pool name, generation,
	// total slice count, and a single device. Apply `count` of these (incrementing the device name) to complete a pool.
	nodeSlice := func(node *corev1.Node, driver, pool string, generation, count int64, device string) *resourcev1.ResourceSlice {
		return test.ResourceSlice(resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("%s-%s-%s", node.Name, pool, device),
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID}},
			},
			Spec: resourcev1.ResourceSliceSpec{
				Driver:   driver,
				NodeName: lo.ToPtr(node.Name),
				Pool:     resourcev1.ResourcePool{Name: pool, Generation: generation, ResourceSliceCount: count},
				Devices:  []resourcev1.Device{{Name: device}},
			},
		})
	}
	reconcile := func(nodeClaim *v1.NodeClaim) *v1.NodeClaim {
		GinkgoHelper()
		ExpectObjectReconciled(ctx, env.Client, nodeClaimController, nodeClaim)
		return ExpectExists(ctx, env.Client, nodeClaim)
	}

	It("holds initialization until the expected driver publishes a complete pool (a)", func() {
		nodeClaim, node := registeredReadyNodeClaim(driverA)

		nodeClaim = reconcile(nodeClaim)
		cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized)
		Expect(cond.IsUnknown()).To(BeTrue())
		Expect(cond.Reason).To(Equal("DRADriverPoolsNotPublished"))

		ExpectApplied(ctx, env.Client, nodeSlice(node, driverA, "pool-a", 1, 1, "dev-0"))
		nodeClaim = reconcile(nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
	})

	It("does not consider an incomplete pool satisfied (b)", func() {
		nodeClaim, node := registeredReadyNodeClaim(driverA)

		// Pool declares 2 slices but only 1 is published.
		ExpectApplied(ctx, env.Client, nodeSlice(node, driverA, "pool-a", 1, 2, "dev-0"))
		nodeClaim = reconcile(nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsUnknown()).To(BeTrue())

		// Publishing the second slice completes the pool.
		ExpectApplied(ctx, env.Client, nodeSlice(node, driverA, "pool-a", 1, 2, "dev-1"))
		nodeClaim = reconcile(nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
	})

	It("does not let stale older-generation slices satisfy a newer incomplete pool (b2)", func() {
		nodeClaim, node := registeredReadyNodeClaim(driverA)

		// Generation 1 was complete (count 1), but generation 2 now declares 2 slices with only 1 published.
		ExpectApplied(ctx, env.Client, nodeSlice(node, driverA, "pool-a", 2, 2, "dev-0"))
		nodeClaim = reconcile(nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsUnknown()).To(BeTrue())
	})

	It("requires every expected driver to publish a complete pool (c)", func() {
		nodeClaim, node := registeredReadyNodeClaim(strings.Join([]string{driverA, driverB}, ","))

		// Only driverA has a complete pool.
		ExpectApplied(ctx, env.Client, nodeSlice(node, driverA, "pool-a", 1, 1, "dev-0"))
		nodeClaim = reconcile(nodeClaim)
		cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized)
		Expect(cond.IsUnknown()).To(BeTrue())
		Expect(cond.Message).To(ContainSubstring(driverB))

		// Publish driverB's pool — now both are satisfied.
		ExpectApplied(ctx, env.Client, nodeSlice(node, driverB, "pool-b", 1, 1, "dev-0"))
		nodeClaim = reconcile(nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
	})

	It("initializes a NodeClaim with no requested-dra-drivers annotation (d)", func() {
		nodeClaim, _ := registeredReadyNodeClaim("")
		nodeClaim = reconcile(nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
	})

	It("skips the gate when DRA is disabled (e)", func() {
		// Even with an annotation and no published slices, a DRA-disabled cluster initializes normally.
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{IgnoreDRARequests: lo.ToPtr(true)}))
		nodeClaim, _ := registeredReadyNodeClaim(driverA)
		nodeClaim = reconcile(nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue()).To(BeTrue())
	})
})
