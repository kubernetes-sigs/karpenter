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
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nodeutil "sigs.k8s.io/karpenter/pkg/utils/node"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/controller"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/functional"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// LaunchOptions are the set of options that can be used to trigger certain
// actions and configuration during scheduling
type LaunchOptions struct {
	RecordPodNomination bool
	Reason              string
}

// RecordPodNomination causes nominate pod events to be recorded against the node.
func RecordPodNomination(o LaunchOptions) LaunchOptions {
	o.RecordPodNomination = true
	return o
}

func WithReason(reason string) func(LaunchOptions) LaunchOptions {
	return func(o LaunchOptions) LaunchOptions {
		o.Reason = reason
		return o
	}
}

// Provisioner waits for enqueued pods, batches them, creates capacity and binds the pods to the capacity.
type Provisioner struct {
	cloudProvider  cloudprovider.CloudProvider
	kubeClient     client.Client
	batcher        *Batcher
	volumeTopology *scheduler.VolumeTopology
	cluster        *state.Cluster
	recorder       events.Recorder
	cm             *pretty.ChangeMonitor
}

func NewProvisioner(kubeClient client.Client, recorder events.Recorder,
	cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster,
) *Provisioner {
	p := &Provisioner{
		batcher:        NewBatcher(),
		cloudProvider:  cloudProvider,
		kubeClient:     kubeClient,
		volumeTopology: scheduler.NewVolumeTopology(kubeClient),
		cluster:        cluster,
		recorder:       recorder,
		cm:             pretty.NewChangeMonitor(),
	}
	return p
}

func (p *Provisioner) Name() string {
	return "provisioner"
}

func (p *Provisioner) Trigger() {
	p.batcher.Trigger()
}

func (p *Provisioner) Builder(_ context.Context, mgr manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(mgr)
}

