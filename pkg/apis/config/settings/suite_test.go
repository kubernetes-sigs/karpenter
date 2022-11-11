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

package settings_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	. "knative.dev/pkg/logging/testing"

	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/apis/config/settings"
)

var ctx context.Context

func TestSettings(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Settings")
}

var _ = Describe("Settings", func() {
	It("should succeed to set defaults", func() {
		cm := &v1.ConfigMap{
			Data: map[string]string{},
		}
		s := lo.Must(settings.NewSettingsFromConfigMap(cm)).(settings.Settings)
		Expect(s.BatchMaxDuration.Duration).To(Equal(time.Second * 10))
		Expect(s.BatchIdleDuration.Duration).To(Equal(time.Second))
	})
	It("should succeed to set custom values", func() {
		cm := &v1.ConfigMap{
			Data: map[string]string{
				"batchMaxDuration":  "30s",
				"batchIdleDuration": "5s",
			},
		}
		s := lo.Must(settings.NewSettingsFromConfigMap(cm)).(settings.Settings)
		Expect(s.BatchMaxDuration.Duration).To(Equal(time.Second * 30))
		Expect(s.BatchIdleDuration.Duration).To(Equal(time.Second * 5))
	})
	It("should fail validation with panic when batchMaxDuration is negative", func() {
		defer ExpectPanic()
		cm := &v1.ConfigMap{
			Data: map[string]string{
				"batchMaxDuration": "-10s",
			},
		}
		_, _ = settings.NewSettingsFromConfigMap(cm)
	})
	It("should fail validation with panic when batchIdleDuration is negative", func() {
		defer ExpectPanic()
		cm := &v1.ConfigMap{
			Data: map[string]string{
				"batchIdleDuration": "-1s",
			},
		}
		_, _ = settings.NewSettingsFromConfigMap(cm)
	})
})
