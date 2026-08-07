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
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

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
		"DISABLE_CONTROLLER_WARMUP",
		"DISABLE_LEADER_ELECTION",
		"DISABLE_CLUSTER_STATE_OBSERVABILITY",
		"LEADER_ELECTION_NAMESPACE",
		"MEMORY_LIMIT",
		"LOG_LEVEL",
		"LOG_OUTPUT_PATHS",
		"LOG_ERROR_OUTPUT_PATHS",
		"BATCH_MAX_DURATION",
		"BATCH_IDLE_DURATION",
		"PREFERENCE_POLICY",
		"MIN_VALUES_POLICY",
		"FEATURE_GATES",
		"SCHEDULER_CONFIG",
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
				ServiceName:                      new(""),
				MetricsPort:                      new(8080),
				HealthProbePort:                  new(8081),
				KubeClientQPS:                    new(200),
				KubeClientBurst:                  new(300),
				EnableProfiling:                  new(false),
				DisableControllerWarmup:          new(true),
				DisableLeaderElection:            new(false),
				DisableClusterStateObservability: new(false),
				LeaderElectionName:               new("karpenter-leader-election"),
				LeaderElectionNamespace:          new(""),
				MemoryLimit:                      lo.ToPtr[int64](-1),
				LogLevel:                         new("info"),
				LogOutputPaths:                   new("stdout"),
				LogErrorOutputPaths:              new("stderr"),
				BatchMaxDuration:                 lo.ToPtr(10 * time.Second),
				BatchIdleDuration:                lo.ToPtr(time.Second),
				PreferencePolicy:                 lo.ToPtr(options.PreferencePolicyRespect),
				MinValuesPolicy:                  lo.ToPtr(options.MinValuesPolicyStrict),
				FeatureGates: test.FeatureGates{
					ReservedCapacity:        new(true),
					NodeRepair:              new(false),
					SpotToSpotConsolidation: new(false),
					NodeOverlay:             new(false),
					StaticCapacity:          new(false),
					CapacityBuffer:          new(false),
				},
				IgnoreDRARequests: new(true),
				SchedulerConfig:   nil,
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
				"--disable-controller-warmup=false",
				"--disable-leader-election=true",
				"--disable-cluster-state-observability=true",
				"--leader-election-name=karpenter-controller",
				"--leader-election-namespace=karpenter",
				"--memory-limit", "0",
				"--log-level", "debug",
				"--log-output-paths", "/etc/k8s/test",
				"--log-error-output-paths", "/etc/k8s/testerror",
				"--batch-max-duration", "5s",
				"--batch-idle-duration", "5s",
				"--preference-policy", "Ignore",
				"--min-values-policy", "BestEffort",
				"--feature-gates", "ReservedCapacity=false,SpotToSpotConsolidation=true,NodeRepair=true,NodeOverlay=true,StaticCapacity=true,CapacityBuffer=true",
				"--scheduler-config", `{"podTopologySpread":{"defaultConstraints":[{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway"}]}}`,
			)
			Expect(err).To(BeNil())
			expectOptionsMatch(opts, test.Options(test.OptionsFields{
				ServiceName:                      new("cli"),
				MetricsPort:                      new(0),
				HealthProbePort:                  new(0),
				KubeClientQPS:                    new(0),
				KubeClientBurst:                  new(0),
				EnableProfiling:                  new(true),
				DisableControllerWarmup:          new(false),
				DisableLeaderElection:            new(true),
				DisableClusterStateObservability: new(true),
				LeaderElectionName:               new("karpenter-controller"),
				LeaderElectionNamespace:          new("karpenter"),
				MemoryLimit:                      lo.ToPtr[int64](0),
				LogLevel:                         new("debug"),
				LogOutputPaths:                   new("/etc/k8s/test"),
				LogErrorOutputPaths:              new("/etc/k8s/testerror"),
				BatchMaxDuration:                 lo.ToPtr(5 * time.Second),
				BatchIdleDuration:                lo.ToPtr(5 * time.Second),
				PreferencePolicy:                 lo.ToPtr(options.PreferencePolicyIgnore),
				MinValuesPolicy:                  lo.ToPtr(options.MinValuesPolicyBestEffort),
				FeatureGates: test.FeatureGates{
					ReservedCapacity:        new(false),
					NodeRepair:              new(true),
					SpotToSpotConsolidation: new(true),
					NodeOverlay:             new(true),
					StaticCapacity:          new(true),
					CapacityBuffer:          new(true),
				},
				IgnoreDRARequests: new(true),
				SchedulerConfig: &options.SchedulerConfiguration{
					PodTopologySpread: &options.PodTopologySpreadConfig{
						DefaultConstraints: []corev1.TopologySpreadConstraint{{
							MaxSkew:           1,
							TopologyKey:       "topology.kubernetes.io/zone",
							WhenUnsatisfiable: corev1.ScheduleAnyway,
						}},
					},
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
			os.Setenv("DISABLE_CONTROLLER_WARMUP", "false")
			os.Setenv("DISABLE_LEADER_ELECTION", "true")
			os.Setenv("DISABLE_CLUSTER_STATE_OBSERVABILITY", "true")
			os.Setenv("LEADER_ELECTION_NAME", "karpenter-controller")
			os.Setenv("LEADER_ELECTION_NAMESPACE", "karpenter")
			os.Setenv("MEMORY_LIMIT", "0")
			os.Setenv("LOG_LEVEL", "debug")
			os.Setenv("LOG_OUTPUT_PATHS", "/etc/k8s/test")
			os.Setenv("LOG_ERROR_OUTPUT_PATHS", "/etc/k8s/testerror")
			os.Setenv("BATCH_MAX_DURATION", "5s")
			os.Setenv("BATCH_IDLE_DURATION", "5s")
			os.Setenv("PREFERENCE_POLICY", "Ignore")
			os.Setenv("MIN_VALUES_POLICY", "BestEffort")
			os.Setenv("FEATURE_GATES", "ReservedCapacity=false,SpotToSpotConsolidation=true,NodeRepair=true,NodeOverlay=true,StaticCapacity=true,CapacityBuffer=true")
			os.Setenv("SCHEDULER_CONFIG", `{"podTopologySpread":{"defaultConstraints":[{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway"}]}}`)
			fs = &options.FlagSet{
				FlagSet: flag.NewFlagSet("karpenter", flag.ContinueOnError),
			}
			opts.AddFlags(fs)
			err := opts.Parse(fs)
			Expect(err).To(BeNil())
			expectOptionsMatch(opts, test.Options(test.OptionsFields{
				ServiceName:                      new("env"),
				MetricsPort:                      new(0),
				HealthProbePort:                  new(0),
				KubeClientQPS:                    new(0),
				KubeClientBurst:                  new(0),
				EnableProfiling:                  new(true),
				DisableControllerWarmup:          new(false),
				DisableLeaderElection:            new(true),
				DisableClusterStateObservability: new(true),
				LeaderElectionName:               new("karpenter-controller"),
				LeaderElectionNamespace:          new("karpenter"),
				MemoryLimit:                      lo.ToPtr[int64](0),
				LogLevel:                         new("debug"),
				LogOutputPaths:                   new("/etc/k8s/test"),
				LogErrorOutputPaths:              new("/etc/k8s/testerror"),
				BatchMaxDuration:                 lo.ToPtr(5 * time.Second),
				BatchIdleDuration:                lo.ToPtr(5 * time.Second),
				PreferencePolicy:                 lo.ToPtr(options.PreferencePolicyIgnore),
				MinValuesPolicy:                  lo.ToPtr(options.MinValuesPolicyBestEffort),
				FeatureGates: test.FeatureGates{
					ReservedCapacity:        new(false),
					NodeRepair:              new(true),
					SpotToSpotConsolidation: new(true),
					NodeOverlay:             new(true),
					StaticCapacity:          new(true),
					CapacityBuffer:          new(true),
				},
				IgnoreDRARequests: new(true),
				SchedulerConfig: &options.SchedulerConfiguration{
					PodTopologySpread: &options.PodTopologySpreadConfig{
						DefaultConstraints: []corev1.TopologySpreadConstraint{{
							MaxSkew:           1,
							TopologyKey:       "topology.kubernetes.io/zone",
							WhenUnsatisfiable: corev1.ScheduleAnyway,
						}},
					},
				},
			}))
		})

		It("should correctly merge CLI flags and environment variables", func() {
			os.Setenv("METRICS_PORT", "0")
			os.Setenv("HEALTH_PROBE_PORT", "0")
			os.Setenv("KUBE_CLIENT_QPS", "0")
			os.Setenv("KUBE_CLIENT_BURST", "0")
			os.Setenv("ENABLE_PROFILING", "true")
			os.Setenv("DISABLE_CONTROLLER_WARMUP", "false")
			os.Setenv("DISABLE_LEADER_ELECTION", "true")
			os.Setenv("DISABLE_CLUSTER_STATE_OBSERVABILITY", "true")
			os.Setenv("MEMORY_LIMIT", "0")
			os.Setenv("LOG_LEVEL", "debug")
			os.Setenv("BATCH_MAX_DURATION", "5s")
			os.Setenv("BATCH_IDLE_DURATION", "5s")
			os.Setenv("PREFERENCE_POLICY", "Ignore")
			os.Setenv("MIN_VALUES_POLICY", "BestEffort")
			os.Setenv("FEATURE_GATES", "ReservedCapacity=false,SpotToSpotConsolidation=true,NodeRepair=true,NodeOverlay=true,StaticCapacity=true,CapacityBuffer=true")
			fs = &options.FlagSet{
				FlagSet: flag.NewFlagSet("karpenter", flag.ContinueOnError),
			}
			opts.AddFlags(fs)
			err := opts.Parse(
				fs,
				"--karpenter-service", "cli",
				"--log-output-paths", "/etc/k8s/test",
				"--log-error-output-paths", "/etc/k8s/testerror",
				"--preference-policy", "Respect",
				"--min-values-policy", "Strict",
			)
			Expect(err).To(BeNil())
			expectOptionsMatch(opts, test.Options(test.OptionsFields{
				ServiceName:                      new("cli"),
				MetricsPort:                      new(0),
				HealthProbePort:                  new(0),
				KubeClientQPS:                    new(0),
				KubeClientBurst:                  new(0),
				EnableProfiling:                  new(true),
				DisableControllerWarmup:          new(false),
				DisableLeaderElection:            new(true),
				DisableClusterStateObservability: new(true),
				LeaderElectionName:               new("karpenter-leader-election"),
				LeaderElectionNamespace:          new(""),
				MemoryLimit:                      lo.ToPtr[int64](0),
				LogLevel:                         new("debug"),
				LogOutputPaths:                   new("/etc/k8s/test"),
				LogErrorOutputPaths:              new("/etc/k8s/testerror"),
				BatchMaxDuration:                 lo.ToPtr(5 * time.Second),
				BatchIdleDuration:                lo.ToPtr(5 * time.Second),
				PreferencePolicy:                 lo.ToPtr(options.PreferencePolicyRespect),
				MinValuesPolicy:                  lo.ToPtr(options.MinValuesPolicyStrict),
				FeatureGates: test.FeatureGates{
					ReservedCapacity:        new(false),
					NodeRepair:              new(true),
					SpotToSpotConsolidation: new(true),
					NodeOverlay:             new(true),
					StaticCapacity:          new(true),
					CapacityBuffer:          new(true),
				},
				IgnoreDRARequests: new(true),
			}))
		})

		DescribeTable(
			"should correctly set defaults when a subset of FeatureGates are specified",
			func(gate string) {
				expected, args := func() (options.FeatureGates, []string) {
					expected := new(options.DefaultFeatureGates())

					// Use reflection to find the field for the gate and flip the value
					gateField := reflect.ValueOf(expected).Elem().FieldByName(gate)
					Expect(gateField.IsValid()).To(BeTrue())
					Expect(gateField.Kind()).To(Equal(reflect.Bool))
					expectedGateVal := !gateField.Bool()
					gateField.SetBool(expectedGateVal)

					return *expected, []string{"--feature-gates", fmt.Sprintf("%s=%t", gate, expectedGateVal)}
				}()

				fs = &options.FlagSet{
					FlagSet: flag.NewFlagSet("karpenter", flag.ContinueOnError),
				}
				opts.AddFlags(fs)
				Expect(opts.Parse(fs, args...)).To(Succeed())
				Expect(opts.FeatureGates).To(Equal(expected))
			},
			Entry("when ReservedCapacity is overridden", "ReservedCapacity"),
			Entry("when NodeRepair is overridden", "NodeRepair"),
			Entry("when SpotToSpotConsolidation is overridden", "SpotToSpotConsolidation"),
			Entry("when NodeOverlay is overridden", "NodeOverlay"),
			Entry("when StaticCapacity is overridden", "StaticCapacity"),
			Entry("when CapacityBuffer is overridden", "CapacityBuffer"),
		)
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
		DescribeTable(
			"should fallback to the default if a non-positive value is provided for CPU_REQUESTS",
			func(value string) {
				Expect(opts.Parse(fs, "--cpu-requests", value)).To(Succeed())
				Expect(opts.CPURequests).To(BeNumerically("==", 1000))
			},
			Entry("zero is provided", "0"),
			Entry("negative value is provided", "-50"),
		)
	})

})

