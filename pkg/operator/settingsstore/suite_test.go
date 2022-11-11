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
	"github.com/aws/karpenter-core/pkg/operator/injection"
	"github.com/aws/karpenter-core/pkg/operator/options"
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
	RunSpecs(t, "SettingsStore")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme)
	cmw = informer.NewInformedWatcher(env.KubernetesInterface, system.Namespace())
	defaultConfigMap = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "karpenter-global-settings",
			Namespace: system.Namespace(),
		},
	}
	ExpectApplied(ctx, env.Client, defaultConfigMap)
	ss = settingsstore.NewWatcherOrDie(ctx, env.KubernetesInterface, cmw, settings.Registration, fake.SettingsRegistration)
	Expect(cmw.Start(env.Done)).To(Succeed())
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed())
})

var _ = Describe("SettingsStore", func() {
	Context("ConfigMap", func() {
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
			ExpectDeleted(ctx, env.Client, defaultConfigMap.DeepCopy())
		})
		Context("Operator Settings", func() {
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
		Context("Multiple Settings", func() {
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
	})
	Context("Local", func() {
		It("should parse local settings file", func() {
			ctx = injection.WithOptions(ctx, options.Options{
				SettingsFile: "./testdata/testsettings.yaml",
			})
			ss := settingsstore.NewLocalStoreOrDie(ctx, settings.Registration)
			testCtx := ss.InjectSettings(ctx)
			s := settings.FromContext(testCtx)
			Expect(s.BatchIdleDuration.Duration).To(Equal(2 * time.Second))
			Expect(s.BatchMaxDuration.Duration).To(Equal(15 * time.Second))
		})
		It("should parse local settings file with defaults", func() {
			ctx = injection.WithOptions(ctx, options.Options{
				SettingsFile: "./testdata/testdefaults.yaml",
			})
			ss := settingsstore.NewLocalStoreOrDie(ctx, settings.Registration)
			testCtx := ss.InjectSettings(ctx)
			s := settings.FromContext(testCtx)
			Expect(s.BatchIdleDuration.Duration).To(Equal(1 * time.Second))
			Expect(s.BatchMaxDuration.Duration).To(Equal(10 * time.Second))
		})
	})
})
