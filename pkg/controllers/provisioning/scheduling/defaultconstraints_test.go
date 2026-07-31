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

package scheduling_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// These specs cover the per-pod selector derivation that mirrors kube-scheduler's helper.DefaultSelector. They assert
// through the exported Inject, reading back the constraint it stamps onto the pod, so the derived selector is observed
// exactly as the rest of the scheduling machinery would see it.
var _ = Describe("DefaultTopologySpreadInjector", func() {
	var zoneDefault corev1.TopologySpreadConstraint
	BeforeEach(func() {
		// Mirroring upstream, a default constraint carries no labelSelector; one is deduced per pod.
		zoneDefault = corev1.TopologySpreadConstraint{
			TopologyKey:       corev1.LabelTopologyZone,
			WhenUnsatisfiable: corev1.DoNotSchedule,
			MaxSkew:           1,
		}
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
			SchedulerConfig: &options.SchedulerConfiguration{
				PodTopologySpread: &options.PodTopologySpreadConfig{
					DefaultConstraints: []corev1.TopologySpreadConstraint{zoneDefault},
				},
			},
		}))
	})
	AfterEach(func() {
		ctx = options.ToContext(ctx, test.Options())
	})

	// inject runs the injector over the pod and returns the selector it deduced, or nil if the pod wasn't defaulted.
	inject := func(pod *corev1.Pod) *metav1.LabelSelector {
		GinkgoHelper()
		scheduling.NewDefaultTopologySpreadInjector(env.Client).Inject(ctx, []*corev1.Pod{pod})
		if len(pod.Spec.TopologySpreadConstraints) == 0 {
			return nil
		}
		Expect(pod.Spec.TopologySpreadConstraints).To(HaveLen(1))
		Expect(pod.Spec.TopologySpreadConstraints[0].TopologyKey).To(Equal(corev1.LabelTopologyZone))
		return pod.Spec.TopologySpreadConstraints[0].LabelSelector
	}
	// controllerRef builds the controller owner reference a controller of the given kind would set on its pods.
	controllerRef := func(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: new(true)}
	}
	ownedPod := func(labels map[string]string, owners ...metav1.OwnerReference) *corev1.Pod {
		return test.UnschedulablePod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{
			Labels:          labels,
			OwnerReferences: owners,
		}})
	}

	Context("owner selectors", func() {
		It("should deduce a ReplicaSet's selector, not the pod's labels", func() {
			// The RS selector is the source of truth: a pod label absent from it ("extra") must not appear, and
			// pod-template-hash must, so Deployment pods spread per-revision as they do upstream.
			replicaSet := test.ReplicaSet(test.ReplicaSetOptions{
				Selector: map[string]string{"app": "web", "pod-template-hash": "abc123"},
			})
			ExpectApplied(ctx, env.Client, replicaSet)
			selector := inject(ownedPod(
				map[string]string{"app": "web", "pod-template-hash": "abc123", "extra": "ignored"},
				controllerRef("apps/v1", "ReplicaSet", replicaSet.Name, replicaSet.UID),
			))
			Expect(selector).ToNot(BeNil())
			Expect(selector.MatchLabels).To(BeEmpty())
			Expect(selector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"web"}},
				metav1.LabelSelectorRequirement{Key: "pod-template-hash", Operator: metav1.LabelSelectorOpIn, Values: []string{"abc123"}},
			))
		})
		It("should deduce a StatefulSet's selector and ignore the pod's volatile labels", func() {
			// A StatefulSet pod carries per-pod labels (pod-name, pod-index) that aren't in the SS selector. Deriving
			// from the pod's labels would put every pod in its own topology group, silently voiding the spread.
			statefulSet := test.StatefulSet(test.StatefulSetOptions{
				PodOptions: test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "db"}}},
			})
			ExpectApplied(ctx, env.Client, statefulSet)
			selector := inject(ownedPod(
				map[string]string{
					"app":                                "db",
					"controller-revision-hash":           "db-6f8c",
					"statefulset.kubernetes.io/pod-name": "db-0",
					"apps.kubernetes.io/pod-index":       "0",
				},
				controllerRef("apps/v1", "StatefulSet", statefulSet.Name, statefulSet.UID),
			))
			Expect(selector).ToNot(BeNil())
			Expect(selector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"db"}},
			))
		})
		It("should carry a ReplicaSet's matchExpressions through verbatim", func() {
			// Reading the owner (rather than the pod's labels) is what preserves a non-equality selector.
			replicaSet := test.ReplicaSet(test.ReplicaSetOptions{Selector: map[string]string{"app": "web"}})
			replicaSet.Spec.Selector = &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"other"},
				}},
			}
			ExpectApplied(ctx, env.Client, replicaSet)
			selector := inject(ownedPod(
				map[string]string{"app": "web"},
				controllerRef("apps/v1", "ReplicaSet", replicaSet.Name, replicaSet.UID),
			))
			Expect(selector).ToNot(BeNil())
			Expect(selector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"other"}},
			))
		})
		It("should deduce a ReplicationController's selector", func() {
			rc := &corev1.ReplicationController{
				ObjectMeta: test.NamespacedObjectMeta(),
				Spec: corev1.ReplicationControllerSpec{
					Selector: map[string]string{"app": "legacy"},
					Template: &corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "legacy"}},
						Spec:       test.Pod().Spec,
					},
				},
			}
			ExpectApplied(ctx, env.Client, rc)
			selector := inject(ownedPod(
				map[string]string{"app": "legacy"},
				controllerRef("v1", "ReplicationController", rc.Name, rc.UID),
			))
			Expect(selector).ToNot(BeNil())
			Expect(selector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"legacy"}},
			))
		})
		It("should not deduce a selector from an unsupported owner kind", func() {
			// kube-scheduler only deduces from rc/rs/ss, so a Job-owned pod is defaulted only via a Service.
			Expect(inject(ownedPod(
				map[string]string{"app": "batch"},
				controllerRef("batch/v1", "Job", "test-job", "job-uid"),
			))).To(BeNil())
		})
		It("should ignore a non-controller owner reference", func() {
			// Upstream uses GetControllerOfNoCopy, which only considers the ref with controller=true.
			replicaSet := test.ReplicaSet(test.ReplicaSetOptions{Selector: map[string]string{"app": "web"}})
			ExpectApplied(ctx, env.Client, replicaSet)
			owner := controllerRef("apps/v1", "ReplicaSet", replicaSet.Name, replicaSet.UID)
			owner.Controller = new(false)
			Expect(inject(ownedPod(map[string]string{"app": "web"}, owner))).To(BeNil())
		})
		It("should tolerate an owner that no longer exists", func() {
			// A ReplicaSet can be deleted while its pods linger. Upstream proceeds without the owner's contribution
			// rather than failing, so with no Service either the pod simply isn't defaulted.
			Expect(inject(ownedPod(
				map[string]string{"app": "web"},
				controllerRef("apps/v1", "ReplicaSet", "does-not-exist", "gone-uid"),
			))).To(BeNil())
		})
	})

	Context("service selectors", func() {
		It("should deduce a selector from a matching service", func() {
			ExpectApplied(ctx, env.Client, test.Service(test.ServiceOptions{Selector: map[string]string{"app": "web"}}))
			selector := inject(ownedPod(map[string]string{"app": "web", "extra": "ignored"}))
			Expect(selector).ToNot(BeNil())
			// Service selectors are equality-only, so they contribute matchLabels.
			Expect(selector.MatchLabels).To(Equal(map[string]string{"app": "web"}))
			Expect(selector.MatchExpressions).To(BeEmpty())
		})
		It("should merge the selectors of every matching service", func() {
			ExpectApplied(ctx, env.Client,
				test.Service(test.ServiceOptions{Selector: map[string]string{"app": "web"}}),
				test.Service(test.ServiceOptions{Selector: map[string]string{"tier": "frontend"}}),
			)
			selector := inject(ownedPod(map[string]string{"app": "web", "tier": "frontend"}))
			Expect(selector).ToNot(BeNil())
			Expect(selector.MatchLabels).To(Equal(map[string]string{"app": "web", "tier": "frontend"}))
		})
		It("should not deduce a selector from a non-matching service", func() {
			ExpectApplied(ctx, env.Client, test.Service(test.ServiceOptions{Selector: map[string]string{"app": "other"}}))
			Expect(inject(ownedPod(map[string]string{"app": "web"}))).To(BeNil())
		})
		It("should skip a service with a nil selector", func() {
			// A nil selector matches nothing, not everything - as with a headless service.
			ExpectApplied(ctx, env.Client, test.Service(test.ServiceOptions{Selector: nil}))
			Expect(inject(ownedPod(map[string]string{"app": "web"}))).To(BeNil())
		})
		It("should contribute nothing for a service with an empty selector", func() {
			// An empty (non-nil) selector matches every pod but contributes no labels, so no selector is deduced.
			ExpectApplied(ctx, env.Client, test.Service(test.ServiceOptions{Selector: map[string]string{}}))
			Expect(inject(ownedPod(map[string]string{"app": "web"}))).To(BeNil())
		})
		It("should ignore a matching service in another namespace", func() {
			namespace := test.Namespace()
			ExpectApplied(ctx, env.Client, namespace)
			service := test.Service(test.ServiceOptions{Selector: map[string]string{"app": "web"}})
			service.Namespace = namespace.Name
			ExpectApplied(ctx, env.Client, service)
			Expect(inject(ownedPod(map[string]string{"app": "web"}))).To(BeNil())
		})
		It("should combine a service selector with the owner's", func() {
			// The service contributes matchLabels and the owner matchExpressions, so a key in both is ANDed rather
			// than overwritten - mirroring upstream's selector.Add of the owner's requirements.
			replicaSet := test.ReplicaSet(test.ReplicaSetOptions{
				Selector: map[string]string{"app": "web", "pod-template-hash": "abc123"},
			})
			ExpectApplied(ctx, env.Client, replicaSet,
				test.Service(test.ServiceOptions{Selector: map[string]string{"tier": "frontend"}}))
			selector := inject(ownedPod(
				map[string]string{"app": "web", "pod-template-hash": "abc123", "tier": "frontend"},
				controllerRef("apps/v1", "ReplicaSet", replicaSet.Name, replicaSet.UID),
			))
			Expect(selector).ToNot(BeNil())
			Expect(selector.MatchLabels).To(Equal(map[string]string{"tier": "frontend"}))
			Expect(selector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"web"}},
				metav1.LabelSelectorRequirement{Key: "pod-template-hash", Operator: metav1.LabelSelectorOpIn, Values: []string{"abc123"}},
			))
		})
	})

	Context("injection", func() {

		It("should leave a pod that declares its own constraints untouched", func() {
			own := []corev1.TopologySpreadConstraint{{
				TopologyKey:       corev1.LabelHostname,
				WhenUnsatisfiable: corev1.ScheduleAnyway,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				MaxSkew:           5,
			}}
			replicaSet := test.ReplicaSet(test.ReplicaSetOptions{Selector: map[string]string{"app": "web"}})
			ExpectApplied(ctx, env.Client, replicaSet)
			pod := test.UnschedulablePod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels:          map[string]string{"app": "web"},
					OwnerReferences: []metav1.OwnerReference{controllerRef("apps/v1", "ReplicaSet", replicaSet.Name, replicaSet.UID)},
				},
				TopologySpreadConstraints: own,
			})
			scheduling.NewDefaultTopologySpreadInjector(env.Client).Inject(ctx, []*corev1.Pod{pod})
			Expect(pod.Spec.TopologySpreadConstraints).To(Equal(own))
		})
		It("should be a no-op when no defaults are configured", func() {
			ctx = options.ToContext(ctx, test.Options())
			replicaSet := test.ReplicaSet(test.ReplicaSetOptions{Selector: map[string]string{"app": "web"}})
			ExpectApplied(ctx, env.Client, replicaSet)
			Expect(inject(ownedPod(
				map[string]string{"app": "web"},
				controllerRef("apps/v1", "ReplicaSet", replicaSet.Name, replicaSet.UID),
			))).To(BeNil())
		})
		It("should give each pod its own constraint slice so relaxation can't leak across pods", func() {
			replicaSet := test.ReplicaSet(test.ReplicaSetOptions{Selector: map[string]string{"app": "web"}})
			ExpectApplied(ctx, env.Client, replicaSet)
			owner := controllerRef("apps/v1", "ReplicaSet", replicaSet.Name, replicaSet.UID)
			pods := []*corev1.Pod{
				ownedPod(map[string]string{"app": "web"}, owner),
				ownedPod(map[string]string{"app": "web"}, owner),
			}
			scheduling.NewDefaultTopologySpreadInjector(env.Client).Inject(ctx, pods)
			for _, p := range pods {
				Expect(p.Spec.TopologySpreadConstraints).To(HaveLen(1))
			}
			// Relaxation mutates a pod's slice in place; the other pod must be unaffected.
			pods[0].Spec.TopologySpreadConstraints = nil
			Expect(pods[1].Spec.TopologySpreadConstraints).To(HaveLen(1))
			Expect(pods[1].Spec.TopologySpreadConstraints[0].LabelSelector).ToNot(BeNil())
		})
		It("should preserve the configured constraint fields on the injected copy", func() {
			minDomains := int32(3)
			nodeTaintsPolicy := corev1.NodeInclusionPolicyHonor
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
				SchedulerConfig: &options.SchedulerConfiguration{
					PodTopologySpread: &options.PodTopologySpreadConfig{
						DefaultConstraints: []corev1.TopologySpreadConstraint{{
							TopologyKey:       corev1.LabelTopologyZone,
							WhenUnsatisfiable: corev1.ScheduleAnyway,
							MaxSkew:           4,
							MinDomains:        &minDomains,
							NodeTaintsPolicy:  &nodeTaintsPolicy,
						}},
					},
				},
			}))
			replicaSet := test.ReplicaSet(test.ReplicaSetOptions{Selector: map[string]string{"app": "web"}})
			ExpectApplied(ctx, env.Client, replicaSet)
			pod := ownedPod(map[string]string{"app": "web"}, controllerRef("apps/v1", "ReplicaSet", replicaSet.Name, replicaSet.UID))
			scheduling.NewDefaultTopologySpreadInjector(env.Client).Inject(ctx, []*corev1.Pod{pod})
			Expect(pod.Spec.TopologySpreadConstraints).To(HaveLen(1))
			tsc := pod.Spec.TopologySpreadConstraints[0]
			Expect(tsc.MaxSkew).To(BeEquivalentTo(4))
			Expect(tsc.WhenUnsatisfiable).To(Equal(corev1.ScheduleAnyway))
			Expect(*tsc.MinDomains).To(BeEquivalentTo(3))
			Expect(*tsc.NodeTaintsPolicy).To(Equal(corev1.NodeInclusionPolicyHonor))
			Expect(tsc.LabelSelector).ToNot(BeNil())
		})
		It("should deduce independent selectors for pods of different workloads in one pass", func() {
			// Exercises the per-namespace service and per-owner selector caches across a mixed batch.
			webRS := test.ReplicaSet(test.ReplicaSetOptions{Selector: map[string]string{"app": "web"}})
			apiRS := test.ReplicaSet(test.ReplicaSetOptions{Selector: map[string]string{"app": "api"}})
			ExpectApplied(ctx, env.Client, webRS, apiRS)
			web := ownedPod(map[string]string{"app": "web"}, controllerRef("apps/v1", "ReplicaSet", webRS.Name, webRS.UID))
			api := ownedPod(map[string]string{"app": "api"}, controllerRef("apps/v1", "ReplicaSet", apiRS.Name, apiRS.UID))
			undefaulted := ownedPod(map[string]string{"app": "orphan"})
			scheduling.NewDefaultTopologySpreadInjector(env.Client).Inject(ctx, []*corev1.Pod{web, api, undefaulted})
			Expect(web.Spec.TopologySpreadConstraints[0].LabelSelector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"web"}},
			))
			Expect(api.Spec.TopologySpreadConstraints[0].LabelSelector.MatchExpressions).To(ConsistOf(
				metav1.LabelSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"api"}},
			))
			Expect(undefaulted.Spec.TopologySpreadConstraints).To(BeEmpty())
		})
	})
})
