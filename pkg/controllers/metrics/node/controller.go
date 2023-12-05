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
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/controller"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const (
	resourceType = "resource_type"
	nodeName     = "node_name"
	nodePhase    = "phase"
)

var (
	allocatableGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "allocatable",
			Help:      "Node allocatable are the resources allocatable by nodes.",
		},
		nodeLabelNames(),
	)
	podRequestsGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_pod_requests",
			Help:      "Node total pod requests are the resources requested by non-DaemonSet pods bound to nodes.",
		},
		nodeLabelNames(),
	)
	podLimitsGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_pod_limits",
			Help:      "Node total pod limits are the resources specified by non-DaemonSet pod limits.",
		},
		nodeLabelNames(),
	)
	daemonRequestsGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_daemon_requests",
			Help:      "Node total daemon requests are the resource requested by DaemonSet pods bound to nodes.",
		},
		nodeLabelNames(),
	)
	daemonLimitsGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_daemon_limits",
			Help:      "Node total daemon limits are the resources specified by DaemonSet pod limits.",
		},
		nodeLabelNames(),
	)
	overheadGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "system_overhead",
			Help:      "Node system daemon overhead are the resources reserved for system overhead, the difference between the node's capacity and allocatable values are reported by the status.",
		},
		nodeLabelNames(),
	)
	wellKnownLabels = getWellKnownLabels()
)

func nodeLabelNames() []string {
	return append(
		sets.New(lo.Values(wellKnownLabels)...).UnsortedList(),
		resourceType,
		nodeName,
		nodePhase,
	)
}

func init() {
	crmetrics.Registry.MustRegister(
		allocatableGaugeVec,
		podRequestsGaugeVec,
		podLimitsGaugeVec,
		daemonRequestsGaugeVec,
		daemonLimitsGaugeVec,
		overheadGaugeVec,
	)
}

type Controller struct {
	cluster     *state.Cluster
	metricStore *metrics.Store
}

func NewController(cluster *state.Cluster) *Controller {
	return &Controller{
		cluster:     cluster,
		metricStore: metrics.NewStore(),
	}
}

func (c *Controller) Name() string {
	return "metrics.node"
}

func (c *Controller) Reconcile(_ context.Context, _ reconcile.Request) (reconcile.Result, error) {
	nodes := lo.Reject(c.cluster.Nodes(), func(n *state.StateNode, _ int) bool {
		return n.Node == nil
	})
	c.metricStore.ReplaceAll(lo.SliceToMap(nodes, func(n *state.StateNode) (string, []*metrics.StoreMetric) {
		return client.ObjectKeyFromObject(n.Node).String(), buildMetrics(n)
	}))
	return reconcile.Result{RequeueAfter: time.Second * 5}, nil
}

func (c *Controller) Builder(_ context.Context, mgr manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(mgr)
}

func buildMetrics(n *state.StateNode) (res []*metrics.StoreMetric) {
	for gaugeVec, resourceList := range map[*prometheus.GaugeVec]v1.ResourceList{
		overheadGaugeVec:       resources.Subtract(n.Node.Status.Capacity, n.Node.Status.Allocatable),
		podRequestsGaugeVec:    resources.Subtract(n.PodRequests(), n.DaemonSetRequests()),
		podLimitsGaugeVec:      resources.Subtract(n.PodLimits(), n.DaemonSetLimits()),
		daemonRequestsGaugeVec: n.DaemonSetRequests(),
		daemonLimitsGaugeVec:   n.DaemonSetLimits(),
		allocatableGaugeVec:    n.Node.Status.Allocatable,
	} {
		for resourceName, quantity := range resourceList {
			res = append(res, &metrics.StoreMetric{
				GaugeVec: gaugeVec,
				Value:    lo.Ternary(resourceName == v1.ResourceCPU, float64(quantity.MilliValue())/float64(1000), float64(quantity.Value())),
				Labels:   getNodeLabels(n.Node, strings.ReplaceAll(strings.ToLower(string(resourceName)), "-", "_")),
			})
		}
	}
	return res
}

func getNodeLabels(node *v1.Node, resourceTypeName string) prometheus.Labels {
	metricLabels := prometheus.Labels{}
	metricLabels[resourceType] = resourceTypeName
	metricLabels[nodeName] = node.Name
	metricLabels[nodePhase] = string(node.Status.Phase)

	// Populate well known labels
	for wellKnownLabel, label := range wellKnownLabels {
		metricLabels[label] = node.Labels[wellKnownLabel]
	}
	return metricLabels
}

func getWellKnownLabels() map[string]string {
	labels := make(map[string]string)
	// TODO @joinnis: Remove v1alpha5 well-known labels in favor of only v1beta1 well-known labels after v1alpha5 is dropped
	for wellKnownLabel := range v1beta1.WellKnownLabels {
		if parts := strings.Split(wellKnownLabel, "/"); len(parts) == 2 {
			label := parts[1]
			// Reformat label names to be consistent with Prometheus naming conventions (snake_case)
			label = strings.ReplaceAll(strings.ToLower(label), "-", "_")
			labels[wellKnownLabel] = label
		}
	}
	return labels
}