func (p *Provisioner) Reconcile(ctx context.Context, _ reconcile.Request) (result reconcile.Result, err error) {
	// Batch pods
	if triggered := p.batcher.Wait(ctx); !triggered {
		return reconcile.Result{}, nil
	}
	// We need to ensure that our internal cluster state mechanism is synced before we proceed
	// with making any scheduling decision off of our state nodes. Otherwise, we have the potential to make
	// a scheduling decision based on a smaller subset of nodes in our cluster state than actually exist.
	if !p.cluster.Synced(ctx) {
		logging.FromContext(ctx).Debugf("waiting on cluster sync")
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}

	// Schedule pods to potential nodes, exit if nothing to do
	results, err := p.Schedule(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(results.NewNodeClaims) == 0 {
		return reconcile.Result{}, nil
	}
	_, err = p.CreateNodeClaims(ctx, results.NewNodeClaims, WithReason(metrics.ProvisioningReason), RecordPodNomination)
	return reconcile.Result{}, err
}

// CreateNodeClaims launches nodes passed into the function in parallel. It returns a slice of the successfully created node
// names as well as a multierr of any errors that occurred while launching nodes
func (p *Provisioner) CreateNodeClaims(ctx context.Context, nodeClaims []*scheduler.NodeClaim, opts ...functional.Option[LaunchOptions]) ([]string, error) {
	// Create capacity and bind pods
	errs := make([]error, len(nodeClaims))
	nodeClaimNames := make([]string, len(nodeClaims))
	workqueue.ParallelizeUntil(ctx, len(nodeClaims), len(nodeClaims), func(i int) {
		// create a new context to avoid a data race on the ctx variable
		if name, err := p.Create(ctx, nodeClaims[i], opts...); err != nil {
			errs[i] = fmt.Errorf("creating node claim, %w", err)
		} else {
			nodeClaimNames[i] = name
		}
	})
	return nodeClaimNames, multierr.Combine(errs...)
}

func (p *Provisioner) GetPendingPods(ctx context.Context) ([]*v1.Pod, error) {
	// filter for provisionable pods first, so we don't check for validity/PVCs on pods we won't provision anyway
	// (e.g. those owned by daemonsets)
	pods, err := nodeutil.GetProvisionablePods(ctx, p.kubeClient)
	if err != nil {
		return nil, fmt.Errorf("listing pods, %w", err)
	}
	return lo.Reject(pods, func(po *v1.Pod, _ int) bool {
		if err := p.Validate(ctx, po); err != nil {
			logging.FromContext(ctx).With("pod", client.ObjectKeyFromObject(po)).Debugf("ignoring pod, %s", err)
			return true
		}
		p.consolidationWarnings(ctx, po)
		return false
	}), nil
}

// consolidationWarnings potentially writes logs warning about possible unexpected interactions between scheduling
// constraints and consolidation
func (p *Provisioner) consolidationWarnings(ctx context.Context, po *v1.Pod) {
	// We have pending pods that have preferred anti-affinity or topology spread constraints.  These can interact
	// unexpectedly with consolidation so we warn once per hour when we see these pods.
	if po.Spec.Affinity != nil && po.Spec.Affinity.PodAntiAffinity != nil {
		if len(po.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 0 {
			if p.cm.HasChanged(string(po.UID), "pod-antiaffinity") {
				logging.FromContext(ctx).Infof("pod %q has a preferred Anti-Affinity which can prevent consolidation", client.ObjectKeyFromObject(po))
			}
		}
	}
	for _, tsc := range po.Spec.TopologySpreadConstraints {
		if tsc.WhenUnsatisfiable == v1.ScheduleAnyway {
			if p.cm.HasChanged(string(po.UID), "pod-topology-spread") {
				logging.FromContext(ctx).Infof("pod %q has a preferred TopologySpreadConstraint which can prevent consolidation", client.ObjectKeyFromObject(po))
			}
		}
	}
}

var ErrNodePoolsNotFound = errors.New("no nodepools found")

//nolint:gocyclo
func (p *Provisioner) NewScheduler(ctx context.Context, pods []*v1.Pod, stateNodes []*state.StateNode) (*scheduler.Scheduler, error) {
	nodePoolList := &v1beta1.NodePoolList{}
	err := p.kubeClient.List(ctx, nodePoolList)
	if err != nil {
		return nil, fmt.Errorf("listing node pools, %w", err)
	}
	nodePoolList.Items = lo.Filter(nodePoolList.Items, func(n v1beta1.NodePool, _ int) bool {
		if err := n.RuntimeValidate(); err != nil {
			logging.FromContext(ctx).With("nodepool", n.Name).Errorf("nodepool failed validation, %s", err)
			return false
		}
		return n.DeletionTimestamp.IsZero()
	})
	if len(nodePoolList.Items) == 0 {
		return nil, ErrNodePoolsNotFound
	}

	// nodeTemplates generated from NodePools are ordered by weight
	// since they are stored within a slice and scheduling
	// will always attempt to schedule on the first nodeTemplate
	nodePoolList.OrderByWeight()

	instanceTypes := map[string][]*cloudprovider.InstanceType{}
	domains := map[string]sets.Set[string]{}
	for _, nodePool := range nodePoolList.Items {
		// Get instance type options
		instanceTypeOptions, err := p.cloudProvider.GetInstanceTypes(ctx, lo.ToPtr(nodePool))
		if err != nil {
			// we just log an error and skip the provisioner to prevent a single mis-configured provisioner from stopping
			// all scheduling
			logging.FromContext(ctx).With("nodepool", nodePool.Name).Errorf("skipping, unable to resolve instance types, %s", err)
			continue
		}
		if len(instanceTypeOptions) == 0 {
			logging.FromContext(ctx).With("nodepool", nodePool.Name).Info("skipping, no resolved instance types found")
			continue
		}
		instanceTypes[nodePool.Name] = append(instanceTypes[nodePool.Name], instanceTypeOptions...)

		// Construct Topology Domains
		for _, instanceType := range instanceTypeOptions {
			// We need to intersect the instance type requirements with the current nodePool requirements.  This
			// ensures that something like zones from an instance type don't expand the universe of valid domains.
			requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nodePool.Spec.Template.Spec.Requirements...)
			requirements.Add(scheduling.NewLabelRequirements(nodePool.Spec.Template.Labels).Values()...)
			requirements.Add(instanceType.Requirements.Values()...)

			for key, requirement := range requirements {
				// This code used to execute a Union between domains[key] and requirement.Values().
				// The downside of this is that Union is immutable and takes a copy of the set it is executed upon.
				// This resulted in a lot of memory pressure on the heap and poor performance
				// https://github.com/aws/karpenter/issues/3565
				if domains[key] == nil {
					domains[key] = sets.New(requirement.Values()...)
				} else {
					domains[key].Insert(requirement.Values()...)
				}
			}
		}

		requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nodePool.Spec.Template.Spec.Requirements...)
		requirements.Add(scheduling.NewLabelRequirements(nodePool.Spec.Template.Labels).Values()...)
		for key, requirement := range requirements {
			if requirement.Operator() == v1.NodeSelectorOpIn {
				// The following is a performance optimisation, for the explanation see the comment above
				if domains[key] == nil {
					domains[key] = sets.New(requirement.Values()...)
				} else {
					domains[key].Insert(requirement.Values()...)
				}
			}
		}
	}

	// inject topology constraints
	pods = p.injectVolumeTopologyRequirements(ctx, pods)

	// Calculate cluster topology
	topology, err := scheduler.NewTopology(ctx, p.kubeClient, p.cluster, domains, pods)
	if err != nil {
		return nil, fmt.Errorf("tracking topology counts, %w", err)
	}
	daemonSetPods, err := p.getDaemonSetPods(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting daemon pods, %w", err)
	}
	return scheduler.NewScheduler(ctx, p.kubeClient, lo.ToSlicePtr(nodePoolList.Items), p.cluster, stateNodes, topology, instanceTypes, daemonSetPods, p.recorder), nil
}

