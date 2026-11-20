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
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const (
	nodeName  = "node_name"
	nodePhase = "phase"
	managed   = "managed"
)

// Fixed node metric dimensions. The remaining dimensions are the cloud
// provider's well-known labels, which are self-describing and wrapped without
// help text (see nodeLabelNamesWithoutNodeName).
var (
	nodeNameLabel  = opmetrics.Label{Name: nodeName, Help: "The name of the node."}
	nodePhaseLabel = opmetrics.Label{Name: nodePhase, Help: "The node's lifecycle phase, e.g. `Pending`, `Running`."}
	managedLabel   = opmetrics.Label{Name: managed, Help: "Whether the node is managed by Karpenter.", Values: metrics.BoolValues}
)

var (
	Allocatable                  opmetrics.GaugeMetric
	TotalPodRequests             opmetrics.GaugeMetric
	TotalPodLimits               opmetrics.GaugeMetric
	TotalDaemonRequests          opmetrics.GaugeMetric
	TotalDaemonLimits            opmetrics.GaugeMetric
	SystemOverhead               opmetrics.GaugeMetric
	Lifetime                     opmetrics.GaugeMetric
	ClusterUtilization           opmetrics.GaugeMetric
	RepairPastDueTimeSeconds     opmetrics.GaugeMetric
	RepairPastDueDurationSeconds opmetrics.ObservationMetric
)

// Initialize metrics at runtime to ensure cloud provider's well-known labels are properly
// injected, preventing race conditions in dependency ordering during label injection for global variable. .
func initializeMetrics() {
	Allocatable = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "allocatable",
			Help:      "Node allocatable are the resources allocatable by nodes.",
		},
		nodeLabelNamesWithResourceType(),
		opmetrics.Beta,
	)
	TotalPodRequests = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "total_pod_requests",
			Help:      "Node total pod requests are the resources requested by pods bound to nodes, including the DaemonSet pods.",
		},
		nodeLabelNamesWithResourceType(),
		opmetrics.Beta,
	)
	TotalPodLimits = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "total_pod_limits",
			Help:      "Node total pod limits are the resources specified by pod limits, including the DaemonSet pods.",
		},
		nodeLabelNamesWithResourceType(),
		opmetrics.Beta,
	)
	TotalDaemonRequests = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "total_daemon_requests",
			Help:      "Node total daemon requests are the resource requested by DaemonSet pods bound to nodes.",
		},
		nodeLabelNamesWithResourceType(),
		opmetrics.Beta,
	)
	TotalDaemonLimits = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "total_daemon_limits",
			Help:      "Node total daemon limits are the resources specified by DaemonSet pod limits.",
		},
		nodeLabelNamesWithResourceType(),
		opmetrics.Beta,
	)
	SystemOverhead = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "system_overhead",
			Help:      "Node system daemon overhead are the resources reserved for system overhead, the difference between the node's capacity and allocatable values are reported by the status.",
		},
		nodeLabelNamesWithResourceType(),
		opmetrics.Beta,
	)
	Lifetime = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "current_lifetime_seconds",
			Help:      "Node age in seconds",
		},
		nodeLabelNames(),
		opmetrics.Alpha,
	)
	ClusterUtilization = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: "cluster",
			Name:      "utilization_percent",
			Help:      "Utilization of allocatable resources by pod requests",
		},
		[]opmetrics.Label{metrics.ResourceType},
		opmetrics.Alpha,
	)
	RepairPastDueTimeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "repair_past_due_time_seconds",
			Help:      "How long a node has been past due on repair, or 0 while it isn't. Every node that Karpenter manages and that has a NodePool reports a value, so the number of past-due nodes is the number of values above 0, for example `count by (nodepool, condition) (karpenter_nodes_repair_past_due_time_seconds > 0)` in Prometheus. A node is past due once it is still unhealthy after all the time repair gets. That time starts when the first of the node's repair policies becomes eligible, and is the 15 minute node registration timeout plus the drain bound at that point: the shortest termination grace period that the NodeClaim and the policies eligible by then set, if any. Policies that become eligible later don't change it. Nodes that repair holds back, such as for disruption budgets, still become past due. Nodes with do-not-repair, and nodes that repair is rebooting, report 0. Labeled like karpenter_nodes_current_lifetime_seconds: node_name, phase, managed and the node's well-known labels, such as nodepool, zone and capacity_type. It also has the condition repair acts on, which is empty while the node isn't past due. Not reported when node repair is off, with --legacy-node-repair, or before cluster state first syncs.",
		},
		nodeLabelNamesWithRepairCondition(),
		opmetrics.Alpha,
	)
	RepairPastDueDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodeSubsystem,
			Name:      "repair_past_due_duration_seconds",
			Help:      "How long a node was past due on repair, as karpenter_nodes_repair_past_due_time_seconds defines it. Recorded each time a node stops being past due: it is gone, repair starts a reboot, it recovers, or repair stops acting on it, such as for do-not-repair. Labeled by phase, managed, the node's well-known labels, such as nodepool, zone and capacity_type, and the condition repair acted on, all as of the last time the node was past due. A stretch that ends while Karpenter is down isn't recorded. Not recorded when node repair is off, with --legacy-node-repair, or before cluster state first syncs.",
			Buckets:   prometheus.ExponentialBuckets(60, 2, 12), // 1m, 2m, 4m, ... up to about 34h
		},
		// Histogram series are never removed, so leave out node_name, which is different for every node.
		append(nodeLabelNamesWithoutNodeName(), disruption.RepairCondition),
		opmetrics.Alpha,
	)
}

