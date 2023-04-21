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

package provisioning

import (
	"context"
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
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/workqueue"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	schedulingevents "github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling/events"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/utils/functional"
	"github.com/aws/karpenter-core/pkg/utils/pretty"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	scheduler "github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/utils/pod"
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
	coreV1Client   corev1.CoreV1Interface
	batcher        *Batcher
	volumeTopology *scheduler.VolumeTopology
	cluster        *state.Cluster
	recorder       events.Recorder
	cm             *pretty.ChangeMonitor
}

func NewProvisioner(kubeClient client.Client, coreV1Client corev1.CoreV1Interface,
	recorder events.Recorder, cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster) *Provisioner {
	p := &Provisioner{
		batcher:        NewBatcher(),
		cloudProvider:  cloudProvider,
		kubeClient:     kubeClient,
		coreV1Client:   coreV1Client,
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
	if len(results.NewMachines) == 0 {
		return reconcile.Result{}, nil
	}
	_, err = p.LaunchMachines(ctx, results.NewMachines, WithReason(metrics.ProvisioningReason), RecordPodNomination)
	return reconcile.Result{}, err
}

// LaunchMachines launches nodes passed into the function in parallel. It returns a slice of the successfully created node
// names as well as a multierr of any errors that occurred while launching nodes
func (p *Provisioner) LaunchMachines(ctx context.Context, machines []*scheduler.Machine, opts ...functional.Option[LaunchOptions]) ([]string, error) {
	// Launch capacity and bind pods
	errs := make([]error, len(machines))
	machineNames := make([]string, len(machines))
	workqueue.ParallelizeUntil(ctx, len(machines), len(machines), func(i int) {
		// create a new context to avoid a data race on the ctx variable
		if machineName, err := p.Launch(ctx, machines[i], opts...); err != nil {
			errs[i] = fmt.Errorf("launching machine, %w", err)
		} else {
			machineNames[i] = machineName
		}
	})
	return machineNames, multierr.Combine(errs...)
}

func (p *Provisioner) GetPendingPods(ctx context.Context) ([]*v1.Pod, error) {
	var podList v1.PodList
	if err := p.kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": ""}); err != nil {
		return nil, fmt.Errorf("listing pods, %w", err)
	}
	var pods []*v1.Pod
	for i := range podList.Items {
		po := podList.Items[i]
		// filter for provisionable pods first so we don't check for validity/PVCs on pods we won't provision anyway
		// (e.g. those owned by daemonsets)
		if !pod.IsProvisionable(&po) {
			continue
		}
		if err := p.Validate(ctx, &po); err != nil {
			logging.FromContext(ctx).With("pod", client.ObjectKeyFromObject(&po)).Debugf("ignoring pod, %s", err)
			continue
		}

		p.consolidationWarnings(ctx, po)
		pods = append(pods, &po)
	}
	return pods, nil
}

