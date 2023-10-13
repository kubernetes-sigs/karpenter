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

package options

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/samber/lo"
	cliflag "k8s.io/component-base/cli/flag"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/utils/env"
)

var validLogLevels = []string{"", "debug", "info", "error"}

type optionsKey struct{}

type FeatureGates struct {
	Drift bool

	inputStr string
}

type OptionFields struct {
	FlagSet *flag.FlagSet
	// Vendor Neutral
	ServiceName          string
	DisableWebhook       bool
	WebhookPort          int
	MetricsPort          int
	WebhookMetricsPort   int
	HealthProbePort      int
	KubeClientQPS        int
	KubeClientBurst      int
	EnableProfiling      bool
	EnableLeaderElection bool
	MemoryLimit          int64
	LogLevel             string
	BatchMaxDuration     time.Duration
	BatchIdleDuration    time.Duration
	FeatureGates         FeatureGates
}

// Note: temporary flags to note if merged fields have been set
type optionFlags struct {
	BatchMaxDurationSet  bool
	BatchIdleDurationSet bool
	FeatureGatesSet      bool
}

type Options struct {
	OptionFields
	optionFlags
}

// New creates an Options struct and registers CLI flags and environment variables to fill-in the Options struct fields
func New() *Options {
	opts := &Options{}
	f := flag.NewFlagSet("karpenter", flag.ContinueOnError)
	opts.FlagSet = f

	// Vendor Neutral
	f.StringVar(&opts.ServiceName, "karpenter-service", env.WithDefaultString("KARPENTER_SERVICE", ""), "The Karpenter Service name for the dynamic webhook certificate")
	f.BoolVar(&opts.DisableWebhook, "disable-webhook", env.WithDefaultBool("DISABLE_WEBHOOK", false), "Disable the admission and validation webhooks")
	f.IntVar(&opts.WebhookPort, "webhook-port", env.WithDefaultInt("WEBHOOK_PORT", 8443), "The port the webhook endpoint binds to for validation and mutation of resources")
	f.IntVar(&opts.MetricsPort, "metrics-port", env.WithDefaultInt("METRICS_PORT", 8000), "The port the metric endpoint binds to for operating metrics about the controller itself")
	f.IntVar(&opts.WebhookMetricsPort, "webhook-metrics-port", env.WithDefaultInt("WEBHOOK_METRICS_PORT", 8001), "The port the webhook metric endpoing binds to for operating metrics about the webhook")
	f.IntVar(&opts.HealthProbePort, "health-probe-port", env.WithDefaultInt("HEALTH_PROBE_PORT", 8081), "The port the health probe endpoint binds to for reporting controller health")
	f.IntVar(&opts.KubeClientQPS, "kube-client-qps", env.WithDefaultInt("KUBE_CLIENT_QPS", 200), "The smoothed rate of qps to kube-apiserver")
	f.IntVar(&opts.KubeClientBurst, "kube-client-burst", env.WithDefaultInt("KUBE_CLIENT_BURST", 300), "The maximum allowed burst of queries to the kube-apiserver")
	f.BoolVar(&opts.EnableProfiling, "enable-profiling", env.WithDefaultBool("ENABLE_PROFILING", false), "Enable the profiling on the metric endpoint")
	f.BoolVar(&opts.EnableLeaderElection, "leader-elect", env.WithDefaultBool("LEADER_ELECT", true), "Start leader election client and gain leadership before executing the main loop. Enable this when running replicated components for high availability.")
	f.Int64Var(&opts.MemoryLimit, "memory-limit", env.WithDefaultInt64("MEMORY_LIMIT", -1), "Memory limit on the container running the controller. The GC soft memory limit is set to 90% of this value.")
	f.StringVar(&opts.LogLevel, "log-level", env.WithDefaultString("LOG_LEVEL", ""), "Log verbosity level. Can be one of 'debug', 'info', or 'error'")

	// Vars that must be merged with settings
	f.DurationVar(&opts.BatchMaxDuration, "batch-max-duration", env.WithDefaultDuration("BATCH_MAX_DURATION", 10*time.Second), "The maximum length of a batch window. The longer this is, the more pods we can consider for provisioning at one time which usually results in fewer but larger nodes.")
	f.DurationVar(&opts.BatchIdleDuration, "batch-idle-duration", env.WithDefaultDuration("BATCH_IDLE_DURATION", time.Second), "The maximum amount of time with no new pending pods that if exceeded ends the current batching window. If pods arrive faster than this time, the batching window will be extended up to the maxDuration. If they arrive slower, the pods will be batched separately.")
	f.StringVar(&opts.FeatureGates.inputStr, "feature-gates", env.WithDefaultString("FEATURE_GATES", "Drift=false"), "Optional features can be enabled / disabled using feature gates. Current options are: Drift")

	return opts
}

