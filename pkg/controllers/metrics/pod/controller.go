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
	"strconv"
	"strings"
	"time"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
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

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
)

const (
	podName             = "name"
	podNamespace        = "namespace"
	ownerSelfLink       = "owner"
	podHostName         = "node"
	podHostArchitecture = "arch"
	podHostInstanceType = "instance_type"
	podPhase            = "phase"
	podScheduled        = "scheduled"
	podReady            = "ready"
	managed             = "managed"
)

// Pod metric dimensions. zone, nodepool, and capacity_type reuse the shared
// dimensions from pkg/metrics.
var (
	podNameLabel             = opmetrics.Label{Name: podName, Help: "The name of the pod."}
	podNamespaceLabel        = opmetrics.Label{Name: podNamespace, Help: "The namespace of the pod."}
	ownerLabel               = opmetrics.Label{Name: ownerSelfLink, Help: "The owning workload of the pod, formatted as `<kind>/<name>`."}
	podHostNameLabel         = opmetrics.Label{Name: podHostName, Help: "The name of the node the pod is bound to."}
	podHostArchitectureLabel = opmetrics.Label{Name: podHostArchitecture, Help: "The CPU architecture of the node the pod is bound to."}
	podHostInstanceTypeLabel = opmetrics.Label{Name: podHostInstanceType, Help: "The instance type of the node the pod is bound to."}
	podScheduledLabel        = opmetrics.Label{Name: podScheduled, Help: "Whether the pod has been scheduled to a node.", Values: metrics.BoolValues}
	podReadyLabel            = opmetrics.Label{Name: podReady, Help: "Whether the pod is ready.", Values: metrics.BoolValues}
	managedLabel             = opmetrics.Label{Name: managed, Help: "Whether the pod is bound to a Karpenter-managed node.", Values: metrics.BoolValues}
	podPhaseLabel            = opmetrics.Label{
		Name: podPhase,
		Help: "The pod's lifecycle phase.",
		Values: []opmetrics.Value{
			{Name: string(corev1.PodPending), Help: "The pod has been accepted but not all containers are running."},
			{Name: string(corev1.PodRunning), Help: "The pod is bound to a node and all containers are running."},
			{Name: string(corev1.PodSucceeded), Help: "All containers terminated successfully."},
			{Name: string(corev1.PodFailed), Help: "All containers terminated and at least one failed."},
			{Name: string(corev1.PodUnknown), Help: "The pod's state could not be obtained."},
		},
	}
)

var (
	PodState = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.PodSubsystem,
			Name:      "state",
			Help:      "Pod state is the current state of pods. This metric can be used several ways as it is labeled by the pod name, namespace, owner, node, whether the pod is scheduled, nodepool name, zone, architecture, capacity type, instance type, pod phase, pod readiness, and whether the node is Karpenter-managed.",
		},
		labelNames(),
	)
	PodStartupDurationSeconds = opmetrics.NewPrometheusSummary(
		crmetrics.Registry,
		prometheus.SummaryOpts{
			Namespace:  metrics.Namespace,
			Subsystem:  metrics.PodSubsystem,
			Name:       "startup_duration_seconds",
			Help:       "The time from pod creation until the pod is running.",
			Objectives: metrics.SummaryObjectives(),
		},
		[]opmetrics.Label{},
	)
	PodUnstartedTimeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.PodSubsystem,
			Name:      "unstarted_time_seconds",
			Help:      "The time from pod creation until the pod is running.",
		},
		[]opmetrics.Label{podNameLabel, podNamespaceLabel},
	)
	PodBoundDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.PodSubsystem,
			Name:      "bound_duration_seconds",
			Help:      "The time from pod creation until the pod is bound.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]opmetrics.Label{},
	)
	PodUnboundTimeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.PodSubsystem,
			Name:      "unbound_time_seconds",
			Help:      "The time from pod creation until the pod is bound.",
		},
		[]opmetrics.Label{podNameLabel, podNamespaceLabel},
	)
	// Stage: alpha
	PodProvisioningBoundDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.PodSubsystem,
			Name:      "provisioning_bound_duration_seconds",
			Help:      "The time from when Karpenter first thinks the pod can schedule until it binds. Note: this calculated from a point in memory, not by the pod creation timestamp.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]opmetrics.Label{},
	)
	// Stage: alpha
	PodProvisioningUnboundTimeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.PodSubsystem,
			Name:      "provisioning_unbound_time_seconds",
			Help:      "The time from when Karpenter first thinks the pod can schedule until it binds. Note: this calculated from a point in memory, not by the pod creation timestamp.",
		},
		[]opmetrics.Label{podNameLabel, podNamespaceLabel},
	)
	// Stage: alpha
	PodProvisioningStartupDurationSeconds = opmetrics.NewPrometheusHistogram(
		crmetrics.Registry,
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.PodSubsystem,
			Name:      "provisioning_startup_duration_seconds",
			Help:      "The time from when Karpenter first thinks the pod can schedule until the pod is running. Note: this calculated from a point in memory, not by the pod creation timestamp.",
			Buckets:   metrics.DurationBuckets(),
		},
		[]opmetrics.Label{},
	)
	// Stage: alpha
	PodProvisioningUnstartedTimeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.PodSubsystem,
			Name:      "provisioning_unstarted_time_seconds",
			Help:      "The time from when Karpenter first thinks the pod can schedule until the pod is running. Note: this calculated from a point in memory, not by the pod creation timestamp.",
		},
		[]opmetrics.Label{podNameLabel, podNamespaceLabel},
	)
	// Stage: alpha
	PodSchedulingUndecidedTimeSeconds = opmetrics.NewPrometheusGauge(
		crmetrics.Registry,
		prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.PodSubsystem,
			Name:      "provisioning_scheduling_undecided_time_seconds",
			Help:      "The time from when Karpenter has seen a pod without making a scheduling decision for the pod. Note: this calculated from a point in memory, not by the pod creation timestamp.",
		},
		[]opmetrics.Label{podNameLabel, podNamespaceLabel},
	)
)

