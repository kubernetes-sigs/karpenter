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

package state

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	stateSubsystem = "state"
)

var (
	ClusterStateNodesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: stateSubsystem,
			Name:      "nodes_total",
			Help:      "total nodes in karpenter's cluster state",
		},
		[]string{},
	)

	ClusterStateSynced = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: stateSubsystem,
			Name:      "synced",
			Help:      "Synced validates that nodeclaims and nodes that are stored in the apiserver have the same representation as karpenter's cluster state",
		},
		[]string{},
	)
)

func init() {
	crmetrics.Registry.MustRegister(ClusterStateNodesTotal, ClusterStateSynced)
}