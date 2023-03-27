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
	v1 "k8s.io/api/core/v1"
	. "knative.dev/pkg/logging/testing"

	"github.com/aws/karpenter-core/pkg/apis/settings"
)

var ctx context.Context

func TestSettings(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Settings")
}

var _ = Describe("Validation", func() {
	It("should succeed to set defaults", func() {
		cm := &v1.ConfigMap{
			Data: map[string]string{},
		}
		ctx, err := (&settings.Settings{}).Inject(ctx, cm)
		Expect(err).ToNot(HaveOccurred())
		s := settings.FromContext(ctx)
		Expect(s.BatchMaxDuration.Duration).To(Equal(time.Second * 10))
		Expect(s.BatchIdleDuration.Duration).To(Equal(time.Second))
		Expect(s.DriftEnabled).To(BeFalse())
	})
	It("should succeed to set custom values", func() {
		cm := &v1.ConfigMap{
			Data: map[string]string{
				"batchMaxDuration":          "30s",
				"batchIdleDuration":         "5s",
				"featureGates.driftEnabled": "true",
			},
		}
		ctx, err := (&settings.Settings{}).Inject(ctx, cm)
		Expect(err).ToNot(HaveOccurred())
		s := settings.FromContext(ctx)
		Expect(s.BatchMaxDuration.Duration).To(Equal(time.Second * 30))
		Expect(s.BatchIdleDuration.Duration).To(Equal(time.Second * 5))
		Expect(s.DriftEnabled).To(BeTrue())
	})
	It("should fail validation when batchMaxDuration is negative", func() {
		cm := &v1.ConfigMap{
			Data: map[string]string{
				"batchMaxDuration": "-10s",
			},
		}
		_, err := (&settings.Settings{}).Inject(ctx, cm)
		Expect(err).To(HaveOccurred())
	})
	It("should fail validation when batchMaxDuration is set to empty", func() {
		cm := &v1.ConfigMap{
			Data: map[string]string{
				"batchMaxDuration": "",
			},
		}
		_, err := (&settings.Settings{}).Inject(ctx, cm)
		Expect(err).To(HaveOccurred())
	})
	It("should fail validation when batchIdleDuration is negative", func() {
		cm := &v1.ConfigMap{
			Data: map[string]string{
				"batchIdleDuration": "-1s",
			},
		}
		_, err := (&settings.Settings{}).Inject(ctx, cm)
		Expect(err).To(HaveOccurred())
	})
	It("should fail validation when batchIdleDuration is set to empty", func() {
		cm := &v1.ConfigMap{
			Data: map[string]string{
				"batchMaxDuration": "",
			},
		}
		_, err := (&settings.Settings{}).Inject(ctx, cm)
		Expect(err).To(HaveOccurred())
	})
	It("should fail validation when driftEnabled is not a valid boolean value", func() {
		cm := &v1.ConfigMap{
			Data: map[string]string{
				"featureGates.driftEnabled": "foobar",
			},
		}
		_, err := (&settings.Settings{}).Inject(ctx, cm)
		Expect(err).To(HaveOccurred())
	})
})
