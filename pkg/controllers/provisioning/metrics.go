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

package provisioning

import (
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	capacityBufferSubsystem = "capacitybuffers"

	// namespaceLabelName is the CapacityBuffer's namespace. It is co-located
	// here because the pod metric's namespace dimension describes a pod, not a buffer.
	namespaceLabelName = "namespace"
	// capacityBufferLabelName is the CapacityBuffer object's name. The published
	// metric contract uses this label name rather than a snake-cased form.
	capacityBufferLabelName = "capacitybuffer"
	// nodeNameLabelName matches the node metric dimension. That Label is
	// unexported in the node metrics package, so it is declared again here.
	nodeNameLabelName = "node_name"
)

var (
	namespaceLabel = opmetrics.Label{
		Name: namespaceLabelName,
		Help: "The namespace of the CapacityBuffer.",
	}
	capacityBufferLabel = opmetrics.Label{
		Name: capacityBufferLabelName,
		Help: "The name of the CapacityBuffer.",
	}
	nodeNameLabel = opmetrics.Label{
		Name: nodeNameLabelName,
		Help: "The name of the node the virtual pods were assigned to.",
	}
)

// CapacityBufferVirtualPods is the number of CapacityBuffer virtual pods the
// latest completed provisioning simulation assigned to an existing node.
// Each simulation replaces the whole snapshot, so series for nodes or buffers
// that no longer have virtual pods are dropped.
var CapacityBufferVirtualPods = opmetrics.NewPrometheusGauge(
	crmetrics.Registry,
	prometheus.GaugeOpts{
		Namespace: metrics.Namespace,
		Subsystem: capacityBufferSubsystem,
		Name:      "virtual_pods",
		Help:      "Number of CapacityBuffer virtual pods assigned to an existing node by the latest completed provisioning simulation. Series are replaced on each simulation, so nodes and buffers that no longer have virtual pods are dropped.",
	},
	[]opmetrics.Label{
		namespaceLabel,
		capacityBufferLabel,
		nodeNameLabel,
	},
	opmetrics.Alpha,
)

// capacityBufferVirtualPodStore is the snapshot of virtual-pod placement series.
// ReplaceAll drops every series absent from the latest completed simulation,
// including when that snapshot is empty.
var capacityBufferVirtualPodStore = metrics.NewStore()

type virtualPodPlacement struct {
	namespace string
	buffer    string
	nodeName  string
}

// recordCapacityBufferVirtualPods replaces the virtual-pod placement gauge with
// the counts from one completed provisioning simulation. Only virtual pods
// assigned to existing nodes are counted; pods that need a new NodeClaim or
// failed to schedule are not on a node yet. An empty result drops every series.
func recordCapacityBufferVirtualPods(results scheduler.Results) {
	counts := map[virtualPodPlacement]int{}
	for _, existing := range results.ExistingNodes {
		nodeName := existingNodeName(existing)
		if nodeName == "" {
			continue
		}
		for _, pod := range existing.Pods {
			namespace, name, ok := virtualPodBuffer(pod)
			if !ok {
				continue
			}
			counts[virtualPodPlacement{namespace: namespace, buffer: name, nodeName: nodeName}]++
		}
	}

	snapshot := make(map[string][]*metrics.StoreMetric, len(counts))
	for placement, count := range counts {
		// One key per series so ReplaceAll deletes a node or buffer that is
		// absent from this simulation instead of leaving its series in place.
		key := placement.namespace + "/" + placement.buffer + "/" + placement.nodeName
		snapshot[key] = []*metrics.StoreMetric{{
			GaugeMetric: CapacityBufferVirtualPods,
			Value:       float64(count),
			Labels: prometheus.Labels{
				namespaceLabel.Name:      placement.namespace,
				capacityBufferLabel.Name: placement.buffer,
				nodeNameLabel.Name:       placement.nodeName,
			},
		}}
	}
	capacityBufferVirtualPodStore.ReplaceAll(snapshot)
}

// existingNodeName is the node name to publish for an existing-node placement.
// A node that has not registered yet is identified by its NodeClaim name, which
// is what StateNode.Name reports in that case. Nodes with no name are skipped
// so the gauge never grows a blank series.
func existingNodeName(n *scheduler.ExistingNode) string {
	if n == nil || n.StateNode == nil {
		return ""
	}
	if n.Node == nil && n.NodeClaim == nil {
		return ""
	}
	return n.Name()
}
