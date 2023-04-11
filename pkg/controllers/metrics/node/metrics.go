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

package node

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/metrics"
)

const (
	resourceType    = "resource_type"
	nodeName        = "node_name"
	nodeProvisioner = "provisioner"
	nodePhase       = "phase"
)

var (
	wellKnownLabelPromMapping = lo.SliceToMap(v1alpha5.WellKnownLabels.List(), func(l string) (string, string) {
		return l, getPromLabel(l)
	})
	nodeLabels = append(
		lo.Values(wellKnownLabelPromMapping),
		resourceType,
		nodeName,
		nodeProvisioner,
		nodePhase,
	)
)

// getPromLabel translates a standard Kubernetes label to a prometheus-compliant label
func getPromLabel(l string) string {
	l = strings.ReplaceAll(strings.ToLower(l), "-", "_")
	if res := strings.Split(l, "/"); len(res) == 2 {
		return res[1]
	}
	return l
}

// getNodeLabels gets all the prometheus metric labels from the node labels
func getNodeLabels(node *v1.Node, resourceTypeName string) prometheus.Labels {
	metricLabels := prometheus.Labels{}
	metricLabels[resourceType] = resourceTypeName
	metricLabels[nodeName] = node.GetName()
	metricLabels[nodeProvisioner] = node.Labels[v1alpha5.ProvisionerNameLabelKey]
	metricLabels[nodePhase] = string(node.Status.Phase)
	for label, promLabel := range wellKnownLabelPromMapping {
		metricLabels[promLabel] = node.Labels[label]
	}
	return metricLabels
}

var (
	allocatableGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "allocatable",
			Help:      "Node allocatable are the resources allocatable by nodes.",
		},
		nodeLabels,
	)
	podRequestsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "total_pod_requests",
			Help:      "Node total pod requests are the resources requested by non-DaemonSet pods bound to nodes.",
		},
		nodeLabels,
	)
	podLimitsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "total_pod_limits",
			Help:      "Node total pod limits are the resources specified by non-DaemonSet pod limits.",
		},
		nodeLabels,
	)
	daemonRequestsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "total_daemon_requests",
			Help:      "Node total daemon requests are the resource requested by DaemonSet pods bound to nodes.",
		},
		nodeLabels,
	)
	daemonLimitsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "total_daemon_limits",
			Help:      "Node total daemon limits are the resources specified by DaemonSet pod limits.",
		},
		nodeLabels,
	)
	overheadGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "system_overhead",
			Help:      "Node system daemon overhead are the resources reserved for system overhead, the difference between the node's capacity and allocatable values are reported by the status.",
		},
		nodeLabels,
	)
	gauges = []*prometheus.GaugeVec{allocatableGauge, podRequestsGauge, podLimitsGauge, daemonRequestsGauge, daemonLimitsGauge, overheadGauge}
)

func init() {
	crmetrics.Registry.MustRegister(lo.Map(gauges, func(g *prometheus.GaugeVec, _ int) prometheus.Collector { return g })...)
}
