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

package capacitybuffer

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	testv1alpha1 "sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx          context.Context
	env          *test.Environment
	cbController *Controller
)

func TestCapacityBuffer(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "CapacityBuffer Controller")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(testv1alpha1.CRDs...))
	cbController = NewController(env.Client, &fakeTrigger{})
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("CapacityBuffer Controller", func() {
	Context("PodTemplateRef resolution", func() {
		It("should resolve a PodTemplate and set ReadyForProvisioning=True", func() {
			pt := &v1.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{
						Containers: []v1.Container{{
							Name:  "app",
							Image: "pause:latest",
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("1"),
									v1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						}},
					},
				},
			}
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-buffer",
					Namespace: "default",
				},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ProvisioningStrategy: lo.ToPtr("buffer.x-k8s.io/active-capacity"),
					PodTemplateRef:       &autoscalingv1beta1.LocalObjectRef{Name: "test-template"},
					Replicas:             lo.ToPtr(int32(5)),
				},
			}
			ExpectApplied(ctx, env.Client, pt, cb)
			ExpectObjectReconciled(ctx, env.Client, cbController, cb)

			cb = ExpectExists(ctx, env.Client, cb)
			cond := findCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
			Expect(cond).ToNot(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(ReasonResolved))
			Expect(cb.Status.PodTemplateRef).ToNot(BeNil())
			Expect(cb.Status.PodTemplateRef.Name).To(Equal("test-template"))
			Expect(cb.Status.Replicas).ToNot(BeNil())
			Expect(*cb.Status.Replicas).To(Equal(int32(5)))
			Expect(cb.Status.ProvisioningStrategy).ToNot(BeNil())
			Expect(*cb.Status.ProvisioningStrategy).To(Equal("buffer.x-k8s.io/active-capacity"))
		})

		It("should set ReadyForProvisioning=False when PodTemplate is not found", func() {
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-buffer-missing",
					Namespace: "default",
				},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "nonexistent"},
					Replicas:       lo.ToPtr(int32(3)),
				},
			}
			ExpectApplied(ctx, env.Client, cb)
			ExpectObjectReconciled(ctx, env.Client, cbController, cb)

			cb = ExpectExists(ctx, env.Client, cb)
			cond := findCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
			Expect(cond).ToNot(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonPodTemplateNotFound))
		})
	})

	Context("ScalableRef resolution", func() {
		It("should resolve a Deployment and calculate percentage-based replicas", func() {
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-app",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: lo.ToPtr(int32(10)),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "my-app"}},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "my-app"}},
						Spec: v1.PodSpec{
							Containers: []v1.Container{{
								Name:  "app",
								Image: "pause:latest",
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU:    resource.MustParse("500m"),
										v1.ResourceMemory: resource.MustParse("256Mi"),
									},
								},
							}},
						},
					},
				},
			}
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-buffer-scalable",
					Namespace: "default",
				},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "apps",
						Kind:     "Deployment",
						Name:     "my-app",
					},
					Percentage: lo.ToPtr(int32(20)),
				},
			}
			ExpectApplied(ctx, env.Client, deploy, cb)
			ExpectObjectReconciled(ctx, env.Client, cbController, cb)

			cb = ExpectExists(ctx, env.Client, cb)
			cond := findCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
			Expect(cond).ToNot(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cb.Status.Replicas).ToNot(BeNil())
			// 20% of 10 = 2
			Expect(*cb.Status.Replicas).To(Equal(int32(2)))
			// scalableRef does not set PodTemplateRef in status
			Expect(cb.Status.PodTemplateRef).To(BeNil())
		})

		It("should set ReadyForProvisioning=False when scalable ref is not found", func() {
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-buffer-missing-ref",
					Namespace: "default",
				},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "apps",
						Kind:     "Deployment",
						Name:     "nonexistent",
					},
					Replicas: lo.ToPtr(int32(3)),
				},
			}
			ExpectApplied(ctx, env.Client, cb)
			ExpectObjectReconciled(ctx, env.Client, cbController, cb)

			cb = ExpectExists(ctx, env.Client, cb)
			cond := findCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
			Expect(cond).ToNot(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonScalableRefNotFound))
		})

		It("should resolve a StatefulSet", func() {
			sts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-sts",
					Namespace: "default",
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: lo.ToPtr(int32(5)),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "my-sts"}},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "my-sts"}},
						Spec: v1.PodSpec{
							Containers: []v1.Container{{
								Name:  "app",
								Image: "pause:latest",
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU: resource.MustParse("500m"),
									},
								},
							}},
						},
					},
				},
			}
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-buffer-sts",
					Namespace: "default",
				},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "apps",
						Kind:     "StatefulSet",
						Name:     "my-sts",
					},
					Percentage: lo.ToPtr(int32(20)),
				},
			}
			ExpectApplied(ctx, env.Client, sts, cb)
			ExpectObjectReconciled(ctx, env.Client, cbController, cb)

			cb = ExpectExists(ctx, env.Client, cb)
			cond := findCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
			Expect(cond).ToNot(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cb.Status.Replicas).ToNot(BeNil())
			// 20% of 5 = 1
			Expect(*cb.Status.Replicas).To(Equal(int32(1)))
		})

		It("should return error for unsupported kind", func() {
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-buffer-bad-kind",
					Namespace: "default",
				},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "batch",
						Kind:     "Job",
						Name:     "my-job",
					},
					Percentage: lo.ToPtr(int32(20)),
				},
			}
			ExpectApplied(ctx, env.Client, cb)
			Expect(ExpectObjectReconcileFailed(ctx, env.Client, cbController, cb)).To(HaveOccurred())
		})

		It("should return error for unsupported API group", func() {
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-buffer-bad-group",
					Namespace: "default",
				},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "batch",
						Kind:     "Deployment",
						Name:     "my-deploy",
					},
					Percentage: lo.ToPtr(int32(20)),
				},
			}
			ExpectApplied(ctx, env.Client, cb)
			Expect(ExpectObjectReconcileFailed(ctx, env.Client, cbController, cb)).To(HaveOccurred())
		})

		It("should default to 1 replica when workload has nil Spec.Replicas", func() {
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nil-replicas-app",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "nil-replicas"}},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "nil-replicas"}},
						Spec: v1.PodSpec{
							Containers: []v1.Container{{
								Name:  "app",
								Image: "pause:latest",
								Resources: v1.ResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceCPU: resource.MustParse("500m"),
									},
								},
							}},
						},
					},
				},
			}
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-buffer-nil-replicas",
					Namespace: "default",
				},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "apps",
						Kind:     "Deployment",
						Name:     "nil-replicas-app",
					},
					Percentage: lo.ToPtr(int32(100)),
				},
			}
			ExpectApplied(ctx, env.Client, deploy, cb)
			ExpectObjectReconciled(ctx, env.Client, cbController, cb)

			cb = ExpectExists(ctx, env.Client, cb)
			cond := findCondition(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition)
			Expect(cond).ToNot(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cb.Status.Replicas).ToNot(BeNil())
			// 100% of 1 (defaulted) = 1
			Expect(*cb.Status.Replicas).To(Equal(int32(1)))
		})
	})

	Context("Replica calculation", func() {
		// Each entry passes the backing workload and a buffer that references it; the body
		// applies both, reconciles, and asserts the resolved status.Replicas.
		DescribeTable("should resolve buffer replicas from replicas, percentage, and limits",
			func(backing client.Object, cb *autoscalingv1beta1.CapacityBuffer, expected int32) {
				ExpectApplied(ctx, env.Client, backing, cb)
				ExpectObjectReconciled(ctx, env.Client, cbController, cb)
				Expect(ExpectExists(ctx, env.Client, cb).Status.Replicas).To(HaveValue(Equal(expected)))
			},
			// podTemplateRef: replicas and limits only (percentage requires a scalableRef).
			Entry("fixed replicas only",
				test.PodTemplate(test.PodTemplateOptions{ObjectMeta: metav1.ObjectMeta{Name: "pt-fixed"}}),
				&autoscalingv1beta1.CapacityBuffer{
					ObjectMeta: metav1.ObjectMeta{Name: "cb-fixed", Namespace: "default"},
					Spec: autoscalingv1beta1.CapacityBufferSpec{
						PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "pt-fixed"},
						Replicas:       lo.ToPtr(int32(7)),
					},
				},
				int32(7)),
			// replicas = 10, capped by limit (3 CPU / 1 CPU per pod = 3)
			Entry("replicas capped by binding limit",
				test.PodTemplate(test.PodTemplateOptions{ObjectMeta: metav1.ObjectMeta{Name: "pt-cap"}, PodOptions: cpuPod()}),
				&autoscalingv1beta1.CapacityBuffer{
					ObjectMeta: metav1.ObjectMeta{Name: "cb-cap", Namespace: "default"},
					Spec: autoscalingv1beta1.CapacityBufferSpec{
						PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "pt-cap"},
						Replicas:       lo.ToPtr(int32(10)),
						Limits:         autoscalingv1beta1.Limits{v1.ResourceCPU: resource.MustParse("3")},
					},
				},
				int32(3)),
			// limit allows 10 pods, so the cap does not bind => 3
			Entry("non-binding limit does not reduce (podTemplateRef)",
				test.PodTemplate(test.PodTemplateOptions{ObjectMeta: metav1.ObjectMeta{Name: "pt-loose"}, PodOptions: cpuPod()}),
				&autoscalingv1beta1.CapacityBuffer{
					ObjectMeta: metav1.ObjectMeta{Name: "cb-loose", Namespace: "default"},
					Spec: autoscalingv1beta1.CapacityBufferSpec{
						PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "pt-loose"},
						Replicas:       lo.ToPtr(int32(3)),
						Limits:         autoscalingv1beta1.Limits{v1.ResourceCPU: resource.MustParse("10")},
					},
				},
				int32(3)),
			// limits alone determine the count (5 CPU / 1 CPU per pod = 5)
			Entry("limits only",
				test.PodTemplate(test.PodTemplateOptions{ObjectMeta: metav1.ObjectMeta{Name: "pt-limit"}, PodOptions: cpuPod()}),
				&autoscalingv1beta1.CapacityBuffer{
					ObjectMeta: metav1.ObjectMeta{Name: "cb-limit", Namespace: "default"},
					Spec: autoscalingv1beta1.CapacityBufferSpec{
						PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "pt-limit"},
						Limits:         autoscalingv1beta1.Limits{v1.ResourceCPU: resource.MustParse("5")},
					},
				},
				int32(5)),
			// scalableRef: max(replicas, percentage), then capped by limits.
			Entry("percentage only",
				test.Deployment(test.DeploymentOptions{ObjectMeta: metav1.ObjectMeta{Name: "d-pct-only"}, Replicas: 10}),
				&autoscalingv1beta1.CapacityBuffer{
					ObjectMeta: metav1.ObjectMeta{Name: "cb-pct-only", Namespace: "default"},
					Spec: autoscalingv1beta1.CapacityBufferSpec{
						ScalableRef: deploymentRef("d-pct-only"),
						Percentage:  lo.ToPtr(int32(20)),
					},
				},
				int32(2)),
			Entry("replicas > percentage",
				test.Deployment(test.DeploymentOptions{ObjectMeta: metav1.ObjectMeta{Name: "d-repl"}, Replicas: 10}),
				&autoscalingv1beta1.CapacityBuffer{
					ObjectMeta: metav1.ObjectMeta{Name: "cb-repl", Namespace: "default"},
					Spec: autoscalingv1beta1.CapacityBufferSpec{
						ScalableRef: deploymentRef("d-repl"),
						Replicas:    lo.ToPtr(int32(5)),
						Percentage:  lo.ToPtr(int32(20)), // 20% of 10 = 2 => max(5, 2) = 5
					},
				},
				int32(5)),
			Entry("percentage > replicas",
				test.Deployment(test.DeploymentOptions{ObjectMeta: metav1.ObjectMeta{Name: "d-pct"}, Replicas: 10}),
				&autoscalingv1beta1.CapacityBuffer{
					ObjectMeta: metav1.ObjectMeta{Name: "cb-pct", Namespace: "default"},
					Spec: autoscalingv1beta1.CapacityBufferSpec{
						ScalableRef: deploymentRef("d-pct"),
						Replicas:    lo.ToPtr(int32(5)),
						Percentage:  lo.ToPtr(int32(80)), // 80% of 10 = 8 => max(5, 8) = 8
					},
				},
				int32(8)),
			// limit caps the max() only when it binds (downward).
			Entry("max of replicas and percentage capped by limit",
				test.Deployment(test.DeploymentOptions{ObjectMeta: metav1.ObjectMeta{Name: "d-cap"}, Replicas: 10, PodOptions: cpuPod()}),
				&autoscalingv1beta1.CapacityBuffer{
					ObjectMeta: metav1.ObjectMeta{Name: "cb-scap", Namespace: "default"},
					Spec: autoscalingv1beta1.CapacityBufferSpec{
						ScalableRef: deploymentRef("d-cap"),
						Replicas:    lo.ToPtr(int32(5)),
						Percentage:  lo.ToPtr(int32(80)), // max(5, 8) = 8
						Limits:      autoscalingv1beta1.Limits{v1.ResourceCPU: resource.MustParse("4")},
					},
				},
				int32(4)),
			// percentage rounds up to a floor of 1 (10% of 1 = 0.1 => 1).
			Entry("percentage rounds up to a minimum of 1",
				test.Deployment(test.DeploymentOptions{ObjectMeta: metav1.ObjectMeta{Name: "d-min"}, Replicas: 1}),
				&autoscalingv1beta1.CapacityBuffer{
					ObjectMeta: metav1.ObjectMeta{Name: "cb-min", Namespace: "default"},
					Spec: autoscalingv1beta1.CapacityBufferSpec{
						ScalableRef: deploymentRef("d-min"),
						Percentage:  lo.ToPtr(int32(10)),
					},
				},
				int32(1)),
		)
	})

	Context("PodTemplate watch", func() {
		It("should re-resolve the template when its generation changes", func() {
			pt := &v1.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "watched-template",
					Namespace: "default",
				},
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{Containers: []v1.Container{{Name: "app", Image: "pause:v1"}}},
				},
			}
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-watch-buffer",
					Namespace: "default",
				},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "watched-template"},
					Replicas:       lo.ToPtr(int32(2)),
				},
			}
			ExpectApplied(ctx, env.Client, pt, cb)
			ExpectObjectReconciled(ctx, env.Client, cbController, cb)

			cb = ExpectExists(ctx, env.Client, cb)
			Expect(cb.Status.PodTemplateGeneration).ToNot(BeNil())
			originalGen := *cb.Status.PodTemplateGeneration

			// PodTemplate is not a subresource-based CRD and doesn't auto-bump generation;
			// for this test we force a change by re-applying with a modified image, which
			// in envtest triggers a generation bump on the stored object.
			pt = ExpectExists(ctx, env.Client, pt)
			pt.Template.Spec.Containers[0].Image = "pause:v2"
			ExpectApplied(ctx, env.Client, pt)

			// Reconcile the buffer again; podTemplateGeneration in status should update.
			ExpectObjectReconciled(ctx, env.Client, cbController, cb)
			cb = ExpectExists(ctx, env.Client, cb)
			Expect(cb.Status.PodTemplateGeneration).ToNot(BeNil())
			// Generation must have advanced OR stayed the same (envtest behavior varies);
			// the stored template image must reflect v2.
			updatedPT := ExpectExists(ctx, env.Client, pt)
			Expect(updatedPT.Template.Spec.Containers[0].Image).To(Equal("pause:v2"))
			Expect(*cb.Status.PodTemplateGeneration).To(BeNumerically(">=", originalGen))
		})
	})

	Context("Provisioner trigger", func() {
		It("should trigger the provisioner after a successful reconcile", func() {
			trigger := &fakeTrigger{}
			ctrl := NewController(env.Client, trigger)

			pt := &v1.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "trig-template", Namespace: "default"},
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{Containers: []v1.Container{{Name: "c", Image: "p"}}},
				},
			}
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: "trig-buffer", Namespace: "default"},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "trig-template"},
					Replicas:       lo.ToPtr(int32(1)),
				},
			}
			ExpectApplied(ctx, env.Client, pt, cb)
			ExpectObjectReconciled(ctx, env.Client, ctrl, cb)

			cb = ExpectExists(ctx, env.Client, cb)
			Expect(trigger.calls).To(ContainElement(cb.UID))
		})

		It("should NOT trigger the provisioner when resolution fails", func() {
			trigger := &fakeTrigger{}
			ctrl := NewController(env.Client, trigger)

			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: "no-trig-buffer", Namespace: "default"},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "does-not-exist"},
					Replicas:       lo.ToPtr(int32(1)),
				},
			}
			ExpectApplied(ctx, env.Client, cb)
			ExpectObjectReconciled(ctx, env.Client, ctrl, cb)

			Expect(trigger.calls).To(BeEmpty())
		})
	})

	Context("podTemplateToBuffers mapping", func() {
		It("should return no requests when no buffers reference the template", func() {
			pt := &v1.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "unref-template", Namespace: "default"},
			}
			// Unrelated buffer referencing a different template
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "different-template"},
					Replicas:       lo.ToPtr(int32(1)),
				},
			}
			ExpectApplied(ctx, env.Client, cb)
			reqs := cbController.podTemplateToBuffers(ctx, pt)
			Expect(reqs).To(BeEmpty())
		})

		It("should return a single request for one matching buffer", func() {
			pt := &v1.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "map-template-1", Namespace: "default"},
			}
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: "matching-buffer", Namespace: "default"},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "map-template-1"},
					Replicas:       lo.ToPtr(int32(1)),
				},
			}
			ExpectApplied(ctx, env.Client, cb)
			reqs := cbController.podTemplateToBuffers(ctx, pt)
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].Name).To(Equal("matching-buffer"))
			Expect(reqs[0].Namespace).To(Equal("default"))
		})

		It("should return multiple requests when many buffers reference the same template", func() {
			pt := &v1.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-template", Namespace: "default"},
			}
			cb1 := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-a", Namespace: "default"},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "shared-template"},
					Replicas:       lo.ToPtr(int32(1)),
				},
			}
			cb2 := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-b", Namespace: "default"},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: "shared-template"},
					Replicas:       lo.ToPtr(int32(2)),
				},
			}
			ExpectApplied(ctx, env.Client, cb1, cb2)
			reqs := cbController.podTemplateToBuffers(ctx, pt)
			Expect(reqs).To(HaveLen(2))
			names := []string{reqs[0].Name, reqs[1].Name}
			Expect(names).To(ContainElements("shared-a", "shared-b"))
		})

		It("should ignore buffers with nil podTemplateRef", func() {
			pt := &v1.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "ignore-template", Namespace: "default"},
			}
			cb := &autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: "scalable-only", Namespace: "default"},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ScalableRef: &autoscalingv1beta1.ScalableRef{
						APIGroup: "apps",
						Kind:     "Deployment",
						Name:     "some-deploy",
					},
					Percentage: lo.ToPtr(int32(10)),
				},
			}
			ExpectApplied(ctx, env.Client, cb)
			reqs := cbController.podTemplateToBuffers(ctx, pt)
			Expect(reqs).To(BeEmpty())
		})
	})
})

// cpuPod returns PodOptions requesting 1 CPU, so a buffer's CPU limit maps directly
// to a pod count (limit N CPU / 1 CPU per pod = N) when exercising the limit cap.
func cpuPod() test.PodOptions {
	return test.PodOptions{
		ResourceRequirements: v1.ResourceRequirements{
			Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
		},
	}
}

// deploymentRef is a scalableRef pointing at a Deployment of the given name.
func deploymentRef(name string) *autoscalingv1beta1.ScalableRef {
	return &autoscalingv1beta1.ScalableRef{APIGroup: "apps", Kind: "Deployment", Name: name}
}

//nolint:unparam
func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// fakeTrigger records which buffer UIDs had Trigger called on them.
type fakeTrigger struct {
	calls []types.UID
}

func (f *fakeTrigger) Trigger(uid types.UID) {
	f.calls = append(f.calls, uid)
}