func (p *Provisioner) Schedule(ctx context.Context) (scheduler.Results, error) {
	defer metrics.Measure(schedulingDuration)()
	start := time.Now()

	// We collect the nodes with their used capacities before we get the list of pending pods. This ensures that
	// the node capacities we schedule against are always >= what the actual capacity is at any given instance. This
	// prevents over-provisioning at the cost of potentially under-provisioning which will self-heal during the next
	// scheduling loop when we launch a new node.  When this order is reversed, our node capacity may be reduced by pods
	// that have bound which we then provision new un-needed capacity for.
	// -------
	// We don't consider the nodes that are MarkedForDeletion since this capacity shouldn't be considered
	// as persistent capacity for the cluster (since it will soon be removed). Additionally, we are scheduling for
	// the pods that are on these nodes so the MarkedForDeletion node capacity can't be considered.
	nodes := p.cluster.Nodes()

	// Get pods, exit if nothing to do
	pendingPods, err := p.GetPendingPods(ctx)
	if err != nil {
		return scheduler.Results{}, err
	}
	// Get pods from nodes that are preparing for deletion
	// We do this after getting the pending pods so that we undershoot if pods are
	// actively migrating from a node that is being deleted
	// NOTE: The assumption is that these nodes are cordoned and no additional pods will schedule to them
	deletingNodePods, err := nodes.Deleting().ReschedulablePods(ctx, p.kubeClient)
	if err != nil {
		return scheduler.Results{}, err
	}
	pods := append(pendingPods, deletingNodePods...)
	// nothing to schedule, so just return success
	if len(pods) == 0 {
		return scheduler.Results{}, nil
	}
	s, err := p.NewScheduler(ctx, pods, nodes.Active())
	if err != nil {
		if errors.Is(err, ErrNodePoolsNotFound) {
			logging.FromContext(ctx).Info(ErrNodePoolsNotFound)
			return scheduler.Results{}, nil
		}
		return scheduler.Results{}, fmt.Errorf("creating scheduler, %w", err)
	}
	results := s.Solve(ctx, pods).TruncateInstanceTypes(scheduler.MaxInstanceTypes)
	logging.FromContext(ctx).With("pods", pretty.Slice(lo.Map(pods, func(p *v1.Pod, _ int) string { return client.ObjectKeyFromObject(p).String() }), 5)).
		With("duration", time.Since(start)).
		Infof("found provisionable pod(s)")
	results.Record(ctx, p.recorder, p.cluster)
	return results, nil
}

