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
	"flag"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/operator/options"
)

var ctx context.Context
var fs *options.FlagSet
var opts *options.Options

func TestOptions(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Options")
}

var _ = Describe("Options", func() {
	BeforeEach(func() {
		fs = &options.FlagSet{
			FlagSet: flag.NewFlagSet("karpenter", flag.ContinueOnError),
		}
		opts = &options.Options{}
		opts.AddFlags(fs)
	})

	Context("FeatureGates", func() {
		DescribeTable(
			"should successfully parse well formed feature gate strings",
			func(str string, driftVal bool) {
				gates, err := options.ParseFeatureGates(str)
				Expect(err).To(BeNil())
				Expect(gates.Drift).To(Equal(driftVal))
			},
			Entry("basic true", "Drift=true", true),
			Entry("basic false", "Drift=false", false),
			Entry("with whitespace", "Drift\t= false", false),
			Entry("multiple values", "Hello=true,Drift=false,World=true", false),
		)
	})
	Context("Parse", func() {
		It("should use the correct default values", func() {
			err := opts.Parse(fs)
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
		BeforeEach(func() {
			ctx = settings.ToContext(ctx, &settings.Settings{
				BatchMaxDuration:  50 * time.Second,
				BatchIdleDuration: 50 * time.Second,
				DriftEnabled:      true,
			})
		})

		It("shouldn't overwrite BatchMaxDuration when specified by CLI", func() {
			err := opts.Parse(fs, "--batch-max-duration", "1s")
			Expect(err).To(BeNil())
			opts.MergeSettings(ctx)
			Expect(opts.BatchMaxDuration).To(Equal(time.Second))
		})
		It("shouldn't overwrite BatchIdleDuration when specified by CLI", func() {
			err := opts.Parse(fs, "--batch-idle-duration", "1s")
			Expect(err).To(BeNil())
			opts.MergeSettings(ctx)
			Expect(opts.BatchIdleDuration).To(Equal(time.Second))
		})
		It("shouldn't overwrite FeatureGates.Drift when specified by CLI", func() {
			err := opts.Parse(fs, "--feature-gates", "Drift=false")
			Expect(err).To(BeNil())
			opts.MergeSettings(ctx)
			Expect(opts.FeatureGates.Drift).To(BeFalse())
		})
		It("should use values from settings when not specified", func() {
			err := opts.Parse(fs, "--batch-max-duration", "1s", "--feature-gates", "Drift=false")
			Expect(err).To(BeNil())
			opts.MergeSettings(ctx)
			Expect(opts.BatchIdleDuration).To(Equal(50 * time.Second))
		})
	})

	Context("Validation", func() {
		DescribeTable(
			"should parse valid log levels successfully",
			func(level string) {
				err := opts.Parse(fs, "--log-level", level)
				Expect(err).To(BeNil())
			},
			Entry("empty string", ""),
			Entry("debug", "debug"),
			Entry("info", "info"),
			Entry("error", "error"),
		)
		It("should error with an invalid log level", func() {
			err := opts.Parse(fs, "--log-level", "hello")
			Expect(err).ToNot(BeNil())
		})
	})
})
