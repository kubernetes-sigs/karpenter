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

package controller_test

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/system"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	"github.com/aws/karpenter-core/pkg/operator/controller"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Inject Settings", func() {
	BeforeEach(func() {
		defaultConfigMap = &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "karpenter-global-settings",
				Namespace: system.Namespace(),
			},
		}
		ExpectApplied(ctx, env.Client, defaultConfigMap)
	})
	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
	})

	It("should inject default settings into Reconcile loop", func() {
		ExpectApplied(ctx, env.Client, defaultConfigMap.DeepCopy())
		expected := settings.Settings{
			BatchMaxDuration:  metav1.Duration{Duration: time.Second * 10},
			BatchIdleDuration: metav1.Duration{Duration: time.Second * 1},
		}

		fakeController := &FakeController{
			ReconcileAssertions: []ReconcileAssertion{
				ExpectOperatorSettingsInjected(expected),
			},
		}
		c := controller.InjectSettings(fakeController, ss)
		Eventually(func(g Gomega) {
			innerCtx := GomegaWithContext(ctx, g)
			_, err := c.Reconcile(innerCtx, reconcile.Request{})
			g.Expect(err).To(Succeed())
		}).Should(Succeed())
	})
	It("should inject custom settings into Reconcile loop", func() {
		expected := settings.Settings{
			BatchMaxDuration:  metav1.Duration{Duration: time.Second * 30},
			BatchIdleDuration: metav1.Duration{Duration: time.Second * 5},
		}
		cm := defaultConfigMap.DeepCopy()
		cm.Data = map[string]string{
			"batchMaxDuration":  expected.BatchMaxDuration.Duration.String(),
			"batchIdleDuration": expected.BatchIdleDuration.Duration.String(),
		}
		ExpectApplied(ctx, env.Client, cm)

		fakeController := &FakeController{
			ReconcileAssertions: []ReconcileAssertion{
				ExpectOperatorSettingsInjected(expected),
			},
		}
		c := controller.InjectSettings(fakeController, ss)
		Eventually(func(g Gomega) {
			innerCtx := GomegaWithContext(ctx, g)
			_, err := c.Reconcile(innerCtx, reconcile.Request{})
			g.Expect(err).To(Succeed())
		}).Should(Succeed())
	})
})

func ExpectSettingsMatch(g Gomega, a settings.Settings, b settings.Settings) {
	g.Expect(a.BatchMaxDuration.Duration == b.BatchMaxDuration.Duration &&
		a.BatchIdleDuration.Duration == b.BatchIdleDuration.Duration).To(BeTrue())
}

func ExpectOperatorSettingsInjected(expected settings.Settings) ReconcileAssertion {
	return func(ctx context.Context, _ reconcile.Request) {
		settings := settings.FromContext(ctx)
		ExpectSettingsMatch(GomegaFromContext(ctx), expected, settings)
	}
}

type ReconcileAssertion func(context.Context, reconcile.Request)

type FakeController struct {
	ReconcileAssertions []ReconcileAssertion
}

func (c *FakeController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	for _, elem := range c.ReconcileAssertions {
		elem(ctx, req)
	}
	return reconcile.Result{}, nil
}

func (c *FakeController) Builder(_ context.Context, _ manager.Manager) controller.Builder {
	return nil
}