// consolidationWarnings potentially writes logs warning about possible unexpected interactions between scheduling
// constraints and consolidation
func (p *Provisioner) consolidationWarnings(ctx context.Context, po v1.Pod) {
	// We have pending pods that have preferred anti-affinity or topology spread constraints.  These can interact
	// unexpectedly with consolidation so we warn once per hour when we see these pods.
	if po.Spec.Affinity != nil && po.Spec.Affinity.PodAntiAffinity != nil {
		if len(po.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 0 {
			if p.cm.HasChanged(string(po.UID), "pod-antiaffinity") {
				logging.FromContext(ctx).Infof("pod %s has a preferred Anti-Affinity which can prevent consolidation", client.ObjectKeyFromObject(&po))
			}
		}
	}
	for _, tsc := range po.Spec.TopologySpreadConstraints {
		if tsc.WhenUnsatisfiable == v1.ScheduleAnyway {
			if p.cm.HasChanged(string(po.UID), "pod-topology-spread") {
				logging.FromContext(ctx).Infof("pod %s has a preferred TopologySpreadConstraint which can prevent consolidation", client.ObjectKeyFromObject(&po))
			}
		}
	}
}

//nolint:gocyclo
func (p *Provisioner) NewScheduler(ctx context.Context, pods []*v1.Pod, stateNodes []*state.StateNode, opts scheduler.SchedulerOptions) (*scheduler.Scheduler, error) {
	// Build node templates
	var machines []*scheduler.MachineTemplate
	var provisionerList v1alpha5.ProvisionerList
	instanceTypes := map[string][]*cloudprovider.InstanceType{}
	domains := map[string]sets.String{}
	if err := p.kubeClient.List(ctx, &provisionerList); err != nil {
		return nil, fmt.Errorf("listing provisioners, %w", err)
	}

	// nodeTemplates generated from provisioners are ordered by weight
	// since they are stored within a slice and scheduling
	// will always attempt to schedule on the first nodeTemplate
	provisionerList.OrderByWeight()

	for i := range provisionerList.Items {
		provisioner := &provisionerList.Items[i]
		if !provisioner.DeletionTimestamp.IsZero() {
			continue
		}
		// Create node template
		machines = append(machines, scheduler.NewMachineTemplate(provisioner))
		// Get instance type options
		instanceTypeOptions, err := p.cloudProvider.GetInstanceTypes(ctx, provisioner)
		if err != nil {
			return nil, fmt.Errorf("getting instance types, %w", err)
		}
		instanceTypes[provisioner.Name] = append(instanceTypes[provisioner.Name], instanceTypeOptions...)

		// Construct Topology Domains
		for _, instanceType := range instanceTypeOptions {
			// We need to intersect the instance type requirements with the current provisioner requirements.  This
			// ensures that something like zones from an instance type don't expand the universe of valid domains.
			requirements := scheduling.NewNodeSelectorRequirements(provisioner.Spec.Requirements...)
			requirements.Add(instanceType.Requirements.Values()...)

			for key, requirement := range requirements {
				//This code used to execute a Union between domains[key] and requirement.Values().
				//The downside of this is that Union is immutable and takes a copy of the set it is executed upon.
				//This resulted in a lot of memory pressure on the heap and poor performance
				//https://github.com/aws/karpenter/issues/3565
				if domains[key] == nil {
					domains[key] = sets.NewString(requirement.Values()...)
				} else {
					domains[key].Insert(requirement.Values()...)
				}
			}
		}

		for key, requirement := range scheduling.NewNodeSelectorRequirements(provisioner.Spec.Requirements...) {
			if requirement.Operator() == v1.NodeSelectorOpIn {
				//The following is a performance optimisation, for the explanation see the comment above
				if domains[key] == nil {
					domains[key] = sets.NewString(requirement.Values()...)
				} else {
					domains[key].Insert(requirement.Values()...)
				}
			}
		}
	}

	if len(machines) == 0 {
		return nil, fmt.Errorf("no provisioners found")
	}

	// inject topology constraints
	pods = p.injectTopology(ctx, pods)

	// Calculate cluster topology
	topology, err := scheduler.NewTopology(ctx, p.kubeClient, p.cluster, domains, pods)
	if err != nil {
		return nil, fmt.Errorf("tracking topology counts, %w", err)
	}
	daemonSetPods, err := p.getDaemonSetPods(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting daemon pods, %w", err)
	}
	return scheduler.NewScheduler(ctx, p.kubeClient, machines, provisionerList.Items, p.cluster, stateNodes, topology, instanceTypes, daemonSetPods, p.recorder, opts), nil
}

func (p *Provisioner) Schedule(ctx context.Context) (*scheduler.Results, error) {
	defer metrics.Measure(schedulingDuration)()

	// We collect the nodes with their used capacities before we get the list of pending pods. This ensures that
	// the node capacities we schedule against are always >= what the actual capacity is at any given instance. This
	// prevents over-provisioning at the cost of potentially under-provisioning which will self-heal during the next
	// scheduling loop when we Launch a new node.  When this order is reversed, our node capacity may be reduced by pods
	// that have bound which we then provision new un-needed capacity for.
	// -------
	// We don't consider the nodes that are MarkedForDeletion since this capacity shouldn't be considered
	// as persistent capacity for the cluster (since it will soon be removed). Additionally, we are scheduling for
	// the pods that are on these nodes so the MarkedForDeletion node capacity can't be considered.
	nodes := p.cluster.Nodes()

	// Get pods, exit if nothing to do
	pendingPods, err := p.GetPendingPods(ctx)
	if err != nil {
		return nil, err
	}
	// Get pods from nodes that are preparing for deletion
	// We do this after getting the pending pods so that we undershoot if pods are
	// actively migrating from a node that is being deleted
	// NOTE: The assumption is that these nodes are cordoned and no additional pods will schedule to them
	deletingNodePods, err := nodes.Deleting().Pods(ctx, p.kubeClient)
	if err != nil {
		return nil, err
	}
	pods := append(pendingPods, deletingNodePods...)
	// nothing to schedule, so just return success
	if len(pods) == 0 {
		return &scheduler.Results{}, nil
	}
	scheduler, err := p.NewScheduler(ctx, pods, nodes.Active(), scheduler.SchedulerOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating scheduler, %w", err)
	}
	return scheduler.Solve(ctx, pods)
}

func (p *Provisioner) Launch(ctx context.Context, m *scheduler.Machine, opts ...functional.Option[LaunchOptions]) (string, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("provisioner", m.Labels[v1alpha5.ProvisionerNameLabelKey]))

	// Check limits
	latest := &v1alpha5.Provisioner{}
	if err := p.kubeClient.Get(ctx, types.NamespacedName{Name: m.ProvisionerName}, latest); err != nil {
		return "", fmt.Errorf("getting current resource usage, %w", err)
	}
	if err := latest.Spec.Limits.ExceededBy(latest.Status.Resources); err != nil {
		return "", err
	}
	options := functional.ResolveOptions(opts...)

	machine := m.ToMachine(latest)
	if err := p.kubeClient.Create(ctx, machine); err != nil {
		return "", err
	}
	instanceTypeRequirement, _ := lo.Find(machine.Spec.Requirements, func(req v1.NodeSelectorRequirement) bool { return req.Key == v1.LabelInstanceTypeStable })
	logging.FromContext(ctx).With("requests", machine.Spec.Resources.Requests, "instance-types", instanceTypeList(instanceTypeRequirement.Values)).Infof("created machine")
	p.cluster.NominateNodeForPod(ctx, machine.Name)
	metrics.MachinesCreatedCounter.With(prometheus.Labels{
		metrics.ReasonLabel:      options.Reason,
		metrics.ProvisionerLabel: machine.Labels[v1alpha5.ProvisionerNameLabelKey],
	}).Inc()
	if functional.ResolveOptions(opts...).RecordPodNomination {
		for _, pod := range m.Pods {
			p.recorder.Publish(schedulingevents.NominatePod(pod, nil, machine))
		}
	}
	return machine.Name, nil
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
		return pod
	}), nil
}

func (p *Provisioner) Validate(ctx context.Context, pod *v1.Pod) error {
	return multierr.Combine(
		validateProvisionerNameCanExist(pod),
		validateAffinity(pod),
		p.volumeTopology.ValidatePersistentVolumeClaims(ctx, pod),
	)
}

// validateProvisionerNameCanExist provides a more clear error message in the event of scheduling a pod that specifically doesn't
// want to run on a Karpenter node (e.g. a Karpenter controller replica).
func validateProvisionerNameCanExist(p *v1.Pod) error {
	for _, req := range scheduling.NewPodRequirements(p) {
		if req.Key == v1alpha5.ProvisionerNameLabelKey && req.Operator() == v1.NodeSelectorOpDoesNotExist {
			return fmt.Errorf("configured to not run on a Karpenter provisioned node via %s %s requirement",
				v1alpha5.ProvisionerNameLabelKey, v1.NodeSelectorOpDoesNotExist)
		}
	}
	return nil
}

func (p *Provisioner) injectTopology(ctx context.Context, pods []*v1.Pod) []*v1.Pod {
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
			errs = multierr.Append(errs, v1alpha5.ValidateRequirement(requirement))
		}
	}
	return errs
}
