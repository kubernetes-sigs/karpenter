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

	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
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
		"METRICS_PORT",
		"HEALTH_PROBE_PORT",
		"KUBE_CLIENT_QPS",
		"KUBE_CLIENT_BURST",
		"ENABLE_PROFILING",
		"DISABLE_LEADER_ELECTION",
		"LEADER_ELECTION_NAMESPACE",
		"MEMORY_LIMIT",
		"LOG_LEVEL",
		"LOG_OUTPUT_PATHS",
		"LOG_ERROR_OUTPUT_PATHS",
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
				ServiceName:             lo.ToPtr(""),
				MetricsPort:             lo.ToPtr(8080),
				HealthProbePort:         lo.ToPtr(8081),
				KubeClientQPS:           lo.ToPtr(200),
				KubeClientBurst:         lo.ToPtr(300),
				EnableProfiling:         lo.ToPtr(false),
				DisableLeaderElection:   lo.ToPtr(false),
				LeaderElectionName:      lo.ToPtr("karpenter-leader-election"),
				LeaderElectionNamespace: lo.ToPtr(""),
				MemoryLimit:             lo.ToPtr[int64](-1),
				LogLevel:                lo.ToPtr("info"),
				LogOutputPaths:          lo.ToPtr("stdout"),
				LogErrorOutputPaths:     lo.ToPtr("stderr"),
				BatchMaxDuration:        lo.ToPtr(10 * time.Second),
				BatchIdleDuration:       lo.ToPtr(time.Second),
				FeatureGates: test.FeatureGates{
					SpotToSpotConsolidation: lo.ToPtr(false),
				},
			}))
		})

		It("shouldn't overwrite CLI flags with environment variables", func() {
			os.Setenv("LOG_OUTPUT_PATHS", "stdout")
			os.Setenv("LOG_ERROR_OUTPUT_PATHS", "stderr")
			err := opts.Parse(
				fs,
				"--karpenter-service", "cli",
				"--metrics-port", "0",
				"--health-probe-port", "0",
				"--kube-client-qps", "0",
				"--kube-client-burst", "0",
				"--enable-profiling",
				"--disable-leader-election=true",
				"--leader-election-name=karpenter-controller",
				"--leader-election-namespace=karpenter",
				"--memory-limit", "0",
				"--log-level", "debug",
				"--log-output-paths", "/etc/k8s/test",
				"--log-error-output-paths", "/etc/k8s/testerror",
				"--batch-max-duration", "5s",
				"--batch-idle-duration", "5s",
				"--feature-gates", "SpotToSpotConsolidation=true",
			)
			Expect(err).To(BeNil())
			expectOptionsMatch(opts, test.Options(test.OptionsFields{
				ServiceName:             lo.ToPtr("cli"),
				MetricsPort:             lo.ToPtr(0),
				HealthProbePort:         lo.ToPtr(0),
				KubeClientQPS:           lo.ToPtr(0),
				KubeClientBurst:         lo.ToPtr(0),
				EnableProfiling:         lo.ToPtr(true),
				DisableLeaderElection:   lo.ToPtr(true),
				LeaderElectionName:      lo.ToPtr("karpenter-controller"),
				LeaderElectionNamespace: lo.ToPtr("karpenter"),
				MemoryLimit:             lo.ToPtr[int64](0),
				LogLevel:                lo.ToPtr("debug"),
				LogOutputPaths:          lo.ToPtr("/etc/k8s/test"),
				LogErrorOutputPaths:     lo.ToPtr("/etc/k8s/testerror"),
				BatchMaxDuration:        lo.ToPtr(5 * time.Second),
				BatchIdleDuration:       lo.ToPtr(5 * time.Second),
				FeatureGates: test.FeatureGates{
					SpotToSpotConsolidation: lo.ToPtr(true),
				},
			}))
		})

		It("should use environment variables when CLI flags aren't set", func() {
			os.Setenv("KARPENTER_SERVICE", "env")
			os.Setenv("METRICS_PORT", "0")
			os.Setenv("HEALTH_PROBE_PORT", "0")
			os.Setenv("KUBE_CLIENT_QPS", "0")
			os.Setenv("KUBE_CLIENT_BURST", "0")
			os.Setenv("ENABLE_PROFILING", "true")
			os.Setenv("DISABLE_LEADER_ELECTION", "true")
			os.Setenv("LEADER_ELECTION_NAME", "karpenter-controller")
			os.Setenv("LEADER_ELECTION_NAMESPACE", "karpenter")
			os.Setenv("MEMORY_LIMIT", "0")
			os.Setenv("LOG_LEVEL", "debug")
			os.Setenv("LOG_OUTPUT_PATHS", "/etc/k8s/test")
			os.Setenv("LOG_ERROR_OUTPUT_PATHS", "/etc/k8s/testerror")
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
				ServiceName:             lo.ToPtr("env"),
				MetricsPort:             lo.ToPtr(0),
				HealthProbePort:         lo.ToPtr(0),
				KubeClientQPS:           lo.ToPtr(0),
				KubeClientBurst:         lo.ToPtr(0),
				EnableProfiling:         lo.ToPtr(true),
				DisableLeaderElection:   lo.ToPtr(true),
				LeaderElectionName:      lo.ToPtr("karpenter-controller"),
				LeaderElectionNamespace: lo.ToPtr("karpenter"),
				MemoryLimit:             lo.ToPtr[int64](0),
				LogLevel:                lo.ToPtr("debug"),
				LogOutputPaths:          lo.ToPtr("/etc/k8s/test"),
				LogErrorOutputPaths:     lo.ToPtr("/etc/k8s/testerror"),
				BatchMaxDuration:        lo.ToPtr(5 * time.Second),
				BatchIdleDuration:       lo.ToPtr(5 * time.Second),
				FeatureGates: test.FeatureGates{
					SpotToSpotConsolidation: lo.ToPtr(true),
				},
			}))
		})

		It("should correctly merge CLI flags and environment variables", func() {
			os.Setenv("METRICS_PORT", "0")
			os.Setenv("HEALTH_PROBE_PORT", "0")
			os.Setenv("KUBE_CLIENT_QPS", "0")
			os.Setenv("KUBE_CLIENT_BURST", "0")
			os.Setenv("ENABLE_PROFILING", "true")
			os.Setenv("DISABLE_LEADER_ELECTION", "true")
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
				"--log-output-paths", "/etc/k8s/test",
				"--log-error-output-paths", "/etc/k8s/testerror",
			)
			Expect(err).To(BeNil())
			expectOptionsMatch(opts, test.Options(test.OptionsFields{
				ServiceName:             lo.ToPtr("cli"),
				MetricsPort:             lo.ToPtr(0),
				HealthProbePort:         lo.ToPtr(0),
				KubeClientQPS:           lo.ToPtr(0),
				KubeClientBurst:         lo.ToPtr(0),
				EnableProfiling:         lo.ToPtr(true),
				DisableLeaderElection:   lo.ToPtr(true),
				LeaderElectionName:      lo.ToPtr("karpenter-leader-election"),
				LeaderElectionNamespace: lo.ToPtr(""),
				MemoryLimit:             lo.ToPtr[int64](0),
				LogLevel:                lo.ToPtr("debug"),
				LogOutputPaths:          lo.ToPtr("/etc/k8s/test"),
				LogErrorOutputPaths:     lo.ToPtr("/etc/k8s/testerror"),
				BatchMaxDuration:        lo.ToPtr(5 * time.Second),
				BatchIdleDuration:       lo.ToPtr(5 * time.Second),
				FeatureGates: test.FeatureGates{
					SpotToSpotConsolidation: lo.ToPtr(true),
				},
			}))
		})
	})

	DescribeTable(
		"should correctly parse boolean values",
		func(arg string, expected bool) {
			err := opts.Parse(fs, arg)
			Expect(err).ToNot(HaveOccurred())
		},
		Entry("implicit false", "", false),
	)

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
	Expect(optsA.MetricsPort).To(Equal(optsB.MetricsPort))
	Expect(optsA.HealthProbePort).To(Equal(optsB.HealthProbePort))
	Expect(optsA.KubeClientQPS).To(Equal(optsB.KubeClientQPS))
	Expect(optsA.KubeClientBurst).To(Equal(optsB.KubeClientBurst))
	Expect(optsA.EnableProfiling).To(Equal(optsB.EnableProfiling))
	Expect(optsA.DisableLeaderElection).To(Equal(optsB.DisableLeaderElection))
	Expect(optsA.MemoryLimit).To(Equal(optsB.MemoryLimit))
	Expect(optsA.LogLevel).To(Equal(optsB.LogLevel))
	Expect(optsA.LogOutputPaths).To(Equal(optsB.LogOutputPaths))
	Expect(optsA.LogErrorOutputPaths).To(Equal(optsB.LogErrorOutputPaths))
	Expect(optsA.BatchMaxDuration).To(Equal(optsB.BatchMaxDuration))
	Expect(optsA.BatchIdleDuration).To(Equal(optsB.BatchIdleDuration))
	Expect(optsA.FeatureGates.SpotToSpotConsolidation).To(Equal(optsB.FeatureGates.SpotToSpotConsolidation))
}
