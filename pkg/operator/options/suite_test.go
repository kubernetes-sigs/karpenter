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

package options_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/operator/options"
)

var ctx context.Context

func TestOptions(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Options")
}

var _ = Describe("Options", func() {
	Context("Parse", func() {
		It("should correctly parse feature gates", func() {
			ctx, err := options.New().Inject(ctx, "--feature-gates", "Drift=true")
			Expect(err).To(BeNil())
			opts := options.FromContext(ctx)
			Expect(opts.FeatureGates.Drift).To(BeTrue())
			ctx, err = options.New().Inject(ctx, "--feature-gates", "Drift=false")
			Expect(err).To(BeNil())
			opts = options.FromContext(ctx)
			Expect(opts.FeatureGates.Drift).To(BeFalse())
		})
		It("should use the correct default values", func() {
			ctx, err := options.New().Inject(ctx)
			opts := options.FromContext(ctx)
			Expect(err).To(BeNil())
			Expect(opts.ServiceName).To(Equal(""))
			Expect(opts.DisableWebhook).To(BeFalse())
			Expect(opts.WebhookPort).To(Equal(8443))
			Expect(opts.MetricsPort).To(Equal(8000))
			Expect(opts.WebhookMetricsPort).To(Equal(8001))
			Expect(opts.HealthProbePort).To(Equal(8081))
			Expect(opts.KubeClientQPS).To(Equal(200))
			Expect(opts.KubeClientBurst).To(Equal(300))
			Expect(opts.EnableProfiling).To(BeFalse())
			Expect(opts.EnableLeaderElection).To(BeTrue())
			Expect(opts.MemoryLimit).To(Equal(int64(-1)))
			Expect(opts.LogLevel).To(Equal(""))
			Expect(opts.BatchMaxDuration).To(Equal(10 * time.Second))
			Expect(opts.BatchIdleDuration).To(Equal(time.Second))
			Expect(opts.FeatureGates.Drift).To(BeFalse())
		})
	})

	Context("Merge", func() {
		settings := &settings.Settings{
			BatchMaxDuration:  50 * time.Second,
			BatchIdleDuration: 50 * time.Second,
			DriftEnabled:      true,
		}

		It("shouldn't overwrite BatchMaxDuration when specified by CLI", func() {
			ctx, err := options.New().Inject(ctx, "--batch-max-duration", "1s")
			opts := options.FromContext(ctx)
			Expect(err).To(BeNil())
			ctx = opts.MergeSettings(ctx, settings)
			opts = options.FromContext(ctx)
			Expect(opts.BatchMaxDuration).To(Equal(time.Second))
		})
		It("shouldn't overwrite BatchIdleDuration when specified by CLI", func() {
			ctx, err := options.New().Inject(ctx, "--batch-idle-duration", "1s")
			opts := options.FromContext(ctx)
			Expect(err).To(BeNil())
			ctx = opts.MergeSettings(ctx, settings)
			opts = options.FromContext(ctx)
			Expect(opts.BatchIdleDuration).To(Equal(time.Second))
		})
		It("shouldn't overwrite FeatureGates.Drift when specified by CLI", func() {
			ctx, err := options.New().Inject(ctx, "--feature-gates", "Drift=false")
			opts := options.FromContext(ctx)
			Expect(err).To(BeNil())
			ctx = opts.MergeSettings(ctx, settings)
			opts = options.FromContext(ctx)
			Expect(opts.FeatureGates.Drift).To(BeFalse())
		})
		It("should use values from settings when not specified", func() {
			ctx, err := options.New().Inject(ctx, "--batch-max-duration", "1s", "--feature-gates", "Drift=false")
			opts := options.FromContext(ctx)
			Expect(err).To(BeNil())
			ctx = opts.MergeSettings(ctx, settings)
			opts = options.FromContext(ctx)
			Expect(opts.BatchIdleDuration).To(Equal(50 * time.Second))
		})
	})

	Context("Validation", func() {
		It("should parse valid log levels successfully", func() {
			for _, lvl := range []string{"", "debug", "info", "error"} {
				_, err := options.New().Inject(ctx, "--log-level", lvl)
				Expect(err).To(BeNil())
			}
		})
		It("should panic for invalid log levels", func() {
			for _, lvl := range []string{"test", "dbug", "trace", "warn"} {
				_, err := options.New().Inject(ctx, "--log-level", lvl)
				Expect(err).ToNot(BeNil())
			}
		})
	})
})
