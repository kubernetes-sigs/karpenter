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

package node_test

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clock "k8s.io/utils/clock/testing"
	"knative.dev/pkg/ptr"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"

	"github.com/aws/karpenter-core/pkg/controllers/node"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/test"
)

var ctx context.Context
var nodeController controller.Controller
var env *test.Environment
var fakeClock *clock.FakeClock
var cluster *state.Cluster
var cp *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Node")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	ctx = settings.ToContext(ctx, test.Settings())
	cp = fake.NewCloudProvider()
	cluster = state.NewCluster(fakeClock, env.Client, cp)
	nodeController = node.NewController(fakeClock, env.Client, cp, cluster)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Controller", func() {
	var provisioner *v1alpha5.Provisioner
	BeforeEach(func() {
		provisioner = &v1alpha5.Provisioner{
			ObjectMeta: metav1.ObjectMeta{Name: test.RandomName()},
			Spec:       v1alpha5.ProvisionerSpec{},
		}
		ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: true}))
	})

	AfterEach(func() {
		fakeClock.SetTime(time.Now())
		cp.Reset()
		ExpectCleanedUp(ctx, env.Client)
	})

	Context("Drift", func() {
		It("should not detect drift if the feature flag is disabled", func() {
			cp.Drifted = true
			ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: false}))
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       test.RandomName(),
					},
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).ToNot(HaveKeyWithValue(v1alpha5.VoluntaryDisruptionAnnotationKey, v1alpha5.VoluntaryDisruptionDriftedAnnotationValue))
		})
		It("should not detect drift if the provisioner does not exist", func() {
			cp.Drifted = true
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       test.RandomName(),
					},
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).ToNot(HaveKeyWithValue(v1alpha5.VoluntaryDisruptionAnnotationKey, v1alpha5.VoluntaryDisruptionDriftedAnnotationValue))
		})
		It("should annotate the node when it has drifted in the cloud provider", func() {
			cp.Drifted = true
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       test.RandomName(),
					},
				},
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).To(HaveKeyWithValue(v1alpha5.VoluntaryDisruptionAnnotationKey, v1alpha5.VoluntaryDisruptionDriftedAnnotationValue))
		})
		It("should remove the annotation from nodes if drift is disabled", func() {
			cp.Drifted = true
			ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: false}))
			node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
				Annotations: map[string]string{
					v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionDriftedAnnotationValue,
				},
			}})
			ExpectApplied(ctx, env.Client, provisioner, node)

			// step forward to make the node expired
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).ToNot(HaveKey(v1alpha5.VoluntaryDisruptionAnnotationKey))
		})
	})

	Context("Initialization", func() {
		It("should initialize the node when ready", func() {
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					},
				},
				ReadyStatus: v1.ConditionTrue,
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Labels).To(HaveKey(v1alpha5.LabelNodeInitialized))
		})
		It("should not initialize the node when not ready", func() {
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					},
				},
				ReadyStatus: v1.ConditionFalse,
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Labels).ToNot(HaveKey(v1alpha5.LabelNodeInitialized))
		})
		It("should initialize the node when extended resources are registered", func() {
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       "gpu-vendor-instance-type",
					},
				},
				ReadyStatus: v1.ConditionTrue,
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:          resource.MustParse("4"),
					v1.ResourceMemory:       resource.MustParse("4Gi"),
					v1.ResourcePods:         resource.MustParse("5"),
					fake.ResourceGPUVendorA: resource.MustParse("2"),
				},
				Capacity: v1.ResourceList{
					v1.ResourceCPU:          resource.MustParse("4"),
					v1.ResourceMemory:       resource.MustParse("4Gi"),
					v1.ResourcePods:         resource.MustParse("5"),
					fake.ResourceGPUVendorA: resource.MustParse("2"),
				},
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Labels).To(HaveKey(v1alpha5.LabelNodeInitialized))
		})
		It("should not initialize the node when extended resource isn't registered", func() {
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       "gpu-vendor-instance-type",
					},
				},
				ReadyStatus: v1.ConditionTrue,
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("4Gi"),
					v1.ResourcePods:   resource.MustParse("5"),
				},
				Capacity: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("4Gi"),
					v1.ResourcePods:   resource.MustParse("5"),
				},
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Labels).ToNot(HaveKey(v1alpha5.LabelNodeInitialized))
		})
		It("should not initialize the node when capacity is filled but allocatable isn't set", func() {
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       "gpu-vendor-instance-type",
					},
				},
				ReadyStatus: v1.ConditionTrue,
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("4"),
					v1.ResourceMemory: resource.MustParse("4Gi"),
					v1.ResourcePods:   resource.MustParse("5"),
				},
				Capacity: v1.ResourceList{
					v1.ResourceCPU:          resource.MustParse("4"),
					v1.ResourceMemory:       resource.MustParse("4Gi"),
					v1.ResourcePods:         resource.MustParse("5"),
					fake.ResourceGPUVendorA: resource.MustParse("2"),
				},
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Labels).ToNot(HaveKey(v1alpha5.LabelNodeInitialized))
		})
		It("should initialize the node when startup taints are removed", func() {
			provisioner.Spec.StartupTaints = []v1.Taint{
				{
					Key:    "example.com/startup-taint1",
					Value:  "true",
					Effect: v1.TaintEffectNoExecute,
				},
				{
					Key:    "example.com/startup-taint1",
					Value:  "true",
					Effect: v1.TaintEffectNoSchedule,
				},
				{
					Key:    "example.com/startup-taint2",
					Value:  "true",
					Effect: v1.TaintEffectNoExecute,
				},
			}
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					},
				},
				ReadyStatus: v1.ConditionTrue,
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Labels).To(HaveKey(v1alpha5.LabelNodeInitialized))
		})
		It("should not initialize the node when startup taints aren't removed", func() {
			provisioner.Spec.StartupTaints = []v1.Taint{
				{
					Key:    "example.com/startup-taint1",
					Value:  "true",
					Effect: v1.TaintEffectNoExecute,
				},
				{
					Key:    "example.com/startup-taint1",
					Value:  "true",
					Effect: v1.TaintEffectNoSchedule,
				},
				{
					Key:    "example.com/startup-taint2",
					Value:  "true",
					Effect: v1.TaintEffectNoExecute,
				},
			}
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					},
				},
				Taints: []v1.Taint{
					{
						Key:    "example.com/startup-taint1",
						Value:  "true",
						Effect: v1.TaintEffectNoExecute,
					},
				},
				ReadyStatus: v1.ConditionTrue,
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Labels).ToNot(HaveKey(v1alpha5.LabelNodeInitialized))
		})
	})
	Context("Emptiness", func() {
		It("should not TTL nodes that have ready status unknown", func() {
			provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
			node := test.Node(test.NodeOptions{
				ObjectMeta:  metav1.ObjectMeta{Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name}},
				ReadyStatus: v1.ConditionUnknown,
			})

			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).ToNot(HaveKey(v1alpha5.EmptinessTimestampAnnotationKey))
		})
		It("should not TTL nodes that have ready status false", func() {
			provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
			node := test.Node(test.NodeOptions{
				ObjectMeta:  metav1.ObjectMeta{Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name}},
				ReadyStatus: v1.ConditionFalse,
			})

			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).ToNot(HaveKey(v1alpha5.EmptinessTimestampAnnotationKey))
		})
		It("should label nodes as underutilized and add TTL", func() {
			provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
			node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
			}})
			ExpectApplied(ctx, env.Client, provisioner, node)

			// mark it empty first to get past the debounce check
			fakeClock.Step(30 * time.Second)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			// make the node more than 5 minutes old
			fakeClock.Step(320 * time.Second)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).To(HaveKey(v1alpha5.EmptinessTimestampAnnotationKey))
		})
		It("should return a requeue polling interval when the node is underutilized and nominated", func() {
			provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
			node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1alpha5.LabelNodeInitialized:    "true",
					v1.LabelInstanceTypeStable:       "default-instance-type", // need the instance type for the cluster state update
				},
			}})
			ExpectApplied(ctx, env.Client, provisioner, node)

			// Add the node to the cluster state and nominate it in the internal cluster state
			Expect(cluster.UpdateNode(ctx, node)).To(Succeed())
			cluster.NominateNodeForPod(ctx, node.Name)

			result := ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			Expect(result.RequeueAfter).To(Equal(time.Second * 30))
			Expect(node.Labels).ToNot(HaveKey(v1alpha5.EmptinessTimestampAnnotationKey))
		})
		It("should remove labels from non-empty nodes", func() {
			provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
			node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
				Annotations: map[string]string{
					v1alpha5.EmptinessTimestampAnnotationKey: fakeClock.Now().Add(100 * time.Second).Format(time.RFC3339),
				}},
			})
			ExpectApplied(ctx, env.Client, provisioner, node, test.Pod(test.PodOptions{
				NodeName:   node.Name,
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			}))
			// make the node more than 5 minutes old
			fakeClock.Step(320 * time.Second)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).ToNot(HaveKey(v1alpha5.EmptinessTimestampAnnotationKey))
		})
	})
	Context("Expiration", func() {
		It("should remove the annotation from nodes when expiration is disabled", func() {
			node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
				Annotations: map[string]string{
					v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionExpiredAnnotationValue,
				},
			}})
			ExpectApplied(ctx, env.Client, provisioner, node)

			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).ToNot(HaveKey(v1alpha5.VoluntaryDisruptionAnnotationKey))
		})
		It("should annotate nodes as expired", func() {
			provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(30)
			node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
			}})
			ExpectApplied(ctx, env.Client, provisioner, node)

			// step forward to make the node expired
			fakeClock.Step(60 * time.Second)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).To(HaveKeyWithValue(v1alpha5.VoluntaryDisruptionAnnotationKey, v1alpha5.VoluntaryDisruptionExpiredAnnotationValue))
		})
		It("should remove the annotation from non-expired nodes", func() {
			provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(200)
			node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
				Annotations: map[string]string{
					v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionExpiredAnnotationValue,
				}},
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Annotations).ToNot(HaveKey(v1alpha5.VoluntaryDisruptionAnnotationKey))
		})
	})
	Context("Finalizer", func() {
		It("should add the termination finalizer if missing", func() {
			n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels:     map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
				Finalizers: []string{"fake.com/finalizer"},
			}})
			ExpectApplied(ctx, env.Client, provisioner, n)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(n))

			n = ExpectNodeExists(ctx, env.Client, n.Name)
			Expect(n.Finalizers).To(ConsistOf(n.Finalizers[0], v1alpha5.TerminationFinalizer))
		})
		It("should do nothing if terminating", func() {
			n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels:     map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
				Finalizers: []string{"fake.com/finalizer"},
			}})
			ExpectApplied(ctx, env.Client, provisioner, n)
			Expect(env.Client.Delete(ctx, n)).To(Succeed())
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(n))

			n = ExpectNodeExists(ctx, env.Client, n.Name)
			Expect(n.Finalizers).To(Equal(n.Finalizers))
		})
		It("should do nothing if the termination finalizer already exists", func() {
			n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels:     map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
				Finalizers: []string{v1alpha5.TerminationFinalizer, "fake.com/finalizer"},
			}})
			ExpectApplied(ctx, env.Client, provisioner, n)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(n))

			n = ExpectNodeExists(ctx, env.Client, n.Name)
			Expect(n.Finalizers).To(Equal(n.Finalizers))
		})
		It("should add an owner reference to the node", func() {
			n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
			}})
			ExpectApplied(ctx, env.Client, provisioner, n)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(n))
			n = ExpectNodeExists(ctx, env.Client, n.Name)
			Expect(n.OwnerReferences).To(Equal([]metav1.OwnerReference{{
				APIVersion:         v1alpha5.SchemeGroupVersion.String(),
				Kind:               "Provisioner",
				Name:               provisioner.Name,
				UID:                provisioner.UID,
				BlockOwnerDeletion: ptr.Bool(true),
			}}))
		})
	})
	Context("Filters", func() {
		BeforeEach(func() {
			innerCtx, cancel := context.WithCancel(ctx)
			DeferCleanup(func() {
				cancel()
			})
			mgr, err := controllerruntime.NewManager(env.Config, controllerruntime.Options{
				Scheme:             env.Scheme,
				MetricsBindAddress: "0",
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(nodeController.Builder(innerCtx, mgr).Complete(nodeController)).To(Succeed())
			go func() {
				defer GinkgoRecover()
				Expect(mgr.Start(innerCtx)).To(Succeed())
			}()
		})
		It("should do nothing if the not owned by a provisioner", func() {
			n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{"fake.com/finalizer"},
			}})
			ExpectApplied(ctx, env.Client, provisioner, n)

			// Node shouldn't reconcile anything onto it
			Consistently(func(g Gomega) {
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: n.Name}, &v1.Node{})).To(Succeed())
				g.Expect(n.Finalizers).To(Equal(n.Finalizers))
			})
		})
		It("should do nothing if deletion timestamp is set", func() {
			n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{"fake.com/finalizer"},
			}})
			ExpectApplied(ctx, env.Client, provisioner, n)
			Expect(env.Client.Delete(ctx, n)).To(Succeed())

			// Update the node to be provisioned by the provisioner through labels
			n.Labels = map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
			}
			ExpectApplied(ctx, env.Client, n)

			// Node shouldn't reconcile anything onto it
			Consistently(func(g Gomega) {
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: n.Name}, &v1.Node{})).To(Succeed())
				g.Expect(n.Finalizers).To(Equal(n.Finalizers))
			})
		})
	})
})
