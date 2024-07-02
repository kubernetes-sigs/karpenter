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

package pod

import (
	"context"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

const (
	podName             = "name"
	podNameSpace        = "namespace"
	ownerSelfLink       = "owner"
	podHostName         = "node"
	podNodePool         = "nodepool"
	podHostZone         = "zone"
	podHostArchitecture = "arch"
	podHostCapacityType = "capacity_type"
	podHostInstanceType = "instance_type"
	podPhase            = "phase"

	phasePending = "Pending"
)

var (
	podGaugeVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "karpenter",
			Subsystem: "pods",
			Name:      "state",
			Help:      "Pod state is the current state of pods. This metric can be used several ways as it is labeled by the pod name, namespace, owner, node, nodepool name, zone, architecture, capacity type, instance type and pod phase.",
		},
		labelNames(),
	)
	podStartupTimeSummary = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Namespace:  "karpenter",
			Subsystem:  "pods",
			Name:       "startup_time_seconds",
			Help:       "The time from pod creation until the pod is running.",
			Objectives: metrics.SummaryObjectives(),
		},
	)
)

// Controller for the resource
type Controller struct {
	kubeClient  client.Client
	metricStore *metrics.Store

	pendingPods sets.Set[string]
}

func init() {
	crmetrics.Registry.MustRegister(podGaugeVec)
	crmetrics.Registry.MustRegister(podStartupTimeSummary)
}

func labelNames() []string {
	return []string{
		podName,
		podNameSpace,
		ownerSelfLink,
		podHostName,
		podNodePool,
		podHostZone,
		podHostArchitecture,
		podHostCapacityType,
		podHostInstanceType,
		podPhase,
	}
}

// NewController constructs a podController instance
func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient:  kubeClient,
		metricStore: metrics.NewStore(),
		pendingPods: sets.New[string](),
	}
}

func (c *Controller) Name() string {
	return "metrics.pod"
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "metrics.pod")

	pod := &corev1.Pod{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, pod); err != nil {
		if errors.IsNotFound(err) {
			c.pendingPods.Delete(req.NamespacedName.String())
			c.metricStore.Delete(req.NamespacedName.String())
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	labels, err := c.makeLabels(ctx, pod)
	if err != nil {
		return reconcile.Result{}, err
	}
	c.metricStore.Update(client.ObjectKeyFromObject(pod).String(), []*metrics.StoreMetric{
		{
			GaugeVec: podGaugeVec,
			Value:    1,
			Labels:   labels,
		},
	})
	c.recordPodStartupMetric(pod)
	return reconcile.Result{}, nil
}

func (c *Controller) recordPodStartupMetric(pod *corev1.Pod) {
	key := client.ObjectKeyFromObject(pod).String()
	if pod.Status.Phase == phasePending {
		c.pendingPods.Insert(key)
		return
	}
	cond, ok := lo.Find(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady
	})
	if c.pendingPods.Has(key) && ok {
		podStartupTimeSummary.Observe(cond.LastTransitionTime.Sub(pod.CreationTimestamp.Time).Seconds())
		c.pendingPods.Delete(key)
	}
}

// makeLabels creates the makeLabels using the current state of the pod
func (c *Controller) makeLabels(ctx context.Context, pod *corev1.Pod) (prometheus.Labels, error) {
	metricLabels := prometheus.Labels{}
	metricLabels[podName] = pod.Name
	metricLabels[podNameSpace] = pod.Namespace
	// Selflink has been deprecated after v.1.20
	// Manually generate the selflink for the first owner reference
	// Currently we do not support multiple owner references
	selflink := ""
	if len(pod.OwnerReferences) > 0 {
		selflink = fmt.Sprintf("/apis/%s/namespaces/%s/%ss/%s", pod.OwnerReferences[0].APIVersion, pod.Namespace, strings.ToLower(pod.OwnerReferences[0].Kind), pod.OwnerReferences[0].Name)
	}
	metricLabels[ownerSelfLink] = selflink
	metricLabels[podHostName] = pod.Spec.NodeName
	metricLabels[podPhase] = string(pod.Status.Phase)

	node := &corev1.Node{}
	if pod.Spec.NodeName != "" {
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); client.IgnoreNotFound(err) != nil {
			return nil, err
		}
	}
	metricLabels[podHostZone] = node.Labels[corev1.LabelTopologyZone]
	metricLabels[podHostArchitecture] = node.Labels[corev1.LabelArchStable]
	metricLabels[podHostCapacityType] = node.Labels[v1.CapacityTypeLabelKey]
	metricLabels[podHostInstanceType] = node.Labels[corev1.LabelInstanceTypeStable]
	metricLabels[podNodePool] = node.Labels[v1.NodePoolLabelKey]
	return metricLabels, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("metrics.pod").
		For(&corev1.Pod{}).
		Complete(c)
}
