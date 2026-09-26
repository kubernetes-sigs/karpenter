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

package reboot

import (
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

const resultLabel = "result"

// Terminal reboot results. Bounded set; reboot metrics are deliberately fault-agnostic (the executor
// does not interpret the driving fault), so no condition/reason label is emitted.
const (
	resultSucceeded       = "succeeded"
	resultProviderError   = "provider_error"
	resultRecoveryTimeout = "recovery_timeout"
	resultInvalidRequest  = "invalid_request"
)

// Result is the terminal-outcome dimension shared by every reboot metric.
var Result = opmetrics.Label{
	Name: resultLabel,
	Help: "The terminal outcome of the reboot.",
	Values: []opmetrics.Value{
		{Name: resultSucceeded, Help: "The node rebooted and rejoined the cluster (bootID changed + Ready)."},
		{Name: resultProviderError, Help: "The cloud provider rejected the reboot with a terminal error."},
		{Name: resultRecoveryTimeout, Help: "The node did not prove a new boot and rejoin within the observation window."},
		{Name: resultInvalidRequest, Help: "The committed reboot request was invalid (missing, malformed, or negative drain grace period)."},
	},
}

// rebootDurationBuckets span a few seconds to past the observation window (~21m), covering fast VMs
// through slow bare-metal reboots.
var rebootDurationBuckets = prometheus.ExponentialBuckets(10, 2, 8) // 10,20,40,80,160,320,640,1280s

var (
	RebootsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "reboots_total",
			Help:      "Number of node reboots carried out by Karpenter, labeled by terminal result (succeeded, provider_error, recovery_timeout).",
		},
		[]opmetrics.Label{Result},
		opmetrics.Beta,
	)
	// RebootDurationSeconds measures the whole reboot action: RebootRequested through the terminal
	// outcome (includes fence + drain + issue + observe).
	RebootDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "reboot_duration_seconds",
			Help:      "Duration of the full reboot action from request to terminal outcome, labeled by result.",
			Buckets:   rebootDurationBuckets,
		},
		[]opmetrics.Label{Result},
		opmetrics.Beta,
	)
	// RebootRecoveryDurationSeconds measures pure reboot-to-recovery: issuance to a new boot rejoining
	// (bootID changed + Ready). Drain-independent; recorded only on success. This is the signal used to
	// size the observation window.
	RebootRecoveryDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "reboot_recovery_duration_seconds",
			Help:      "Time from issuing a reboot until the node proved a new boot and rejoined (bootID changed + Ready). Recorded on successful reboots only.",
			Buckets:   rebootDurationBuckets,
		},
		[]opmetrics.Label{},
		opmetrics.Beta,
	)
)
