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
	"math/rand"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
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
	cloudProvider.Reset()
	cluster.Reset()
})

var _ = Describe("Combined/Provisioning", func() {
	var provisioner *v1alpha5.Provisioner
	var nodePool *v1beta1.NodePool
	BeforeEach(func() {
		provisioner = test.Provisioner()
		nodePool = test.NodePool()
	})
	It("should schedule pods using owner label selectors", func() {
		ExpectApplied(ctx, env.Client, nodePool, provisioner)

		provisionerPod := test.UnschedulablePod(test.PodOptions{
			NodeSelector: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
			},
		})
		nodePoolPod := test.UnschedulablePod(test.PodOptions{
			NodeSelector: map[string]string{
				v1beta1.NodePoolLabelKey: nodePool.Name,
			},
		})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, provisionerPod, nodePoolPod)
		node := ExpectScheduled(ctx, env.Client, provisionerPod)
		Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, provisioner.Name))
		node = ExpectScheduled(ctx, env.Client, nodePoolPod)
		Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, nodePool.Name))
	})
	It("should schedule pods using custom labels", func() {
		provisioner.Spec.Labels = map[string]string{
			"provisioner": "true",
		}
		nodePool.Spec.Template.Labels = map[string]string{
			"nodepool": "true",
		}
		ExpectApplied(ctx, env.Client, nodePool, provisioner)

		provisionerPod := test.UnschedulablePod(test.PodOptions{
			NodeSelector: map[string]string{
				"provisioner": "true",
			},
		})
		nodePoolPod := test.UnschedulablePod(test.PodOptions{
			NodeSelector: map[string]string{
				"nodepool": "true",
			},
		})

		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, provisionerPod, nodePoolPod)
		node := ExpectScheduled(ctx, env.Client, provisionerPod)
		Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, provisioner.Name))
		node = ExpectScheduled(ctx, env.Client, nodePoolPod)
		Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, nodePool.Name))
	})
	It("should schedule pods using custom requirements", func() {
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      "provisioner",
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"true"},
			},
		}
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      "nodepool",
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"true"},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool, provisioner)

		provisionerPod := test.UnschedulablePod(test.PodOptions{
			NodeSelector: map[string]string{
				"provisioner": "true",
			},
		})
		nodePoolPod := test.UnschedulablePod(test.PodOptions{
			NodeSelector: map[string]string{
				"nodepool": "true",
			},
		})

		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, provisionerPod, nodePoolPod)
		node := ExpectScheduled(ctx, env.Client, provisionerPod)
		Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, provisioner.Name))
		node = ExpectScheduled(ctx, env.Client, nodePoolPod)
		Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, nodePool.Name))
	})
	It("should schedule pods using taints", func() {
		provisioner.Spec.Taints = append(provisioner.Spec.Taints, v1.Taint{
			Key:    "only-provisioner",
			Value:  "true",
			Effect: v1.TaintEffectNoSchedule,
		})
		nodePool.Spec.Template.Spec.Taints = append(nodePool.Spec.Template.Spec.Taints, v1.Taint{
			Key:    "only-nodepool",
			Value:  "true",
			Effect: v1.TaintEffectNoSchedule,
		})
		ExpectApplied(ctx, env.Client, provisioner, nodePool)

		provisionerPod := test.UnschedulablePod(test.PodOptions{
			Tolerations: []v1.Toleration{
				{
					Key:      "only-provisioner",
					Operator: v1.TolerationOpExists,
				},
			},
		})
		nodePoolPod := test.UnschedulablePod(test.PodOptions{
			Tolerations: []v1.Toleration{
				{
					Key:      "only-nodepool",
					Operator: v1.TolerationOpExists,
				},
			},
		})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, provisionerPod, nodePoolPod)
		node := ExpectScheduled(ctx, env.Client, provisionerPod)
		Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, provisioner.Name))
		node = ExpectScheduled(ctx, env.Client, nodePoolPod)
		Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, nodePool.Name))
	})
	It("should order the NodePools and Provisioner by weight", func() {
		var provisioners []*v1alpha5.Provisioner
		var nodePools []*v1beta1.NodePool
		weights := lo.Reject(rand.Perm(101), func(i int, _ int) bool { return i == 0 })
		for i := 0; i < 10; i++ {
			p := test.Provisioner(test.ProvisionerOptions{
				Weight: lo.ToPtr[int32](int32(weights[i])),
			})
			provisioners = append(provisioners, p)
			ExpectApplied(ctx, env.Client, p)
		}
		for i := 0; i < 10; i++ {
			np := test.NodePool(v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Weight: lo.ToPtr[int32](int32(weights[i+10])),
				},
			})
			nodePools = append(nodePools, np)
			ExpectApplied(ctx, env.Client, np)
		}
		highestWeightProvisioner := lo.MaxBy(provisioners, func(a, b *v1alpha5.Provisioner) bool {
			return lo.FromPtr(a.Spec.Weight) > lo.FromPtr(b.Spec.Weight)
		})
		highestWeightNodePool := lo.MaxBy(nodePools, func(a, b *v1beta1.NodePool) bool {
			return lo.FromPtr(a.Spec.Weight) > lo.FromPtr(b.Spec.Weight)
		})

		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)

		if lo.FromPtr(highestWeightProvisioner.Spec.Weight) > lo.FromPtr(highestWeightNodePool.Spec.Weight) {
			Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, highestWeightProvisioner.Name))
		} else {
			Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, highestWeightNodePool.Name))
		}
	})
	Context("Limits", func() {
		It("should select a NodePool if a Provisioner is over its limit", func() {
			provisioner.Spec.Limits = &v1alpha5.Limits{Resources: v1.ResourceList{v1.ResourceCPU: resource.MustParse("0")}}
			ExpectApplied(ctx, env.Client, provisioner, nodePool)

			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, nodePool.Name))
		})
		It("should select a Provisioner if a NodePool is over its limit", func() {
			nodePool.Spec.Limits = v1beta1.Limits(v1.ResourceList{v1.ResourceCPU: resource.MustParse("0")})
			ExpectApplied(ctx, env.Client, provisioner, nodePool)

			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, provisioner.Name))
		})
	})
	Context("Deleting", func() {
		It("should select a NodePool if a Provisioner is deleting", func() {
			ExpectApplied(ctx, env.Client, nodePool, provisioner)
			ExpectDeletionTimestampSet(ctx, env.Client, provisioner)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, nodePool.Name))
		})
		It("should select a Provisioner if a NodePool is deleting", func() {
			ExpectApplied(ctx, env.Client, nodePool, provisioner)
			ExpectDeletionTimestampSet(ctx, env.Client, nodePool)
			pod := test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, provisioner.Name))
		})
	})
	Context("Daemonsets", func() {
		It("should select a NodePool if Daemonsets that would schedule to Provisioner would exceed capacity", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "provisioner-instance-type",
					Resources: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					},
				}),
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "nodepool-instance-type",
					Resources: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:    resource.MustParse("4"),
						v1.ResourceMemory: resource.MustParse("4Gi"),
					},
				}),
			}
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelInstanceType,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"provisioner-instance-type"},
				},
			}
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelInstanceType,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"nodepool-instance-type"},
				},
			}
			daemonSet := test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")}},
				}},
			)
			ExpectApplied(ctx, env.Client, nodePool, provisioner, daemonSet)

			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, nodePool.Name))
		})
		It("should select a Provisioner if Daemonsets that would schedule to NodePool would exceed capacity", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "provisioner-instance-type",
					Resources: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:    resource.MustParse("4"),
						v1.ResourceMemory: resource.MustParse("4Gi"),
					},
				}),
				fake.NewInstanceType(fake.InstanceTypeOptions{
					Name: "nodepool-instance-type",
					Resources: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					},
				}),
			}
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelInstanceType,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"provisioner-instance-type"},
				},
			}
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelInstanceType,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"nodepool-instance-type"},
				},
			}
			daemonSet := test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")}},
				}},
			)
			ExpectApplied(ctx, env.Client, nodePool, provisioner, daemonSet)

			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, provisioner.Name))
		})
		It("should select a NodePool if Daemonset select against a Provisioner and would cause the Provisioner to exceed capacity", func() {
			provisioner.Spec.Labels = lo.Assign(provisioner.Spec.Labels, map[string]string{"scheduleme": "true"})
			nodePool.Spec.Template.Labels = lo.Assign(nodePool.Spec.Template.Labels, map[string]string{"scheduleme": "false"})

			daemonSet := test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{
							Key:      "scheduleme",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"true"},
						},
					},
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("10000"), v1.ResourceMemory: resource.MustParse("10000Gi")}},
				}},
			)
			ExpectApplied(ctx, env.Client, nodePool, provisioner, daemonSet)

			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, nodePool.Name))
		})
		It("should select a Provisioner if Daemonset select against a NodePool and would cause the NodePool to exceed capacity", func() {
			provisioner.Spec.Labels = lo.Assign(provisioner.Spec.Labels, map[string]string{"scheduleme": "false"})
			nodePool.Spec.Template.Labels = lo.Assign(nodePool.Spec.Template.Labels, map[string]string{"scheduleme": "true"})

			daemonSet := test.DaemonSet(
				test.DaemonSetOptions{PodOptions: test.PodOptions{
					NodeRequirements: []v1.NodeSelectorRequirement{
						{
							Key:      "scheduleme",
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"true"},
						},
					},
					ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("10000"), v1.ResourceMemory: resource.MustParse("10000Gi")}},
				}},
			)
			ExpectApplied(ctx, env.Client, nodePool, provisioner, daemonSet)

			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, provisioner.Name))
		})
	})
	Context("Preferential Fallback", func() {
		It("should fallback from a NodePool to a Provisioner when preferences can't be satisfied against the NodePool", func() {
			provisioner.Spec.Labels = lo.Assign(provisioner.Spec.Labels, map[string]string{
				"foo": "true",
			})
			ExpectApplied(ctx, env.Client, nodePool, provisioner)

			pod := test.UnschedulablePod()
			pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{
				{
					Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: "foo", Operator: v1.NodeSelectorOpIn, Values: []string{"true"}},
					}},
				},
				{
					Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: "bar", Operator: v1.NodeSelectorOpIn, Values: []string{"true"}},
					}},
				},
			}}}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, provisioner.Name))
		})
		It("should fallback from a Provisioner to a NodePool when preferences can't be satisfied against the Provisioner", func() {
			nodePool.Spec.Template.Labels = lo.Assign(nodePool.Spec.Template.Labels, map[string]string{
				"foo": "true",
			})
			ExpectApplied(ctx, env.Client, nodePool, provisioner)

			pod := test.UnschedulablePod()
			pod.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{
				{
					Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: "foo", Operator: v1.NodeSelectorOpIn, Values: []string{"true"}},
					}},
				},
				{
					Weight: 1, Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{
						{Key: "bar", Operator: v1.NodeSelectorOpIn, Values: []string{"true"}},
					}},
				},
			}}}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, nodePool.Name))
		})
	})
	Context("Topology", func() {
		var labels map[string]string
		BeforeEach(func() {
			labels = map[string]string{"test": "test"}
		})
		It("should spread across Provisioners and NodePools when considering zonal topology", func() {
			topology := []v1.TopologySpreadConstraint{{
				TopologyKey:       v1.LabelTopologyZone,
				WhenUnsatisfiable: v1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
				MaxSkew:           1,
			}}

			testZone1Provisioner := test.Provisioner(test.ProvisionerOptions{
				Labels: map[string]string{
					v1.LabelTopologyZone: "test-zone-1",
				},
			})
			testZone2NodePool := test.NodePool(v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Template: v1beta1.NodeClaimTemplate{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								v1.LabelTopologyZone: "test-zone-2",
							},
						},
					},
				},
			})
			testZone3Provisioner := test.Provisioner(test.ProvisionerOptions{
				Labels: map[string]string{
					v1.LabelTopologyZone: "test-zone-3",
				},
			})
			ExpectApplied(ctx, env.Client, testZone1Provisioner, testZone2NodePool, testZone3Provisioner)

			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov,
				test.UnschedulablePods(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: labels}, TopologySpreadConstraints: topology}, 4)...,
			)
			ExpectSkew(ctx, env.Client, "default", &topology[0]).To(ConsistOf(1, 1, 2))

			// Expect that among the nodes that have skew that we have deployed nodes across Provisioners and NodePools
			nodes := ExpectNodes(ctx, env.Client)
			_, ok := lo.Find(nodes, func(n *v1.Node) bool { return n.Labels[v1alpha5.ProvisionerNameLabelKey] == testZone1Provisioner.Name })
			Expect(ok).To(BeTrue())
			_, ok = lo.Find(nodes, func(n *v1.Node) bool { return n.Labels[v1beta1.NodePoolLabelKey] == testZone2NodePool.Name })
			Expect(ok).To(BeTrue())
			_, ok = lo.Find(nodes, func(n *v1.Node) bool { return n.Labels[v1alpha5.ProvisionerNameLabelKey] == testZone3Provisioner.Name })
			Expect(ok).To(BeTrue())
		})
		It("should spread across Provisioners and NodePools and respect zonal constraints (subset) with requirements", func() {
			topology := []v1.TopologySpreadConstraint{{
				TopologyKey:       v1.LabelTopologyZone,
				WhenUnsatisfiable: v1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
				MaxSkew:           1,
			}}

			testZone1Provisioner := test.Provisioner(test.ProvisionerOptions{
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      v1.LabelTopologyZone,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"test-zone-1"},
					},
				},
			})
			testZone2NodePool := test.NodePool(v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Template: v1beta1.NodeClaimTemplate{
						Spec: v1beta1.NodeClaimSpec{
							Requirements: []v1.NodeSelectorRequirement{
								{
									Key:      v1.LabelTopologyZone,
									Operator: v1.NodeSelectorOpIn,
									Values:   []string{"test-zone-2"},
								},
							},
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, testZone1Provisioner, testZone2NodePool)

			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov,
				test.UnschedulablePods(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: labels}, TopologySpreadConstraints: topology}, 4)...,
			)
			ExpectSkew(ctx, env.Client, "default", &topology[0]).To(ConsistOf(2, 2))

			// Expect that among the nodes that have skew that we have deployed nodes across Provisioners and NodePools
			nodes := ExpectNodes(ctx, env.Client)
			_, ok := lo.Find(nodes, func(n *v1.Node) bool { return n.Labels[v1alpha5.ProvisionerNameLabelKey] == testZone1Provisioner.Name })
			Expect(ok).To(BeTrue())
			_, ok = lo.Find(nodes, func(n *v1.Node) bool { return n.Labels[v1beta1.NodePoolLabelKey] == testZone2NodePool.Name })
			Expect(ok).To(BeTrue())
		})
		It("should spread across Provisioners and NodePools and respect zonal constraints (subset) with labels", func() {
			topology := []v1.TopologySpreadConstraint{{
				TopologyKey:       v1.LabelTopologyZone,
				WhenUnsatisfiable: v1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
				MaxSkew:           1,
			}}

			testZone1Provisioner := test.Provisioner(test.ProvisionerOptions{
				Labels: map[string]string{
					v1.LabelTopologyZone: "test-zone-1",
				},
			})
			testZone2NodePool := test.NodePool(v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Template: v1beta1.NodeClaimTemplate{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								v1.LabelTopologyZone: "test-zone-2",
							},
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, testZone1Provisioner, testZone2NodePool)

			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov,
				test.UnschedulablePods(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: labels}, TopologySpreadConstraints: topology}, 4)...,
			)
			ExpectSkew(ctx, env.Client, "default", &topology[0]).To(ConsistOf(2, 2))

			// Expect that among the nodes that have skew that we have deployed nodes across Provisioners and NodePools
			nodes := ExpectNodes(ctx, env.Client)
			_, ok := lo.Find(nodes, func(n *v1.Node) bool { return n.Labels[v1alpha5.ProvisionerNameLabelKey] == testZone1Provisioner.Name })
			Expect(ok).To(BeTrue())
			_, ok = lo.Find(nodes, func(n *v1.Node) bool { return n.Labels[v1beta1.NodePoolLabelKey] == testZone2NodePool.Name })
			Expect(ok).To(BeTrue())
		})
		It("should spread across Provisioners and NodePools when considering capacity type", func() {
			topology := []v1.TopologySpreadConstraint{{
				TopologyKey:       v1beta1.CapacityTypeLabelKey,
				WhenUnsatisfiable: v1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
				MaxSkew:           1,
			}}
			onDemandProvisioner := test.Provisioner(test.ProvisionerOptions{
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      v1alpha5.LabelCapacityType,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{v1alpha5.CapacityTypeOnDemand},
					},
				},
			})
			spotNodePool := test.NodePool(v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Template: v1beta1.NodeClaimTemplate{
						Spec: v1beta1.NodeClaimSpec{
							Requirements: []v1.NodeSelectorRequirement{
								{
									Key:      v1beta1.CapacityTypeLabelKey,
									Operator: v1.NodeSelectorOpIn,
									Values:   []string{v1beta1.CapacityTypeSpot},
								},
							},
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, onDemandProvisioner, spotNodePool)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov,
				test.UnschedulablePods(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: labels}, TopologySpreadConstraints: topology}, 4)...,
			)
			ExpectSkew(ctx, env.Client, "default", &topology[0]).To(ConsistOf(2, 2))

			// Expect that among the nodes that have skew that we have deployed nodes across Provisioners and NodePools
			nodes := ExpectNodes(ctx, env.Client)
			_, ok := lo.Find(nodes, func(n *v1.Node) bool { return n.Labels[v1alpha5.ProvisionerNameLabelKey] == onDemandProvisioner.Name })
			Expect(ok).To(BeTrue())
			_, ok = lo.Find(nodes, func(n *v1.Node) bool { return n.Labels[v1beta1.NodePoolLabelKey] == spotNodePool.Name })
			Expect(ok).To(BeTrue())
		})
	})
})

func ExpectNodeClaimRequirements(nodeClaim *v1beta1.NodeClaim, requirements ...v1.NodeSelectorRequirement) {
	GinkgoHelper()
	for _, requirement := range requirements {
		req, ok := lo.Find(nodeClaim.Spec.Requirements, func(r v1.NodeSelectorRequirement) bool {
			return r.Key == requirement.Key && r.Operator == requirement.Operator
		})
		Expect(ok).To(BeTrue())

		have := sets.New(req.Values...)
		expected := sets.New(requirement.Values...)
		Expect(have.Len()).To(Equal(expected.Len()))
		Expect(have.Intersection(expected).Len()).To(Equal(expected.Len()))
	}
}

func ExpectNodeClaimRequests(nodeClaim *v1beta1.NodeClaim, resources v1.ResourceList) {
	GinkgoHelper()
	for name, value := range resources {
		v := nodeClaim.Spec.Resources.Requests[name]
		Expect(v.AsApproximateFloat64()).To(BeNumerically("~", value.AsApproximateFloat64(), 10))
	}
}