func nodeLabelNamesWithResourceType() []opmetrics.Label {
	return append(
		nodeLabelNames(),
		metrics.ResourceType,
	)
}

func nodeLabelNamesWithRepairCondition() []opmetrics.Label {
	return append(
		nodeLabelNames(),
		disruption.RepairCondition,
	)
}

func nodeLabelNames() []opmetrics.Label {
	return append(
		[]opmetrics.Label{nodeNameLabel},
		nodeLabelNamesWithoutNodeName()...,
	)
}

func nodeLabelNamesWithoutNodeName() []opmetrics.Label {
	return append(
		// WellKnownLabels includes the nodepool label, so we don't need to add it as its own item here.
		// If we do, prometheus will panic since there would be duplicate labels.
		// The well-known labels are self-describing, so they are wrapped without help text.
		lo.Map(sets.New(lo.Values(getWellKnownLabels())...).UnsortedList(), func(l string, _ int) opmetrics.Label {
			return opmetrics.Label{Name: l}
		}),
		nodePhaseLabel,
		managedLabel,
	)
}

type Controller struct {
	clock       clock.Clock
	cluster     *state.Cluster
	metricStore *metrics.Store
	// pastDueRepairs is nil when the repair disruption method doesn't run.
	pastDueRepairs *pastDueRepairs
}

// initializeMetricsOnce lets tests build more than one controller, since registering a metric twice panics.
var initializeMetricsOnce sync.Once

func NewController(clk clock.Clock, cluster *state.Cluster) *Controller {
	initializeMetricsOnce.Do(initializeMetrics)
	return &Controller{
		clock:          clk,
		cluster:        cluster,
		metricStore:    metrics.NewStore(),
		pastDueRepairs: newPastDueRepairs(cluster.RepairPolicyMatcher()),
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name()) //nolint:ineffassign,staticcheck
	// Cluster state may miss nodes until another controller, such as disruption, marks it synced.
	reportPastDue := c.pastDueRepairs != nil && c.cluster.HasSynced()

	nodes := lo.Reject(c.cluster.DeepCopyNodes(), func(n *state.StateNode, _ int) bool {
		return n.Node == nil
	})

	// Build per-node metrics
	metricsMap := lo.SliceToMap(nodes, func(n *state.StateNode) (string, []*metrics.StoreMetric) {
		return client.ObjectKeyFromObject(n.Node).String(), buildMetrics(n)
	})

	// Build cluster level metric
	metricsMap["clusterUtilization"] = buildClusterUtilizationMetric(nodes)

	if reportPastDue {
		maps.Copy(metricsMap, c.pastDueRepairs.update(nodes, c.clock.Now()))
	}

	c.metricStore.ReplaceAll(metricsMap)

	return reconciler.Result{RequeueAfter: time.Second * 5}, nil
}

