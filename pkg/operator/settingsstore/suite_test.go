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
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/configmap/informer"
	"knative.dev/pkg/system"

	"github.com/aws/karpenter-core/pkg/apis/config"
	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	"github.com/aws/karpenter-core/pkg/operator/settingsstore"
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

var _ = BeforeEach(func() {
	env = test.NewEnvironment(ctx, func(e *test.Environment) {
		clientSet := kubernetes.NewForConfigOrDie(e.Config)
		cmw = informer.NewInformedWatcher(clientSet, system.Namespace())

		defaultConfigMap = &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "karpenter-global-settings",
				Namespace: system.Namespace(),
			},
		}
		ExpectApplied(ctx, e.Client, defaultConfigMap)
	})
	Expect(env.Start()).To(Succeed())
})

var _ = AfterEach(func() {
	Expect(env.Client.Delete(ctx, defaultConfigMap.DeepCopy())).To(Succeed())
	Expect(env.Stop()).To(Succeed())
})

var _ = Describe("Operator Settings", func() {
	BeforeEach(func() {
		ss = settingsstore.WatchSettings(env.Ctx, cmw, settings.Registration)
		Expect(cmw.Start(env.Ctx.Done())).To(Succeed())
	})
	It("should have default values", func() {
		Eventually(func(g Gomega) {
			testCtx := ss.InjectSettings(ctx)
			s := settings.FromContext(testCtx)
			g.Expect(s.BatchIdleDuration).To(Equal(1 * time.Second))
			g.Expect(s.BatchMaxDuration).To(Equal(10 * time.Second))
		}).Should(Succeed())
	})
	It("should update if values are changed", func() {
		Eventually(func(g Gomega) {
			testCtx := ss.InjectSettings(ctx)
			s := settings.FromContext(testCtx)
			g.Expect(s.BatchIdleDuration).To(Equal(1 * time.Second))
			g.Expect(s.BatchMaxDuration).To(Equal(10 * time.Second))
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
			g.Expect(s.BatchIdleDuration).To(Equal(2 * time.Second))
			g.Expect(s.BatchMaxDuration).To(Equal(15 * time.Second))
		}).Should(Succeed())
	})
})

var _ = Describe("Multiple Settings", func() {
	It("should get operator settings and features from same configMap", func() {
		ss = settingsstore.WatchSettings(env.Ctx, cmw, settings.Registration, Registration)
		Expect(cmw.Start(env.Ctx.Done())).To(Succeed())
		Eventually(func(g Gomega) {
			testCtx := ss.InjectSettings(ctx)
			s := FromContext(testCtx)
			g.Expect(s.TestArg).To(Equal("default"))
		}).Should(Succeed())
	})
	It("should get operator settings and features from same configMap", func() {
		ss = settingsstore.WatchSettings(env.Ctx, cmw, settings.Registration, Registration)
		Expect(cmw.Start(env.Ctx.Done())).To(Succeed())

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
			fs := FromContext(testCtx)
			g.Expect(s.BatchIdleDuration).To(Equal(2 * time.Second))
			g.Expect(s.BatchMaxDuration).To(Equal(15 * time.Second))
			g.Expect(fs.TestArg).To(Equal("my-value"))
		}).Should(Succeed())
	})
})

// FakeSettings Registration START -----------

var Registration = &config.Registration{
	ConfigMapName: "karpenter-global-settings",
	Constructor:   NewFakeSettingsFromConfigMap,
}

var defaultSettings = FakeSettings{
	TestArg: "default",
}

type FakeSettings struct {
	TestArg string
}

func NewFakeSettingsFromConfigMap(cm *v1.ConfigMap) (FakeSettings, error) {
	s := defaultSettings

	if err := configmap.Parse(cm.Data,
		configmap.AsString("testArg", &s.TestArg),
	); err != nil {
		// Failing to parse means that there is some error in the Settings, so we should crash
		panic(fmt.Sprintf("parsing config data, %v", err))
	}
	return s, nil
}

func FromContext(ctx context.Context) FakeSettings {
	data := ctx.Value(Registration)
	if data == nil {
		// This is developer error if this happens, so we should panic
		panic("settings doesn't exist in context")
	}
	return data.(FakeSettings)
}
