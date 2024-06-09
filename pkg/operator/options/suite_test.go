/*
Copyright The Kubernetes Authors.

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
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
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
	var environmentVariables = []string{
		"KARPENTER_SERVICE",
		"DISABLE_WEBHOOK",
		"WEBHOOK_PORT",
		"METRICS_PORT",
		"WEBHOOK_METRICS_PORT",
		"HEALTH_PROBE_PORT",
		"KUBE_CLIENT_QPS",
		"KUBE_CLIENT_BURST",
		"ENABLE_PROFILING",
		"LEADER_ELECT",
		"MEMORY_LIMIT",
		"LOG_LEVEL",
		"BATCH_MAX_DURATION",
		"BATCH_IDLE_DURATION",
		"FEATURE_GATES",
	}

	BeforeEach(func() {
		fs = &options.FlagSet{
			FlagSet: flag.NewFlagSet("karpenter", flag.ContinueOnError),
		}
		opts = &options.Options{}
		opts.AddFlags(fs)
	})

	AfterEach(func() {
		for _, ev := range environmentVariables {
			Expect(os.Unsetenv(ev)).To(Succeed())
		}
	})

	Context("FeatureGates", func() {
		DescribeTable(
			"should successfully parse well formed feature gate strings",
			func(str string, spotToSpotConsolidationVal bool) {
				gates, err := options.ParseFeatureGates(str)
				Expect(err).To(BeNil())
				Expect(gates.SpotToSpotConsolidation).To(Equal(spotToSpotConsolidationVal))
			},
			Entry("basic true", "SpotToSpotConsolidation=true", true),
			Entry("basic false", "SpotToSpotConsolidation=false", false),
			Entry("with whitespace", "SpotToSpotConsolidation\t= false", false),
			Entry("multiple values", "Hello=true,SpotToSpotConsolidation=false,World=true", false),
		)
	})

	Context("Parse", func() {
		It("should use the correct default values", func() {
			err := opts.Parse(fs)
			Expect(err).To(BeNil())
			expectOptionsMatch(opts, test.Options(test.OptionsFields{
				ServiceName:          lo.ToPtr(""),
				DisableWebhook:       lo.ToPtr(true),
				WebhookPort:          lo.ToPtr(8443),
				MetricsPort:          lo.ToPtr(8000),
				WebhookMetricsPort:   lo.ToPtr(8001),
				HealthProbePort:      lo.ToPtr(8081),
				KubeClientQPS:        lo.ToPtr(200),
				KubeClientBurst:      lo.ToPtr(300),
				EnableProfiling:      lo.ToPtr(false),
				EnableLeaderElection: lo.ToPtr(true),
				MemoryLimit:          lo.ToPtr[int64](-1),
				LogLevel:             lo.ToPtr("info"),
				BatchMaxDuration:     lo.ToPtr(10 * time.Second),
				BatchIdleDuration:    lo.ToPtr(time.Second),
				FeatureGates: test.FeatureGates{
					SpotToSpotConsolidation: lo.ToPtr(true),
				},
			}))
		})

		It("shouldn't overwrite CLI flags with environment variables", func() {
			err := opts.Parse(
				fs,
				"--karpenter-service", "cli",
				"--disable-webhook",
				"--webhook-port", "0",
				"--metrics-port", "0",
				"--webhook-metrics-port", "0",
				"--health-probe-port", "0",
				"--kube-client-qps", "0",
				"--kube-client-burst", "0",
				"--enable-profiling",
				"--leader-elect=false",
				"--memory-limit", "0",
				"--log-level", "debug",
				"--batch-max-duration", "5s",
				"--batch-idle-duration", "5s",
				"--feature-gates", "SpotToSpotConsolidation=true",
			)
			Expect(err).To(BeNil())
			expectOptionsMatch(opts, test.Options(test.OptionsFields{
				ServiceName:          lo.ToPtr("cli"),
				DisableWebhook:       lo.ToPtr(true),
				WebhookPort:          lo.ToPtr(0),
				MetricsPort:          lo.ToPtr(0),
				WebhookMetricsPort:   lo.ToPtr(0),
				HealthProbePort:      lo.ToPtr(0),
				KubeClientQPS:        lo.ToPtr(0),
				KubeClientBurst:      lo.ToPtr(0),
				EnableProfiling:      lo.ToPtr(true),
				EnableLeaderElection: lo.ToPtr(false),
				MemoryLimit:          lo.ToPtr[int64](0),
				LogLevel:             lo.ToPtr("debug"),
				BatchMaxDuration:     lo.ToPtr(5 * time.Second),
				BatchIdleDuration:    lo.ToPtr(5 * time.Second),
				FeatureGates: test.FeatureGates{
					SpotToSpotConsolidation: lo.ToPtr(true),
				},
			}))
		})

		It("should use environment variables when CLI flags aren't set", func() {
			os.Setenv("KARPENTER_SERVICE", "env")
			os.Setenv("DISABLE_WEBHOOK", "true")
			os.Setenv("WEBHOOK_PORT", "0")
			os.Setenv("METRICS_PORT", "0")
			os.Setenv("WEBHOOK_METRICS_PORT", "0")
			os.Setenv("HEALTH_PROBE_PORT", "0")
			os.Setenv("KUBE_CLIENT_QPS", "0")
			os.Setenv("KUBE_CLIENT_BURST", "0")
			os.Setenv("ENABLE_PROFILING", "true")
			os.Setenv("LEADER_ELECT", "false")
			os.Setenv("MEMORY_LIMIT", "0")
			os.Setenv("LOG_LEVEL", "debug")
			os.Setenv("BATCH_MAX_DURATION", "5s")
			os.Setenv("BATCH_IDLE_DURATION", "5s")
			os.Setenv("FEATURE_GATES", "SpotToSpotConsolidation=true")
			fs = &options.FlagSet{
				FlagSet: flag.NewFlagSet("karpenter", flag.ContinueOnError),
			}
			opts.AddFlags(fs)
			err := opts.Parse(fs)
			Expect(err).To(BeNil())
			expectOptionsMatch(opts, test.Options(test.OptionsFields{
				ServiceName:          lo.ToPtr("env"),
				DisableWebhook:       lo.ToPtr(true),
				WebhookPort:          lo.ToPtr(0),
				MetricsPort:          lo.ToPtr(0),
				WebhookMetricsPort:   lo.ToPtr(0),
				HealthProbePort:      lo.ToPtr(0),
				KubeClientQPS:        lo.ToPtr(0),
				KubeClientBurst:      lo.ToPtr(0),
				EnableProfiling:      lo.ToPtr(true),
				EnableLeaderElection: lo.ToPtr(false),
				MemoryLimit:          lo.ToPtr[int64](0),
				LogLevel:             lo.ToPtr("debug"),
				BatchMaxDuration:     lo.ToPtr(5 * time.Second),
				BatchIdleDuration:    lo.ToPtr(5 * time.Second),
				FeatureGates: test.FeatureGates{
					SpotToSpotConsolidation: lo.ToPtr(true),
				},
			}))
		})

		It("should correctly merge CLI flags and environment variables", func() {
			os.Setenv("WEBHOOK_PORT", "0")
			os.Setenv("METRICS_PORT", "0")
			os.Setenv("WEBHOOK_METRICS_PORT", "0")
			os.Setenv("HEALTH_PROBE_PORT", "0")
			os.Setenv("KUBE_CLIENT_QPS", "0")
			os.Setenv("KUBE_CLIENT_BURST", "0")
			os.Setenv("ENABLE_PROFILING", "true")
			os.Setenv("LEADER_ELECT", "false")
			os.Setenv("MEMORY_LIMIT", "0")
			os.Setenv("LOG_LEVEL", "debug")
			os.Setenv("BATCH_MAX_DURATION", "5s")
			os.Setenv("BATCH_IDLE_DURATION", "5s")
			os.Setenv("FEATURE_GATES", "SpotToSpotConsolidation=true")
			fs = &options.FlagSet{
				FlagSet: flag.NewFlagSet("karpenter", flag.ContinueOnError),
			}
			opts.AddFlags(fs)
			err := opts.Parse(
				fs,
				"--karpenter-service", "cli",
				"--disable-webhook",
			)
			Expect(err).To(BeNil())
			expectOptionsMatch(opts, test.Options(test.OptionsFields{
				ServiceName:          lo.ToPtr("cli"),
				DisableWebhook:       lo.ToPtr(true),
				WebhookPort:          lo.ToPtr(0),
				MetricsPort:          lo.ToPtr(0),
				WebhookMetricsPort:   lo.ToPtr(0),
				HealthProbePort:      lo.ToPtr(0),
				KubeClientQPS:        lo.ToPtr(0),
				KubeClientBurst:      lo.ToPtr(0),
				EnableProfiling:      lo.ToPtr(true),
				EnableLeaderElection: lo.ToPtr(false),
				MemoryLimit:          lo.ToPtr[int64](0),
				LogLevel:             lo.ToPtr("debug"),
				BatchMaxDuration:     lo.ToPtr(5 * time.Second),
				BatchIdleDuration:    lo.ToPtr(5 * time.Second),
				FeatureGates: test.FeatureGates{
					SpotToSpotConsolidation: lo.ToPtr(true),
				},
			}))
		})

		DescribeTable(
			"should correctly parse boolean values",
			func(arg string, expected bool) {
				err := opts.Parse(fs, arg)
				Expect(err).ToNot(HaveOccurred())
				Expect(opts.DisableWebhook).To(Equal(expected))
			},
			Entry("explicit true", "--disable-webhook=true", true),
			Entry("explicit false", "--disable-webhook=false", false),
			Entry("implicit true", "--disable-webhook", true),
			Entry("implicit true", "", true),
		)
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

func expectOptionsMatch(optsA, optsB *options.Options) {
	GinkgoHelper()
	if optsA == nil && optsB == nil {
		return
	}
	Expect(optsA).ToNot(BeNil())
	Expect(optsB).ToNot(BeNil())
	Expect(optsA.ServiceName).To(Equal(optsB.ServiceName))
	Expect(optsA.DisableWebhook).To(Equal(optsB.DisableWebhook))
	Expect(optsA.WebhookPort).To(Equal(optsB.WebhookPort))
	Expect(optsA.MetricsPort).To(Equal(optsB.MetricsPort))
	Expect(optsA.WebhookMetricsPort).To(Equal(optsB.WebhookMetricsPort))
	Expect(optsA.HealthProbePort).To(Equal(optsB.HealthProbePort))
	Expect(optsA.KubeClientQPS).To(Equal(optsB.KubeClientQPS))
	Expect(optsA.KubeClientBurst).To(Equal(optsB.KubeClientBurst))
	Expect(optsA.EnableProfiling).To(Equal(optsB.EnableProfiling))
	Expect(optsA.EnableLeaderElection).To(Equal(optsB.EnableLeaderElection))
	Expect(optsA.MemoryLimit).To(Equal(optsB.MemoryLimit))
	Expect(optsA.LogLevel).To(Equal(optsB.LogLevel))
	Expect(optsA.BatchMaxDuration).To(Equal(optsB.BatchMaxDuration))
	Expect(optsA.BatchIdleDuration).To(Equal(optsB.BatchIdleDuration))
	Expect(optsA.FeatureGates.SpotToSpotConsolidation).To(Equal(optsB.FeatureGates.SpotToSpotConsolidation))
}
