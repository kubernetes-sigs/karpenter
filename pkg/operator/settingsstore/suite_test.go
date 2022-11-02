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

package settingsstore_test

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/configmap/informer"
	"knative.dev/pkg/system"

	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/operator/settingsstore"
	"github.com/aws/karpenter-core/pkg/operator/settingsstore/fake"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var env *test.Environment
var cmw *informer.InformedWatcher
var ss settingsstore.Store
var defaultConfigMap *v1.ConfigMap

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme)
	cmw = informer.NewInformedWatcher(env.KubernetesInterface, system.Namespace())
	ss = settingsstore.WatchSettingsOrDie(ctx, env.KubernetesInterface, cmw, settings.Registration, fake.SettingsRegistration)
	Expect(cmw.Start(env.Done)).To(Succeed())
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed())
})

var _ = BeforeEach(func() {
	defaultConfigMap = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "karpenter-global-settings",
			Namespace: system.Namespace(),
		},
	}
	ExpectApplied(ctx, env.Client, defaultConfigMap)
})

var _ = AfterEach(func() {
	ExpectDeleted(ctx, env.Client, defaultConfigMap.DeepCopy())
})

var _ = Describe("Operator Settings", func() {
	It("should have default values", func() {
		Eventually(func(g Gomega) {
			testCtx := ss.InjectSettings(ctx)
			s := settings.FromContext(testCtx)
			g.Expect(s.BatchIdleDuration.Duration).To(Equal(1 * time.Second))
			g.Expect(s.BatchMaxDuration.Duration).To(Equal(10 * time.Second))
		}).Should(Succeed())
	})
	It("should update if values are changed", func() {
		Eventually(func(g Gomega) {
			testCtx := ss.InjectSettings(ctx)
			s := settings.FromContext(testCtx)
			g.Expect(s.BatchIdleDuration.Duration).To(Equal(1 * time.Second))
			g.Expect(s.BatchMaxDuration.Duration).To(Equal(10 * time.Second))
		})
		cm := defaultConfigMap.DeepCopy()
		cm.Data = map[string]string{
			"batchIdleDuration": "2s",
			"batchMaxDuration":  "15s",
		}
		ExpectApplied(ctx, env.Client, cm)

		Eventually(func(g Gomega) {
			testCtx := ss.InjectSettings(ctx)
			s := settings.FromContext(testCtx)
			g.Expect(s.BatchIdleDuration.Duration).To(Equal(2 * time.Second))
			g.Expect(s.BatchMaxDuration.Duration).To(Equal(15 * time.Second))
		}).Should(Succeed())
	})
})

var _ = Describe("Multiple Settings", func() {
	It("should get operator settings and features from same configMap", func() {
		Eventually(func(g Gomega) {
			testCtx := ss.InjectSettings(ctx)
			s := fake.SettingsFromContext(testCtx)
			g.Expect(s.TestArg).To(Equal("default"))
		}).Should(Succeed())
	})
	It("should get operator settings and features from same configMap", func() {
		cm := defaultConfigMap.DeepCopy()
		cm.Data = map[string]string{
			"batchIdleDuration": "2s",
			"batchMaxDuration":  "15s",
			"testArg":           "my-value",
		}
		ExpectApplied(ctx, env.Client, cm)

		Eventually(func(g Gomega) {
			testCtx := ss.InjectSettings(ctx)
			s := settings.FromContext(testCtx)
			fs := fake.SettingsFromContext(testCtx)
			g.Expect(s.BatchIdleDuration.Duration).To(Equal(2 * time.Second))
			g.Expect(s.BatchMaxDuration.Duration).To(Equal(15 * time.Second))
			g.Expect(fs.TestArg).To(Equal("my-value"))
		}).Should(Succeed())
	})
})

var _ = Describe("ConfigMap Doesn't Exist on Startup", func() {
	It("should default if the configMap doesn't exist on startup", func() {
		Eventually(func(g Gomega) {
			testCtx := ss.InjectSettings(ctx)
			s := settings.FromContext(testCtx)
			g.Expect(s.BatchIdleDuration.Duration).To(Equal(1 * time.Second))
			g.Expect(s.BatchMaxDuration.Duration).To(Equal(10 * time.Second))
		}).Should(Succeed())
	})
	It("should start watching settings when ConfigMap is added", func() {
		cm := defaultConfigMap.DeepCopy()
		cm.Data = map[string]string{
			"batchIdleDuration": "2s",
			"batchMaxDuration":  "15s",
		}
		ExpectApplied(ctx, env.Client, cm)

		Eventually(func(g Gomega) {
			testCtx := ss.InjectSettings(ctx)
			s := settings.FromContext(testCtx)
			g.Expect(s.BatchIdleDuration.Duration).To(Equal(2 * time.Second))
			g.Expect(s.BatchMaxDuration.Duration).To(Equal(15 * time.Second))
		}).Should(Succeed())
	})
})
