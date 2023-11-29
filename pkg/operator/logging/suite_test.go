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

package logging_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/system"

	"github.com/aws/karpenter-core/pkg/operator/logging"
	"github.com/aws/karpenter-core/pkg/operator/options"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var cancel context.CancelFunc
var env *test.Environment
var defaultConfigMap *v1.ConfigMap

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Logging")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed())
})

var _ = BeforeEach(func() {
	defaultConfigMap = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "config-logging",
			Namespace: system.Namespace(),
		},
	}
	ctx, cancel = context.WithCancel(context.Background())
	ctx = options.ToContext(ctx, test.Options())
	ExpectApplied(ctx, env.Client, defaultConfigMap)
})

var _ = AfterEach(func() {
	ExpectDeleted(ctx, env.Client, defaultConfigMap.DeepCopy())
	cancel()
})

var _ = Describe("Logging", func() {
	It("should timeout if the config-logging doesn't exist", func() {
		ExpectDeleted(ctx, env.Client, defaultConfigMap)

		finished := atomic.Bool{}
		go func() {
			// This won't succeed to exit in time if the waiter isn't using the correct context
			logging.NewLogger(ctx, "controller", env.KubernetesInterface)
			finished.Store(true)
		}()
		Eventually(func() bool {
			return finished.Load()
		}, time.Second*10, time.Second).Should(BeTrue())
	})
})