func (*Options) Inject(ctx context.Context, args ...string) (context.Context, error) {
	o := New()
	if err := o.FlagSet.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return ctx, err
	}

	if !lo.Contains(validLogLevels, o.LogLevel) {
		return ctx, fmt.Errorf("failed to validate cli flags / env vars, invalid log level %q", o.LogLevel)
	}

	o.FeatureGates = FeatureGatesFromStr(o.FeatureGates.inputStr)

	// Check if shared fields have been set. If they haven't, they may be ovewritten by settings parsed from configmaps.
	o.FlagSet.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "batch-max-duration":
			o.BatchMaxDurationSet = true
		case "batch-idle-duration":
			o.BatchIdleDurationSet = true
		case "feature-gates":
			o.FeatureGatesSet = true
		}
	})
	if _, ok := os.LookupEnv("BATCH_MAX_DURATION"); ok {
		o.BatchMaxDurationSet = true
	}
	if _, ok := os.LookupEnv("BATCH_IDLE_DURATION"); ok {
		o.BatchIdleDurationSet = true
	}
	if _, ok := os.LookupEnv("FEATURE_GATES"); ok {
		o.FeatureGatesSet = true
	}

	ctx = ToContext(ctx, o)
	return ctx, nil
}

// MergeSettings applies settings specified in the v1alpha5 configmap to options. If the value was already specified by
// a CLI argument or environment variable, that value will be used.
func (*Options) MergeSettings(ctx context.Context, injectables ...settings.Injectable) context.Context {
	for _, in := range injectables {
		_, ok := in.(*settings.Settings)
		if !ok {
			continue
		}
		s := in.FromContext(ctx).(*settings.Settings)
		o := FromContext(ctx)
		mergeField(&o.BatchMaxDuration, s.BatchMaxDuration, o.BatchMaxDurationSet)
		mergeField(&o.BatchIdleDuration, s.BatchIdleDuration, o.BatchIdleDurationSet)
		mergeField(&o.FeatureGates.Drift, s.DriftEnabled, o.FeatureGatesSet)
		ctx = ToContext(ctx, o)
	}

	// Note: settings also has default values applied to it. If the option is specified by neither Settings nor Options,
	// the default value is used from Settings.
	return ctx
}

func (*Options) ToContext(ctx context.Context, in Injectable) context.Context {
	opts, ok := in.(*Options)
	if !ok {
		panic("failed to inject options into context, incorrect type")
	}
	return ToContext(ctx, opts)
}

func (*Options) FromContext(ctx context.Context) Injectable {
	return FromContext(ctx)
}

func FeatureGatesFromStr(gateStr string) FeatureGates {
	gateMap := map[string]bool{}
	gates := FeatureGates{}

	// Parses feature gates with the upstream mechanism. This is meant to be used with flag directly but this enables
	// simple merging with environment vars.
	lo.Must0(cliflag.NewMapStringBool(&gateMap).Set(gateStr))
	if val, ok := gateMap["Drift"]; ok {
		gates.Drift = val
	}

	return gates
}

func ToContext(ctx context.Context, opts *Options) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

func FromContext(ctx context.Context) *Options {
	retval := ctx.Value(optionsKey{})
	if retval == nil {
		// This is a developer error if this happens, so we should panic
		panic("options doesn't exist in context")
	}
	return retval.(*Options)
}

func mergeField[T any](dest *T, val T, isSet bool) {
	if isSet {
		return
	}
	*dest = val
}
