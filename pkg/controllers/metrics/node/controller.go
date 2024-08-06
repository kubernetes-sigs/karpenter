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

package node

import (
	"context"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/singleton"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const (
	resourceType = "resource_type"
	nodeName     = "node_name"
	nodePhase    = "phase"
)

var (
	allocatable = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "allocatable",
			Help:      "Node allocatable are the resources allocatable by nodes.",
		},
		nodeLabelNames(),
	)
	totalPodRequests = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_pod_requests",
			Help:      "Node total pod requests are the resources requested by non-DaemonSet pods bound to nodes.",
		},
		nodeLabelNames(),
	)
	totalPodLimits = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_pod_limits",
			Help:      "Node total pod limits are the resources specified by non-DaemonSet pod limits.",
		},
		nodeLabelNames(),
	)
	totalDaemonRequests = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_daemon_requests",
			Help:      "Node total daemon requests are the resource requested by DaemonSet pods bound to nodes.",
		},
		nodeLabelNames(),
	)
	totalDaemonLimits = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "total_daemon_limits",
			Help:      "Node total daemon limits are the resources specified by DaemonSet pod limits.",
		},
		nodeLabelNames(),
	)
	systemOverhead = prometheus.NewGaugeVec(
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
		// WellKnownLabels includes the nodepool label, so we don't need to add it as its own item here.
		// If we do, prometheus will panic since there would be duplicate labels.
		sets.New(lo.Values(wellKnownLabels)...).UnsortedList(),
		resourceType,
		nodeName,
		nodePhase,
	)
}

func init() {
	crmetrics.Registry.MustRegister(
		allocatable,
		totalPodRequests,
		totalPodLimits,
		totalDaemonRequests,
		totalDaemonLimits,
		systemOverhead,
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

func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "metrics.node") //nolint:ineffassign,staticcheck

	nodes := lo.Reject(c.cluster.Nodes(), func(n *state.StateNode, _ int) bool {
		return n.Node == nil
	})
	c.metricStore.ReplaceAll(lo.SliceToMap(nodes, func(n *state.StateNode) (string, []*metrics.StoreMetric) {
		return client.ObjectKeyFromObject(n.Node).String(), buildMetrics(n)
	}))
	return reconcile.Result{RequeueAfter: time.Second * 5}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("metrics.node").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

func buildMetrics(n *state.StateNode) (res []*metrics.StoreMetric) {
	for gaugeVec, resourceList := range map[*prometheus.GaugeVec]corev1.ResourceList{
		systemOverhead:      resources.Subtract(n.Node.Status.Capacity, n.Node.Status.Allocatable),
		totalPodRequests:    n.PodRequests(),
		totalPodLimits:      n.PodLimits(),
		totalDaemonRequests: n.DaemonSetRequests(),
		totalDaemonLimits:   n.DaemonSetLimits(),
		allocatable:         n.Node.Status.Allocatable,
	} {
		for resourceName, quantity := range resourceList {
			res = append(res, &metrics.StoreMetric{
				GaugeVec: gaugeVec,
				Value:    lo.Ternary(resourceName == corev1.ResourceCPU, float64(quantity.MilliValue())/float64(1000), float64(quantity.Value())),
				Labels:   getNodeLabels(n.Node, strings.ReplaceAll(strings.ToLower(string(resourceName)), "-", "_")),
			})
		}
	}
	return res
}

func getNodeLabels(node *corev1.Node, resourceTypeName string) prometheus.Labels {
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
	for wellKnownLabel := range v1.WellKnownLabels {
		if parts := strings.Split(wellKnownLabel, "/"); len(parts) == 2 {
			label := parts[1]
			// Reformat label names to be consistent with Prometheus naming conventions (snake_case)
			label = strings.ReplaceAll(strings.ToLower(label), "-", "_")
			labels[wellKnownLabel] = label
		}
	}
	return labels
}
