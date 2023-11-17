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

package termination

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/aws/karpenter-core/pkg/metrics"

	nodemetrics "github.com/aws/karpenter-core/pkg/controllers/metrics/node"
)

var (
	TerminationSummary = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace:  "karpenter",
			Subsystem:  "nodes",
			Name:       "termination_time_seconds",
			Help:       "The time taken between a node's deletion request and the removal of its finalizer",
			Objectives: metrics.SummaryObjectives(),
		},
		[]string{metrics.ProvisionerLabel, metrics.NodePoolLabel},
	)
	NodeDrainTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "drain_time_seconds",
			Help:      "The time taken to drain a node.",
		},
		nodeLabelNames(),
	)
)

func nodeLabelNames() []string {
	return append(
		sets.New(lo.Values(nodemetrics.GetWellKnownLabels())...).UnsortedList(),
		metrics.NodeName,
		metrics.ProvisionerLabel,
		nodemetrics.NodePhase,
	)
}

func init() {
	crmetrics.Registry.MustRegister(TerminationSummary)
	crmetrics.Registry.MustRegister(NodeDrainTime)
}