// Controller for the resource
type Controller struct {
	kubeClient  client.Client
	metricStore *metrics.Store
	cluster     *state.Cluster

	pendingPods     sets.Set[string]
	unscheduledPods sets.Set[string]
}

func labelNames() []opmetrics.Label {
	return []opmetrics.Label{
		podNameLabel,
		podNamespaceLabel,
		ownerLabel,
		podHostNameLabel,
		podScheduledLabel,
		metrics.NodePool,
		metrics.Zone,
		podHostArchitectureLabel,
		metrics.CapacityType,
		podHostInstanceTypeLabel,
		podPhaseLabel,
		podReadyLabel,
		managedLabel,
	}
}

// NewController constructs a podController instance
func NewController(kubeClient client.Client, cluster *state.Cluster) *Controller {
	return &Controller{
		kubeClient:      kubeClient,
		metricStore:     metrics.NewStore(),
		pendingPods:     sets.New[string](),
		unscheduledPods: sets.New[string](),
		cluster:         cluster,
	}
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	pod := &corev1.Pod{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, pod); err != nil {
		if errors.IsNotFound(err) {
			c.pendingPods.Delete(req.String())
			// Delete the unstarted metric since the pod is deleted
			PodUnstartedTimeSeconds.Delete(map[string]string{
				podName:      req.Name,
				podNamespace: req.Namespace,
			})
			PodProvisioningUnstartedTimeSeconds.Delete(map[string]string{
				podName:      req.Name,
				podNamespace: req.Namespace,
			})
			c.unscheduledPods.Delete(req.String())
			// Delete the unbound metric since the pod is deleted
			PodUnboundTimeSeconds.Delete(map[string]string{
				podName:      req.Name,
				podNamespace: req.Namespace,
			})
			PodProvisioningUnboundTimeSeconds.Delete(map[string]string{
				podName:      req.Name,
				podNamespace: req.Namespace,
			})
			PodSchedulingUndecidedTimeSeconds.Delete(map[string]string{
				podName:      req.Name,
				podNamespace: req.Namespace,
			})
			c.metricStore.Delete(req.String())
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	labels, err := c.makeLabels(ctx, pod)
	if err != nil {
		return reconcile.Result{}, err
	}
	c.metricStore.Update(client.ObjectKeyFromObject(pod).String(), []*metrics.StoreMetric{
		{
			GaugeMetric: PodState,
			Value:       1,
			Labels:      labels,
		},
	})
	c.recordPodSchedulingUndecidedMetric(pod)
	// Get the time for when we Karpenter first thought the pod was schedulable. This should be zero if we didn't simulate for this pod.
	schedulableTime := c.cluster.PodSchedulingSuccessTime(types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace})
	c.recordPodStartupMetric(pod, schedulableTime)
	c.recordPodBoundMetric(pod, schedulableTime)
	// Requeue every 30s for pods that are stuck without a state change
	return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
}

func (c *Controller) recordPodSchedulingUndecidedMetric(pod *corev1.Pod) {
	nn := client.ObjectKeyFromObject(pod)
	_, scheduled := lo.Find(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue
	})
	// If we've made a decision on this pod, the pod is already bound, or the pod is terminal, delete the metric idempotently and return
	if decisionTime := c.cluster.PodSchedulingDecisionTime(nn); !decisionTime.IsZero() || scheduled || pod.Spec.NodeName != "" || podutils.IsTerminal(pod) {
		PodSchedulingUndecidedTimeSeconds.Delete(map[string]string{
			podName:      pod.Name,
			podNamespace: pod.Namespace,
		})
		return
	}
	// If we haven't made a decision, get the time that we ACK'd the pod and emit the metric based on that
	if podAckTime := c.cluster.PodAckTime(nn); !podAckTime.IsZero() {
		PodSchedulingUndecidedTimeSeconds.Set(time.Since(podAckTime).Seconds(), map[string]string{
			podName:      pod.Name,
			podNamespace: pod.Namespace,
		})
		return
	}
}

