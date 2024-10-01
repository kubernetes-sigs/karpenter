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

package webhookownerreference_test

import (
	"context"
	"os"
	"testing"

	"github.com/samber/lo"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/controllers/webhookownerreference"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "WebhookOwnerReference")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	ctx = options.ToContext(ctx, test.Options())
	os.Setenv("SYSTEM_NAMESPACE", "test")
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("WebhookOwnerReference", func() {
	It("should remove the ownerReference from the webhook configuration", func() {
		controller := webhookownerreference.NewController[*admissionregistrationv1.ValidatingWebhookConfiguration](env.Client, "test.validating.configuration")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
		ExpectApplied(ctx, env.Client, ns)

		config := &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test.validating.configuration",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "v1",
						Kind:       "Namespace",
						Name:       ns.Name,
						UID:        ns.UID,
					},
				},
			},
			Webhooks: []admissionregistrationv1.ValidatingWebhook{
				{
					Name:        "test.validating.configuration",
					SideEffects: lo.ToPtr(admissionregistrationv1.SideEffectClassNone),
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{
							Name:      "test",
							Namespace: ns.Name,
							Path:      lo.ToPtr("/validate/test.validating.configuration"),
							Port:      lo.ToPtr[int32](8443),
						},
					},
					AdmissionReviewVersions: []string{"v1"},
				},
			},
		}
		ExpectApplied(ctx, env.Client, config)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler[*admissionregistrationv1.ValidatingWebhookConfiguration](env.Client, controller), client.ObjectKeyFromObject(config))

		config = ExpectExists(ctx, env.Client, config)
		Expect(config.OwnerReferences).To(HaveLen(0))
	})
	It("should not remove additional ownerReferences from a webhook configuration", func() {
		controller := webhookownerreference.NewController[*admissionregistrationv1.ValidatingWebhookConfiguration](env.Client, "test.validating.configuration")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
		otherNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-test"}}
		ExpectApplied(ctx, env.Client, ns, otherNS)

		config := &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test.validating.configuration",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "v1",
						Kind:       "Namespace",
						Name:       ns.Name,
						UID:        ns.UID,
					},
					{
						APIVersion: "v1",
						Kind:       "Namespace",
						Name:       otherNS.Name,
						UID:        otherNS.UID,
					},
				},
			},
			Webhooks: []admissionregistrationv1.ValidatingWebhook{
				{
					Name:        "test.validating.configuration",
					SideEffects: lo.ToPtr(admissionregistrationv1.SideEffectClassNone),
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{
							Name:      "test",
							Namespace: ns.Name,
							Path:      lo.ToPtr("/validate/test.validating.configuration"),
							Port:      lo.ToPtr[int32](8443),
						},
					},
					AdmissionReviewVersions: []string{"v1"},
				},
			},
		}
		ExpectApplied(ctx, env.Client, config)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler[*admissionregistrationv1.ValidatingWebhookConfiguration](env.Client, controller), client.ObjectKeyFromObject(config))

		config = ExpectExists(ctx, env.Client, config)
		Expect(config.OwnerReferences).To(HaveLen(1))
	})
	It("should not remove ownerReferences from a webhook configuration that doesn't match the name", func() {
		controller := webhookownerreference.NewController[*admissionregistrationv1.ValidatingWebhookConfiguration](env.Client, "test.validating.configuration")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
		ExpectApplied(ctx, env.Client, ns)

		config := &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test.validating.other",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "v1",
						Kind:       "Namespace",
						Name:       ns.Name,
						UID:        ns.UID,
					},
				},
			},
			Webhooks: []admissionregistrationv1.ValidatingWebhook{
				{
					Name:        "test.validating.other",
					SideEffects: lo.ToPtr(admissionregistrationv1.SideEffectClassNone),
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						Service: &admissionregistrationv1.ServiceReference{
							Name:      "test",
							Namespace: ns.Name,
							Path:      lo.ToPtr("/validate/test.validating.other"),
							Port:      lo.ToPtr[int32](8443),
						},
					},
					AdmissionReviewVersions: []string{"v1"},
				},
			},
		}
		ExpectApplied(ctx, env.Client, config)
		ExpectReconcileSucceeded(ctx, reconcile.AsReconciler[*admissionregistrationv1.ValidatingWebhookConfiguration](env.Client, controller), client.ObjectKeyFromObject(config))

		config = ExpectExists(ctx, env.Client, config)
		Expect(config.OwnerReferences).To(HaveLen(1))
	})
})