func (c *Controller) Name() string {
	return "metrics.node"
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

func buildClusterUtilizationMetric(nodes state.StateNodes) []*metrics.StoreMetric {

	// Aggregate resources allocated/utilized for all the nodes and pods inside the nodes
	allocatableAggregate, utilizedAggregate := corev1.ResourceList{}, corev1.ResourceList{}

	for _, node := range nodes {
		resources.MergeInto(allocatableAggregate, node.Allocatable())
		resources.MergeInto(utilizedAggregate, node.PodRequests())
	}

	res := make([]*metrics.StoreMetric, 0, len(allocatableAggregate))

	for resourceName, allocatableResource := range allocatableAggregate {

		if allocatableResource.Value() == 0 {
			// This zero check may be unnecessary. I'm erring towards caution.
			continue
		}

		utilizedResource := utilizedAggregate[resourceName]

		// Typecast to float before the calculation to maximize resolution
		utilizationPercentage := 100 * lo.Ternary(
			resourceName == corev1.ResourceCPU,
			float64(utilizedResource.MilliValue())/float64(allocatableResource.MilliValue()),
			float64(utilizedResource.Value())/float64(allocatableResource.Value()))

		res = append(res, &metrics.StoreMetric{
			GaugeMetric: ClusterUtilization,
			Value:       utilizationPercentage,
			Labels:      map[string]string{metrics.ResourceTypeLabel: resourceNameToString(resourceName)},
		})
	}

	return res
}

func buildMetrics(n *state.StateNode) (res []*metrics.StoreMetric) {
	for gaugeMetric, resourceList := range map[opmetrics.GaugeMetric]corev1.ResourceList{
		SystemOverhead:      resources.Subtract(n.Node.Status.Capacity, n.Node.Status.Allocatable),
		TotalPodRequests:    n.PodRequests(),
		TotalPodLimits:      n.PodLimits(),
		TotalDaemonRequests: n.DaemonSetRequests(),
		TotalDaemonLimits:   n.DaemonSetLimits(),
		Allocatable:         n.Node.Status.Allocatable,
	} {
		for resourceName, quantity := range resourceList {
			res = append(res, &metrics.StoreMetric{
				GaugeMetric: gaugeMetric,
				Value:       lo.Ternary(resourceName == corev1.ResourceCPU, float64(quantity.MilliValue())/float64(1000), float64(quantity.Value())),
				Labels:      getNodeLabelsWithResourceType(n, resourceNameToString(resourceName)),
			})
		}
	}
	return append(res,
		&metrics.StoreMetric{
			GaugeMetric: Lifetime,
			Value:       time.Since(n.Node.GetCreationTimestamp().Time).Seconds(),
			Labels:      getNodeLabels(n),
		})
}

func getNodeLabelsWithResourceType(n *state.StateNode, resourceTypeName string) prometheus.Labels {
	metricLabels := getNodeLabels(n)
	metricLabels[metrics.ResourceTypeLabel] = resourceTypeName
	return metricLabels
}

func getNodeLabels(n *state.StateNode) prometheus.Labels {
	node := n.Node
	metricLabels := map[string]string{}
	metricLabels[nodeName] = node.Name
	metricLabels[nodePhase] = string(node.Status.Phase)
	metricLabels[managed] = strconv.FormatBool(n.Managed())

	// Populate well known labels
	for wellKnownLabel, label := range getWellKnownLabels() {
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

func resourceNameToString(resourceName corev1.ResourceName) string {
	return strings.ReplaceAll(strings.ToLower(string(resourceName)), "-", "_")
}