func (c *Controller) recordPodStartupMetric(pod *corev1.Pod, schedulableTime time.Time) {
	key := client.ObjectKeyFromObject(pod).String()
	if pod.Status.Phase == corev1.PodPending {
		PodUnstartedTimeSeconds.Set(time.Since(pod.CreationTimestamp.Time).Seconds(), map[string]string{
			podName:      pod.Name,
			podNamespace: pod.Namespace,
		})
		if !schedulableTime.IsZero() {
			PodProvisioningUnstartedTimeSeconds.Set(time.Since(schedulableTime).Seconds(), map[string]string{
				podName:      pod.Name,
				podNamespace: pod.Namespace,
			})
		} else {
			// Idempotently delete the unstarted_time_seconds metric if the schedulable time is zero
			PodProvisioningUnstartedTimeSeconds.Delete(map[string]string{
				podName:      pod.Name,
				podNamespace: pod.Namespace,
			})
		}
		c.pendingPods.Insert(key)
		return
	}
	cond, ready := lo.Find(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	})
	if c.pendingPods.Has(key) {
		if ready {
			// Delete the unstarted metric since the pod is now started
			PodUnstartedTimeSeconds.Delete(map[string]string{
				podName:      pod.Name,
				podNamespace: pod.Namespace,
			})
			PodProvisioningUnstartedTimeSeconds.Delete(map[string]string{
				podName:      pod.Name,
				podNamespace: pod.Namespace,
			})
			PodStartupDurationSeconds.Observe(cond.LastTransitionTime.Sub(pod.CreationTimestamp.Time).Seconds(), nil)
			if !schedulableTime.IsZero() {
				PodProvisioningStartupDurationSeconds.Observe(cond.LastTransitionTime.Sub(schedulableTime).Seconds(), nil)
			}
			c.pendingPods.Delete(key)
			// Clear cluster state's representation of these pods as we don't need to keep track of them anymore
			c.cluster.ClearPodSchedulingMappings(client.ObjectKeyFromObject(pod))
		} else if podutils.IsTerminal(pod) {
			// We do not emit the startup duration metric for pods that are terminal because such pods will have
			// Ready status condition set to False which will cause the metric to take negative values.

			// Delete the unstarted metric since the pod is now started
			PodUnstartedTimeSeconds.Delete(map[string]string{
				podName:      pod.Name,
				podNamespace: pod.Namespace,
			})
			PodProvisioningUnstartedTimeSeconds.Delete(map[string]string{
				podName:      pod.Name,
				podNamespace: pod.Namespace,
			})
			c.pendingPods.Delete(key)
			// Clear cluster state's representation of these pods as we don't need to keep track of them anymore
			c.cluster.ClearPodSchedulingMappings(client.ObjectKeyFromObject(pod))
		} else {
			PodUnstartedTimeSeconds.Set(time.Since(pod.CreationTimestamp.Time).Seconds(), map[string]string{
				podName:      pod.Name,
				podNamespace: pod.Namespace,
			})
			if !schedulableTime.IsZero() {
				PodProvisioningUnstartedTimeSeconds.Set(time.Since(schedulableTime).Seconds(), map[string]string{
					podName:      pod.Name,
					podNamespace: pod.Namespace,
				})
			} else {
				// Idempotently delete the unstarted_time_seconds metric if the schedulable time is zero
				PodProvisioningUnstartedTimeSeconds.Delete(map[string]string{
					podName:      pod.Name,
					podNamespace: pod.Namespace,
				})
			}
		}
	}
}
func (c *Controller) recordPodBoundMetric(pod *corev1.Pod, schedulableTime time.Time) {
	key := client.ObjectKeyFromObject(pod).String()
	cond, ok := lo.Find(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodScheduled
	})
	if pod.Status.Phase == corev1.PodPending {
		if !ok || cond.Status != corev1.ConditionTrue {
			// If the podScheduled condition does not exist, or it exists and is not set to true, we emit pod_current_unbound_time_seconds metric.
			PodUnboundTimeSeconds.Set(time.Since(pod.CreationTimestamp.Time).Seconds(), map[string]string{
				podName:      pod.Name,
				podNamespace: pod.Namespace,
			})
			if !schedulableTime.IsZero() {
				PodProvisioningUnboundTimeSeconds.Set(time.Since(schedulableTime).Seconds(), map[string]string{
					podName:      pod.Name,
					podNamespace: pod.Namespace,
				})
			} else {
				// Idempotently delete the unbound_time_seconds metric if the schedulable time is zero
				PodProvisioningUnboundTimeSeconds.Delete(map[string]string{
					podName:      pod.Name,
					podNamespace: pod.Namespace,
				})
			}
		}
		c.unscheduledPods.Insert(key)
		return
	}
	if c.unscheduledPods.Has(key) && ok && cond.Status == corev1.ConditionTrue {
		// Delete the unbound metric since the pod is now bound
		PodUnboundTimeSeconds.Delete(map[string]string{
			podName:      pod.Name,
			podNamespace: pod.Namespace,
		})
		PodProvisioningUnboundTimeSeconds.Delete(map[string]string{
			podName:      pod.Name,
			podNamespace: pod.Namespace,
		})

		PodBoundDurationSeconds.Observe(cond.LastTransitionTime.Sub(pod.CreationTimestamp.Time).Seconds(), nil)
		if !schedulableTime.IsZero() {
			PodProvisioningBoundDurationSeconds.Observe(cond.LastTransitionTime.Sub(schedulableTime).Seconds(), nil)
		}
		c.unscheduledPods.Delete(key)
	}
}