var _ = Describe("SchedulerConfiguration", func() {
	Context("ParseSchedulerConfiguration", func() {
		It("should return a nil configuration for an empty value", func() {
			cfg, err := options.ParseSchedulerConfiguration("")
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg).To(BeNil())
		})
		It("should parse a valid podTopologySpread.defaultConstraints document", func() {
			cfg, err := options.ParseSchedulerConfiguration(`
podTopologySpread:
  defaultConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
    - maxSkew: 3
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: DoNotSchedule
`)
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg).ToNot(BeNil())
			Expect(cfg.PodTopologySpread).ToNot(BeNil())
			Expect(cfg.PodTopologySpread.DefaultConstraints).To(HaveLen(2))
			Expect(cfg.PodTopologySpread.DefaultConstraints[0].MaxSkew).To(BeEquivalentTo(1))
			Expect(cfg.PodTopologySpread.DefaultConstraints[0].TopologyKey).To(Equal("topology.kubernetes.io/zone"))
			Expect(cfg.PodTopologySpread.DefaultConstraints[0].WhenUnsatisfiable).To(Equal(corev1.ScheduleAnyway))
			Expect(cfg.PodTopologySpread.DefaultConstraints[1].WhenUnsatisfiable).To(Equal(corev1.DoNotSchedule))
		})
		It("should parse an equivalent JSON document", func() {
			cfg, err := options.ParseSchedulerConfiguration(`{"podTopologySpread":{"defaultConstraints":[{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway"}]}}`)
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.PodTopologySpread.DefaultConstraints).To(HaveLen(1))
		})
		It("should fail fast on an unknown field", func() {
			_, err := options.ParseSchedulerConfiguration(`
podTopologySpread:
  defaultConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
notARealField: true
`)
			Expect(err).To(HaveOccurred())
		})
		It("should fail on malformed YAML", func() {
			_, err := options.ParseSchedulerConfiguration(`podTopologySpread: {`)
			Expect(err).To(HaveOccurred())
		})
		It("should reject a non-positive maxSkew", func() {
			_, err := options.ParseSchedulerConfiguration(`
podTopologySpread:
  defaultConstraints:
    - maxSkew: 0
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
`)
			Expect(err).To(HaveOccurred())
		})
		It("should reject a missing topologyKey", func() {
			_, err := options.ParseSchedulerConfiguration(`
podTopologySpread:
  defaultConstraints:
    - maxSkew: 1
      whenUnsatisfiable: ScheduleAnyway
`)
			Expect(err).To(HaveOccurred())
		})
		It("should reject a labelSelector", func() {
			// Upstream forbids this because selectors are deduced per pod. Accepting one would silently diverge: a
			// static selector matches an unrelated set of pods in every other workload.
			_, err := options.ParseSchedulerConfiguration(`
podTopologySpread:
  defaultConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
      labelSelector:
        matchLabels:
          app: test
`)
			Expect(err).To(HaveOccurred())
		})
		It("should reject matchLabelKeys", func() {
			// matchLabelKeys is inert upstream, since the deduced selector overwrites whatever it merged in.
			_, err := options.ParseSchedulerConfiguration(`
podTopologySpread:
  defaultConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
      matchLabelKeys:
        - pod-template-hash
`)
			Expect(err).To(HaveOccurred())
		})
		It("should reject a topologyKey that isn't a valid label name", func() {
			_, err := options.ParseSchedulerConfiguration(`
podTopologySpread:
  defaultConstraints:
    - maxSkew: 1
      topologyKey: "not a valid label name"
      whenUnsatisfiable: ScheduleAnyway
`)
			Expect(err).To(HaveOccurred())
		})
		It("should reject a duplicated topologyKey and whenUnsatisfiable pair", func() {
			_, err := options.ParseSchedulerConfiguration(`
podTopologySpread:
  defaultConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
    - maxSkew: 3
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
`)
			Expect(err).To(HaveOccurred())
		})
		It("should allow the same topologyKey with a different whenUnsatisfiable", func() {
			cfg, err := options.ParseSchedulerConfiguration(`
podTopologySpread:
  defaultConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
    - maxSkew: 3
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: DoNotSchedule
`)
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.PodTopologySpread.DefaultConstraints).To(HaveLen(2))
		})
		It("should reject an invalid whenUnsatisfiable", func() {
			_, err := options.ParseSchedulerConfiguration(`
podTopologySpread:
  defaultConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: Sometimes
`)
			Expect(err).To(HaveOccurred())
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
	Expect(optsA.DisableControllerWarmup).To(Equal(optsB.DisableControllerWarmup))
	Expect(optsA.DisableLeaderElection).To(Equal(optsB.DisableLeaderElection))
	Expect(optsA.DisableClusterStateObservability).To(Equal(optsB.DisableClusterStateObservability))
	Expect(optsA.MemoryLimit).To(Equal(optsB.MemoryLimit))
	Expect(optsA.LogLevel).To(Equal(optsB.LogLevel))
	Expect(optsA.LogOutputPaths).To(Equal(optsB.LogOutputPaths))
	Expect(optsA.LogErrorOutputPaths).To(Equal(optsB.LogErrorOutputPaths))
	Expect(optsA.BatchMaxDuration).To(Equal(optsB.BatchMaxDuration))
	Expect(optsA.BatchIdleDuration).To(Equal(optsB.BatchIdleDuration))
	Expect(optsA.PreferencePolicy).To(Equal(optsB.PreferencePolicy))
	Expect(optsA.MinValuesPolicy).To(Equal(optsB.MinValuesPolicy))
	Expect(optsA.FeatureGates.ReservedCapacity).To(Equal(optsB.FeatureGates.ReservedCapacity))
	Expect(optsA.FeatureGates.NodeRepair).To(Equal(optsB.FeatureGates.NodeRepair))
	Expect(optsA.FeatureGates.NodeOverlay).To(Equal(optsB.FeatureGates.NodeOverlay))
	Expect(optsA.FeatureGates.StaticCapacity).To(Equal(optsB.FeatureGates.StaticCapacity))
	Expect(optsA.FeatureGates.CapacityBuffer).To(Equal(optsB.FeatureGates.CapacityBuffer))
	Expect(optsA.FeatureGates.SpotToSpotConsolidation).To(Equal(optsB.FeatureGates.SpotToSpotConsolidation))
	Expect(optsA.IgnoreDRARequests).To(Equal(optsB.IgnoreDRARequests))
	Expect(optsA.SchedulerConfig).To(Equal(optsB.SchedulerConfig))
}
