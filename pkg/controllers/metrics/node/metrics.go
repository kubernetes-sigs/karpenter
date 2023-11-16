package node

import (
	"strings"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
)

const (
	ResourceType = "resource_type"
	NodePhase    = "phase"
)

func GetWellKnownLabels() map[string]string {
	labels := make(map[string]string)
	// TODO @joinnis: Remove v1alpha5 well-known labels in favor of only v1beta1 well-known labels after v1alpha5 is dropped
	for wellKnownLabel := range v1alpha5.WellKnownLabels.Union(v1beta1.WellKnownLabels) {
		if parts := strings.Split(wellKnownLabel, "/"); len(parts) == 2 {
			label := parts[1]
			// Reformat label names to be consistent with Prometheus naming conventions (snake_case)
			label = strings.ReplaceAll(strings.ToLower(label), "-", "_")
			labels[wellKnownLabel] = label
		}
	}
	return labels
}

func GetNodeLabels(node *v1.Node, resourceTypeName string) prometheus.Labels {
	metricLabels := prometheus.Labels{}
	if resourceTypeName != "" {
		metricLabels[ResourceType] = resourceTypeName
	}
	metricLabels[metrics.NodeName] = node.Name
	metricLabels[metrics.ProvisionerLabel] = node.Labels[v1alpha5.ProvisionerNameLabelKey]
	if node.Labels[v1beta1.NodePoolLabelKey] != "" {
		metricLabels[metrics.NodePoolLabel] = node.Labels[v1beta1.NodePoolLabelKey]
	}
	metricLabels[NodePhase] = string(node.Status.Phase)

	// Populate well known labels
	for wellKnownLabel, label := range wellKnownLabels {
		metricLabels[label] = node.Labels[wellKnownLabel]
	}
	return metricLabels
}