func (p *Provisioner) Create(ctx context.Context, n *scheduler.NodeClaim, opts ...functional.Option[LaunchOptions]) (string, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("nodepool", n.NodePoolName))
	options := functional.ResolveOptions(opts...)
	latest := &v1beta1.NodePool{}
	if err := p.kubeClient.Get(ctx, types.NamespacedName{Name: n.NodePoolName}, latest); err != nil {
		return "", fmt.Errorf("getting current resource usage, %w", err)
	}
	if err := latest.Spec.Limits.ExceededBy(latest.Status.Resources); err != nil {
		return "", err
	}
	nodeClaim := n.ToNodeClaim(latest)

	if err := p.kubeClient.Create(ctx, nodeClaim); err != nil {
		return "", err
	}
	instanceTypeRequirement, _ := lo.Find(nodeClaim.Spec.Requirements, func(req v1beta1.NodeSelectorRequirementWithMinValues) bool {
		return req.Key == v1.LabelInstanceTypeStable
	})
	logging.FromContext(ctx).With("nodeclaim", nodeClaim.Name, "requests", nodeClaim.Spec.Resources.Requests, "instance-types", instanceTypeList(instanceTypeRequirement.Values)).Infof("created nodeclaim")
	metrics.NodeClaimsCreatedCounter.With(prometheus.Labels{
		metrics.ReasonLabel:       options.Reason,
		metrics.NodePoolLabel:     nodeClaim.Labels[v1beta1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: nodeClaim.Labels[v1beta1.CapacityTypeLabelKey],
	}).Inc()
	// Update the nodeclaim manually in state to avoid evenutal consistency delay races with our watcher.
	// This is essential to avoiding races where disruption can create a replacement node, then immediately
	// requeue. This can race with controller-runtime's internal cache as it watches events on the cluster
	// to then trigger cluster state updates. Triggering it manually ensures that Karpenter waits for the
	// internal cache to sync before moving onto another disruption loop.
	p.cluster.UpdateNodeClaim(nodeClaim)
	if functional.ResolveOptions(opts...).RecordPodNomination {
		for _, pod := range n.Pods {
			p.recorder.Publish(scheduler.NominatePodEvent(pod, nil, nodeClaim))
		}
	}
	return nodeClaim.Name, nil
}

func instanceTypeList(names []string) string {
	var itSb strings.Builder
	for i, name := range names {
		// print the first 5 instance types only (indices 0-4)
		if i > 4 {
			lo.Must(fmt.Fprintf(&itSb, " and %d other(s)", len(names)-i))
			break
		} else if i > 0 {
			lo.Must(fmt.Fprint(&itSb, ", "))
		}
		lo.Must(fmt.Fprint(&itSb, name))
	}
	return itSb.String()
}

