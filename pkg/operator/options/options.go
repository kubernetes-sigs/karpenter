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
	"errors"
	"flag"
	"os"
	"runtime/debug"

	"github.com/aws/karpenter-core/pkg/utils/env"
)

type AWSNodeNameConvention string

const (
	IPName       AWSNodeNameConvention = "ip-name"
	ResourceName AWSNodeNameConvention = "resource-name"
)

// Options for running this binary
type Options struct {
	*flag.FlagSet
	// Vendor Neutral
	ServiceName          string
	WebhookPort          int
	MetricsPort          int
	HealthProbePort      int
	KubeClientQPS        int
	KubeClientBurst      int
	EnableProfiling      bool
	EnableLeaderElection bool
	MemoryLimit          int64
}

// New creates an Options struct and registers CLI flags and environment variables to fill-in the Options struct fields
func New() *Options {
	opts := &Options{}
	f := flag.NewFlagSet("karpenter", flag.ContinueOnError)
	opts.FlagSet = f

	// Vendor Neutral
	f.StringVar(&opts.ServiceName, "karpenter-service", env.WithDefaultString("KARPENTER_SERVICE", ""), "The Karpenter Service name for the dynamic webhook certificate")
	f.IntVar(&opts.WebhookPort, "webhook-port", env.WithDefaultInt("PORT", 8443), "The port the webhook endpoint binds to for validation and mutation of resources")
	f.IntVar(&opts.MetricsPort, "metrics-port", env.WithDefaultInt("METRICS_PORT", 8080), "The port the metric endpoint binds to for operating metrics about the controller itself")
	f.IntVar(&opts.HealthProbePort, "health-probe-port", env.WithDefaultInt("HEALTH_PROBE_PORT", 8081), "The port the health probe endpoint binds to for reporting controller health")
	f.IntVar(&opts.KubeClientQPS, "kube-client-qps", env.WithDefaultInt("KUBE_CLIENT_QPS", 200), "The smoothed rate of qps to kube-apiserver")
	f.IntVar(&opts.KubeClientBurst, "kube-client-burst", env.WithDefaultInt("KUBE_CLIENT_BURST", 300), "The maximum allowed burst of queries to the kube-apiserver")
	f.BoolVar(&opts.EnableProfiling, "enable-profiling", env.WithDefaultBool("ENABLE_PROFILING", false), "Enable the profiling on the metric endpoint")
	f.BoolVar(&opts.EnableLeaderElection, "leader-elect", env.WithDefaultBool("LEADER_ELECT", true), "Start leader election client and gain leadership before executing the main loop. Enable this when running replicated components for high availability.")
	f.Int64Var(&opts.MemoryLimit, "memory-limit", env.WithDefaultInt64("MEMORY_LIMIT", -1), "Memory limit on the container running the controller. The GC soft memory limit is set to 90% of this value.")

	if opts.MemoryLimit > 0 {
		newLimit := int64(float64(opts.MemoryLimit) * 0.9)
		debug.SetMemoryLimit(newLimit)
	}
	return opts
}

// MustParse reads the user passed flags, environment variables, and default values.
// Options are valided and panics if an error is returned
func (o *Options) MustParse() *Options {
	err := o.Parse(os.Args[1:])

	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		panic(err)
	}
	return o
}
