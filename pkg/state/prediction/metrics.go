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

package prediction

import (
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	predictionStoreSubsystem = "prediction_store"
)

var (
	// StoreHydrated is written only by Store.Hydrated while it waits and by Store.MarkHydrated, so the series is absent
	// until something waits on or completes hydration. Only leader-elected controllers with the PredictionEnabled feature
	// gate on do either, so standby replicas and processes with the gate off export no series. Alarm on 0 lasting longer
	// than HydrationTimeout. It's process-wide and unlabeled because the operator creates a single Store. If a process
	// creates several stores, as tests do, it reflects whichever store was last waited on or first hydrated.
	StoreHydrated = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: predictionStoreSubsystem,
			Name:      "hydrated",
			Help:      "Returns 1 once a prediction source has hydrated the prediction store and 0 while provisioning or disruption waits for it to hydrate. The series is absent until something waits on or completes hydration, so it's absent on standby replicas and with the PredictionEnabled feature gate off. Alarm on 0 lasting longer than the 1m hydration timeout.",
		},
		[]opmetrics.Label{},
		opmetrics.Alpha,
	)
)
