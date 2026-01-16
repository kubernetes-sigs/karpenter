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

package nodepool

import (
	"context"
	"strings"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

var (
	Limit = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "limit",
			Help:      "Limits specified on the nodepool that restrict the quantity of resources provisioned. Labeled by nodepool name and resource type.",
		},
		[]string{
			metrics.ResourceTypeLabel,
			metrics.NodePoolLabel,
		},
	)
	Usage = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "usage",
			Help:      "The amount of resources that have been provisioned for a nodepool. Labeled by nodepool name and resource type.",
		},
		[]string{
			metrics.ResourceTypeLabel,
			metrics.NodePoolLabel,
		},
	)
	ClusterCost = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "cost_total",
			Help:      "ALPHA METRIC. Total cost of the nodepool from Karpenter's perspective. Units are determined by the cloud provider. Not an authoritative source for billing. Includes modifications due to NodeOverlays",
		},
		[]string{metrics.NodePoolLabel},
	)
)

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	clusterCost   *cost.ClusterCost
	cluster       *state.Cluster
	clock         clock.Clock
	metricStore   *metrics.Store
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, clusterCost *cost.ClusterCost, cluster *state.Cluster, clk clock.Clock) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		clusterCost:   clusterCost,
		cluster:       cluster,
		clock:         clk,
		metricStore:   metrics.NewStore(),
	}
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	nodePool := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, nodePool); err != nil {
		if errors.IsNotFound(err) {
			c.metricStore.Delete(req.String())
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !nodepoolutils.IsManaged(nodePool, c.cloudProvider) {
		return reconcile.Result{}, nil
	}
	c.metricStore.Update(req.String(), c.buildMetrics(nodePool))

	// Update disruption budget metrics
	c.updateDisruptionBudgetMetrics(ctx, nodePool)

	// periodically update our metrics per nodepool even if nothing has changed
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (c *Controller) buildMetrics(nodePool *v1.NodePool) (res []*metrics.StoreMetric) {
	res = append(res, &metrics.StoreMetric{
		GaugeMetric: ClusterCost,
		Labels:      map[string]string{metrics.NodePoolLabel: nodePool.Name},
		Value:       c.clusterCost.GetNodepoolCost(nodePool),
	})

	for gaugeVec, resourceList := range map[opmetrics.GaugeMetric]corev1.ResourceList{
		Usage: nodePool.Status.Resources,
		Limit: getLimits(nodePool),
	} {
		for k, v := range resourceList {
			res = append(res, &metrics.StoreMetric{
				GaugeMetric: gaugeVec,
				Labels:      makeLabels(nodePool, strings.ReplaceAll(strings.ToLower(string(k)), "-", "_")),
				Value:       lo.Ternary(k == corev1.ResourceCPU, float64(v.MilliValue())/float64(1000), float64(v.Value())),
			})
		}
	}
	return res
}

func getLimits(nodePool *v1.NodePool) corev1.ResourceList {
	if nodePool.Spec.Limits != nil {
		return corev1.ResourceList(nodePool.Spec.Limits)
	}
	return corev1.ResourceList{}
}

func makeLabels(nodePool *v1.NodePool, resourceTypeName string) prometheus.Labels {
	return map[string]string{
		metrics.ResourceTypeLabel: resourceTypeName,
		metrics.NodePoolLabel:     nodePool.Name,
	}
}

func (c *Controller) Name() string {
	return "metrics.nodepool"
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1.NodePool{}, builder.WithPredicates(nodepoolutils.IsManagedPredicateFuncs(c.cloudProvider))).
		Complete(c)
}

// updateDisruptionBudgetMetrics updates the allowed_disruptions metric for all disruption reasons
func (c *Controller) updateDisruptionBudgetMetrics(_ context.Context, nodePool *v1.NodePool) {
	// Count nodes in this NodePool
	numNodes := 0
	for node := range c.cluster.Nodes() {
		if nodePoolName := node.Labels()[v1.NodePoolLabelKey]; nodePoolName == nodePool.Name {
			// Only count managed and initialized nodes
			if node.Managed() && node.Initialized() {
				// Skip nodes that are terminating
				if !node.NodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating).IsTrue() {
					numNodes++
				}
			}
		}
	}

	// Update metrics for all disruption reasons
	for _, reason := range []v1.DisruptionReason{
		v1.DisruptionReasonDrifted,
		v1.DisruptionReasonEmpty,
		v1.DisruptionReasonUnderutilized,
	} {
		allowedDisruptions := nodePool.MustGetAllowedDisruptions(c.clock, numNodes, reason)
		disruption.NodePoolAllowedDisruptions.Set(float64(allowedDisruptions), map[string]string{
			metrics.NodePoolLabel: nodePool.Name,
			metrics.ReasonLabel:   string(reason),
		})
	}
}