// makeLabels creates the makeLabels using the current state of the pod
func (c *Controller) makeLabels(ctx context.Context, pod *corev1.Pod) (prometheus.Labels, error) {
	metricLabels := map[string]string{}
	metricLabels[podName] = pod.Name
	metricLabels[podNamespace] = pod.Namespace
	// Selflink has been deprecated after v.1.20
	// Manually generate the selflink for the first owner reference
	// Currently we do not support multiple owner references
	selflink := ""
	if len(pod.OwnerReferences) > 0 {
		selflink = fmt.Sprintf("/apis/%s/namespaces/%s/%ss/%s", pod.OwnerReferences[0].APIVersion, pod.Namespace, strings.ToLower(pod.OwnerReferences[0].Kind), pod.OwnerReferences[0].Name)
	}
	metricLabels[ownerSelfLink] = selflink
	metricLabels[podHostName] = pod.Spec.NodeName
	metricLabels[podScheduled] = lo.Ternary(pod.Spec.NodeName != "", "true", "false")
	metricLabels[podPhase] = string(pod.Status.Phase)

	_, ready := lo.Find(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	})
	metricLabels[podReady] = lo.Ternary(ready, "true", "false")

	node := &corev1.Node{}
	if pod.Spec.NodeName != "" {
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); client.IgnoreNotFound(err) != nil {
			return nil, err
		}
	}
	metricLabels[metrics.ZoneLabel] = node.Labels[corev1.LabelTopologyZone]
	metricLabels[podHostArchitecture] = node.Labels[corev1.LabelArchStable]
	metricLabels[metrics.CapacityTypeLabel] = node.Labels[v1.CapacityTypeLabelKey]
	metricLabels[podHostInstanceType] = node.Labels[corev1.LabelInstanceTypeStable]
	metricLabels[metrics.NodePoolLabel] = node.Labels[v1.NodePoolLabelKey]
	metricLabels[managed] = strconv.FormatBool(node.Labels[v1.NodePoolLabelKey] != "")
	return metricLabels, nil
}

func (c *Controller) Name() string {
	return "metrics.pod"
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&corev1.Pod{}).
		Complete(c)
}
