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

package global

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"
)

var ctx context.Context

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
		Config = config{}
		os.Clearenv()
	})

	AfterEach(func() {
		for _, ev := range environmentVariables {
			Expect(os.Unsetenv(ev)).To(Succeed())
		}
	})

	Context("FeatureGates", func() {
		DescribeTable(
			"should successfully parse well formed feature gate strings",
			func(str string, driftVal bool) {
				Config.FeatureGates = FeatureGates{inputStr: str}
				err := ParseFeatureGates()
				Expect(err).To(BeNil())
				Expect(Config.FeatureGates.Drift).To(Equal(driftVal))
			},
			Entry("basic true", "Drift=true", true),
			Entry("basic false", "Drift=false", false),
			Entry("with whitespace", "Drift\t= false", false),
			Entry("multiple values", "Hello=true,Drift=false,World=true", false),
		)
	})

	Context("Parse", func() {
		It("should use the correct default values", func() {
			err := Initialize()
			Expect(err).To(BeNil())
			expectConfigEqual(Config, config{
				ServiceName:          "",
				DisableWebhook:       true,
				WebhookPort:          8443,
				MetricsPort:          8000,
				WebhookMetricsPort:   8001,
				HealthProbePort:      8081,
				KubeClientQPS:        200,
				KubeClientBurst:      300,
				EnableProfiling:      false,
				EnableLeaderElection: true,
				MemoryLimit:          -1,
				LogLevel:             "info",
				BatchMaxDuration:     10 * time.Second,
				BatchIdleDuration:    time.Second,
				FeatureGates: FeatureGates{
					Drift: true,
				},
			})
		})

		It("shouldn't overwrite CLI flags with environment variables", func() {
			err := Initialize(
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
				"--feature-gates", "Drift=true",
			)
			Expect(err).To(BeNil())
			expectConfigEqual(Config, config{
				ServiceName:          "cli",
				DisableWebhook:       true,
				WebhookPort:          0,
				MetricsPort:          0,
				WebhookMetricsPort:   0,
				HealthProbePort:      0,
				KubeClientQPS:        0,
				KubeClientBurst:      0,
				EnableProfiling:      true,
				EnableLeaderElection: false,
				MemoryLimit:          0,
				LogLevel:             "debug",
				BatchMaxDuration:     5 * time.Second,
				BatchIdleDuration:    5 * time.Second,
				FeatureGates: FeatureGates{
					Drift: true,
				},
			})
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
			os.Setenv("FEATURE_GATES", "Drift=true")

			err := Initialize()
			Expect(err).To(BeNil())
			expectConfigEqual(Config, config{
				ServiceName:          "env",
				DisableWebhook:       true,
				WebhookPort:          0,
				MetricsPort:          0,
				WebhookMetricsPort:   0,
				HealthProbePort:      0,
				KubeClientQPS:        0,
				KubeClientBurst:      0,
				EnableProfiling:      true,
				EnableLeaderElection: false,
				MemoryLimit:          0,
				LogLevel:             "debug",
				BatchMaxDuration:     5 * time.Second,
				BatchIdleDuration:    5 * time.Second,
				FeatureGates: FeatureGates{
					Drift: true,
				},
			})
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
			os.Setenv("FEATURE_GATES", "Drift=true")

			err := Initialize(
				"--karpenter-service", "cli",
				"--disable-webhook",
			)
			Expect(err).To(BeNil())
			expectConfigEqual(Config, config{
				ServiceName:          "cli",
				DisableWebhook:       true,
				WebhookPort:          0,
				MetricsPort:          0,
				WebhookMetricsPort:   0,
				HealthProbePort:      0,
				KubeClientQPS:        0,
				KubeClientBurst:      0,
				EnableProfiling:      true,
				EnableLeaderElection: false,
				MemoryLimit:          0,
				LogLevel:             "debug",
				BatchMaxDuration:     5 * time.Second,
				BatchIdleDuration:    5 * time.Second,
				FeatureGates: FeatureGates{
					Drift: true,
				},
			})
		})

		DescribeTable(
			"should correctly parse boolean values",
			func(arg string, expected bool) {
				err := Initialize(arg)
				Expect(err).ToNot(HaveOccurred())
				Expect(Config.DisableWebhook).To(Equal(expected))
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
				err := Initialize("--log-level", level)
				Expect(err).To(BeNil())
			},
			Entry("empty string", ""),
			Entry("debug", "debug"),
			Entry("info", "info"),
			Entry("error", "error"),
		)
		It("should error with an invalid log level", func() {
			err := Initialize("--log-level", "hello")
			Expect(err).ToNot(BeNil())
		})
	})
})

func expectConfigEqual(configA, configB config) {
	GinkgoHelper()
	Expect(configA.ServiceName).To(Equal(configB.ServiceName))
	Expect(configA.DisableWebhook).To(Equal(configB.DisableWebhook))
	Expect(configA.WebhookPort).To(Equal(configB.WebhookPort))
	Expect(configA.MetricsPort).To(Equal(configB.MetricsPort))
	Expect(configA.WebhookMetricsPort).To(Equal(configB.WebhookMetricsPort))
	Expect(configA.HealthProbePort).To(Equal(configB.HealthProbePort))
	Expect(configA.KubeClientQPS).To(Equal(configB.KubeClientQPS))
	Expect(configA.KubeClientBurst).To(Equal(configB.KubeClientBurst))
	Expect(configA.EnableProfiling).To(Equal(configB.EnableProfiling))
	Expect(configA.EnableLeaderElection).To(Equal(configB.EnableLeaderElection))
	Expect(configA.MemoryLimit).To(Equal(configB.MemoryLimit))
	Expect(configA.LogLevel).To(Equal(configB.LogLevel))
	Expect(configA.BatchMaxDuration).To(Equal(configB.BatchMaxDuration))
	Expect(configA.BatchIdleDuration).To(Equal(configB.BatchIdleDuration))
	Expect(configA.FeatureGates.Drift).To(Equal(configB.FeatureGates.Drift))
}