func (p *Provisioner) getDaemonSetPods(ctx context.Context) ([]*v1.Pod, error) {
	daemonSetList := &appsv1.DaemonSetList{}
	if err := p.kubeClient.List(ctx, daemonSetList); err != nil {
		return nil, fmt.Errorf("listing daemonsets, %w", err)
	}

	return lo.Map(daemonSetList.Items, func(d appsv1.DaemonSet, _ int) *v1.Pod {
		pod := p.cluster.GetDaemonSetPod(&d)
		if pod == nil {
			pod = &v1.Pod{Spec: d.Spec.Template.Spec}
		}
		// Replacing retrieved pod affinity with daemonset pod template required node affinity since this is overridden
		// by the daemonset controller during pod creation
		// https://github.com/kubernetes/kubernetes/blob/c5cf0ac1889f55ab51749798bec684aed876709d/pkg/controller/daemon/util/daemonset_util.go#L176
		if d.Spec.Template.Spec.Affinity != nil && d.Spec.Template.Spec.Affinity.NodeAffinity != nil && d.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			if pod.Spec.Affinity == nil {
				pod.Spec.Affinity = &v1.Affinity{}
			}
			if pod.Spec.Affinity.NodeAffinity == nil {
				pod.Spec.Affinity.NodeAffinity = &v1.NodeAffinity{}
			}
			pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = d.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		}
		return pod
	}), nil
}

func (p *Provisioner) Validate(ctx context.Context, pod *v1.Pod) error {
	return multierr.Combine(
		validateKarpenterManagedLabelCanExist(pod),
		validateNodeSelector(pod),
		validateAffinity(pod),
		p.volumeTopology.ValidatePersistentVolumeClaims(ctx, pod),
	)
}

// validateKarpenterManagedLabelCanExist provides a more clear error message in the event of scheduling a pod that specifically doesn't
// want to run on a Karpenter node (e.g. a Karpenter controller replica).
func validateKarpenterManagedLabelCanExist(p *v1.Pod) error {
	for _, req := range scheduling.NewPodRequirements(p) {
		if req.Key == v1beta1.NodePoolLabelKey && req.Operator() == v1.NodeSelectorOpDoesNotExist {
			return fmt.Errorf("configured to not run on a Karpenter provisioned node via the %s %s requirement",
				v1beta1.NodePoolLabelKey, v1.NodeSelectorOpDoesNotExist)
		}
	}
	return nil
}

func (p *Provisioner) injectVolumeTopologyRequirements(ctx context.Context, pods []*v1.Pod) []*v1.Pod {
	var schedulablePods []*v1.Pod
	for _, pod := range pods {
		if err := p.volumeTopology.Inject(ctx, pod); err != nil {
			logging.FromContext(ctx).With("pod", client.ObjectKeyFromObject(pod)).Errorf("getting volume topology requirements, %s", err)
		} else {
			schedulablePods = append(schedulablePods, pod)
		}
	}
	return schedulablePods
}

func validateNodeSelector(p *v1.Pod) (errs error) {
	terms := lo.MapToSlice(p.Spec.NodeSelector, func(k string, v string) v1.NodeSelectorTerm {
		return v1.NodeSelectorTerm{
			MatchExpressions: []v1.NodeSelectorRequirement{
				{
					Key:      k,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{v},
				},
			},
		}
	})
	for _, term := range terms {
		errs = multierr.Append(errs, validateNodeSelectorTerm(term))
	}
	return errs
}

func validateAffinity(p *v1.Pod) (errs error) {
	if p.Spec.Affinity == nil {
		return nil
	}
	if p.Spec.Affinity.NodeAffinity != nil {
		for _, term := range p.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			errs = multierr.Append(errs, validateNodeSelectorTerm(term.Preference))
		}
		if p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			for _, term := range p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
				errs = multierr.Append(errs, validateNodeSelectorTerm(term))
			}
		}
	}
	return errs
}

func validateNodeSelectorTerm(term v1.NodeSelectorTerm) (errs error) {
	if term.MatchFields != nil {
		errs = multierr.Append(errs, fmt.Errorf("node selector term with matchFields is not supported"))
	}
	if term.MatchExpressions != nil {
		for _, requirement := range term.MatchExpressions {
			errs = multierr.Append(errs, v1beta1.ValidateRequirement(v1beta1.NodeSelectorRequirementWithMinValues{
				NodeSelectorRequirement: requirement,
			}))
		}
	}
	return errs
}
