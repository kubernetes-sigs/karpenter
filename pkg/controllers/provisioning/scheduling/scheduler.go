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

package scheduling

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/scheduling"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"
	"github.com/aws/karpenter-core/pkg/utils/pod"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

// SchedulerOptions can be used to control the scheduling, these options are currently only used during consolidation.
type SchedulerOptions struct {
	// SimulationMode if true will prevent recording of the pod nomination decisions as events
	SimulationMode bool
}

func NewScheduler(ctx context.Context, kubeClient client.Client, nodeClaimTemplates []*NodeClaimTemplate,
	nodePools []v1beta1.NodePool, cluster *state.Cluster, stateNodes []*state.StateNode, topology *Topology,
	instanceTypes map[nodepoolutil.Key][]*cloudprovider.InstanceType, daemonSetPods []*v1.Pod,
	recorder events.Recorder, opts SchedulerOptions) *Scheduler {

	// if any of the nodePools add a taint with a prefer no schedule effect, we add a toleration for the taint
	// during preference relaxation
	toleratePreferNoSchedule := false
	for _, prov := range nodePools {
		for _, taint := range prov.Spec.Template.Spec.Taints {
			if taint.Effect == v1.TaintEffectPreferNoSchedule {
				toleratePreferNoSchedule = true
			}
		}
	}

	s := &Scheduler{
		ctx:                ctx,
		kubeClient:         kubeClient,
		nodeClaimTemplates: nodeClaimTemplates,
		topology:           topology,
		cluster:            cluster,
		instanceTypes:      instanceTypes,
		daemonOverhead:     getDaemonOverhead(nodeClaimTemplates, daemonSetPods),
		recorder:           recorder,
		opts:               opts,
		preferences:        &Preferences{ToleratePreferNoSchedule: toleratePreferNoSchedule},
		remainingResources: map[nodepoolutil.Key]v1.ResourceList{},
	}
	for _, nodePool := range nodePools {
		s.remainingResources[nodepoolutil.Key{Name: nodePool.Name, IsProvisioner: nodePool.IsProvisioner}] = v1.ResourceList(nodePool.Spec.Limits)
	}
	s.calculateExistingNodeClaims(stateNodes, daemonSetPods)
	return s
}

type Scheduler struct {
	ctx                context.Context
	newNodeClaims      []*NodeClaim
	existingNodes      []*ExistingNode
	nodeClaimTemplates []*NodeClaimTemplate
	remainingResources map[nodepoolutil.Key]v1.ResourceList               // (NodePool name, isProvisioner) -> remaining resources for that NodePool
	instanceTypes      map[nodepoolutil.Key][]*cloudprovider.InstanceType // (NodePool name, isProvisioner) -> instance types for NodePool
	daemonOverhead     map[*NodeClaimTemplate]v1.ResourceList
	preferences        *Preferences
	topology           *Topology
	cluster            *state.Cluster
	recorder           events.Recorder
	opts               SchedulerOptions
	kubeClient         client.Client
}

// Results contains the results of the scheduling operation
type Results struct {
	NewNodeClaims []*NodeClaim
	ExistingNodes []*ExistingNode
	PodErrors     map[*v1.Pod]error
}

// AllNonPendingPodsScheduled returns true if all pods scheduled.
// We don't care if a pod was pending before consolidation and will still be pending after. It may be a pod that we can't
// schedule at all and don't want it to block consolidation.
func (r Results) AllNonPendingPodsScheduled() bool {
	return len(lo.OmitBy(r.PodErrors, func(p *v1.Pod, err error) bool {
		return pod.IsProvisionable(p)
	})) == 0
}

// NonPendingPodSchedulingErrors creates a string that describes why pods wouldn't schedule that is suitable for presentation
func (r Results) NonPendingPodSchedulingErrors() string {
	errs := lo.OmitBy(r.PodErrors, func(p *v1.Pod, err error) bool {
		return pod.IsProvisionable(p)
	})
	if len(errs) == 0 {
		return "No Pod Scheduling Errors"
	}
	var msg bytes.Buffer
	fmt.Fprintf(&msg, "not all pods would schedule, ")
	const MaxErrors = 5
	numErrors := 0
	for k, err := range errs {
		fmt.Fprintf(&msg, "%s/%s => %s ", k.Namespace, k.Name, err)
		numErrors++
		if numErrors >= MaxErrors {
			fmt.Fprintf(&msg, " and %d other(s)", len(errs)-MaxErrors)
			break
		}
	}
	return msg.String()
}

func (s *Scheduler) Solve(ctx context.Context, pods []*v1.Pod) *Results {
	defer metrics.Measure(schedulingSimulationDuration)()
	// We loop trying to schedule unschedulable pods as long as we are making progress.  This solves a few
	// issues including pods with affinity to another pod in the batch. We could topo-sort to solve this, but it wouldn't
	// solve the problem of scheduling pods where a particular order is needed to prevent a max-skew violation. E.g. if we
	// had 5xA pods and 5xB pods were they have a zonal topology spread, but A can only go in one zone and B in another.
	// We need to schedule them alternating, A, B, A, B, .... and this solution also solves that as well.
	errors := map[*v1.Pod]error{}
	q := NewQueue(pods...)
	for {
		// Try the next pod
		pod, ok := q.Pop()
		if !ok {
			break
		}

		// Schedule to existing nodes or create a new node
		if errors[pod] = s.add(ctx, pod); errors[pod] == nil {
			continue
		}

		// If unsuccessful, relax the pod and recompute topology
		relaxed := s.preferences.Relax(ctx, pod)
		q.Push(pod, relaxed)
		if relaxed {
			if err := s.topology.Update(ctx, pod); err != nil {
				logging.FromContext(ctx).Errorf("updating topology, %s", err)
			}
		}
	}

	for _, m := range s.newNodeClaims {
		m.FinalizeScheduling()
	}
	if !s.opts.SimulationMode {
		s.recordSchedulingResults(ctx, pods, q.List(), errors)
	}
	// clear any nil errors so we can know that len(PodErrors) == 0 => all pods scheduled
	// we can also clear any pod errors for pods that aren't provisionable since
	// 1. Provisioning only cares about the new nodes we need to provisionable
	// 2. Deprovisioning only cares about the pods on the candidates we're trying to delete.
	for k, v := range errors {
		if v == nil {
			delete(errors, k)
		}
	}
	return &Results{
		NewNodeClaims: s.newNodeClaims,
		ExistingNodes: s.existingNodes,
		PodErrors:     errors,
	}
}

func (s *Scheduler) recordSchedulingResults(ctx context.Context, pods []*v1.Pod, failedToSchedule []*v1.Pod, errors map[*v1.Pod]error) {
	// Report failures and nominations
	for _, pod := range failedToSchedule {
		logging.FromContext(ctx).With("pod", client.ObjectKeyFromObject(pod)).Errorf("Could not schedule pod, %s", errors[pod])
		s.recorder.Publish(PodFailedToScheduleEvent(pod, errors[pod]))
	}

	for _, existing := range s.existingNodes {
		if len(existing.Pods) > 0 {
			s.cluster.NominateNodeForPod(ctx, existing.ProviderID())
		}
		for _, pod := range existing.Pods {
			s.recorder.Publish(NominatePodEvent(pod, existing.Node, existing.NodeClaim))
		}
	}

	// Report new nodes, or exit to avoid log spam
	newCount := 0
	for _, nodeClaim := range s.newNodeClaims {
		newCount += len(nodeClaim.Pods)
	}
	if newCount == 0 {
		return
	}
	logging.FromContext(ctx).With("pods", len(pods)).Infof("found provisionable pod(s)")
	logging.FromContext(ctx).With("machines", len(s.newNodeClaims), "pods", newCount).Infof("computed new machine(s) to fit pod(s)")
	// Report in flight newNodes, or exit to avoid log spam
	inflightCount := 0
	existingCount := 0
	for _, node := range lo.Filter(s.existingNodes, func(node *ExistingNode, _ int) bool { return len(node.Pods) > 0 }) {
		inflightCount++
		existingCount += len(node.Pods)
	}
	if existingCount == 0 {
		return
	}
	logging.FromContext(ctx).Infof("computed %d unready node(s) will fit %d pod(s)", inflightCount, existingCount)
}

func (s *Scheduler) add(ctx context.Context, pod *v1.Pod) error {
	// first try to schedule against an in-flight real node
	for _, node := range s.existingNodes {
		if err := node.Add(ctx, s.kubeClient, pod); err == nil {
			return nil
		}
	}

	// Consider using https://pkg.go.dev/container/heap
	sort.Slice(s.newNodeClaims, func(a, b int) bool { return len(s.newNodeClaims[a].Pods) < len(s.newNodeClaims[b].Pods) })

	// Pick existing node that we are about to create
	for _, nodeClaim := range s.newNodeClaims {
		if err := nodeClaim.Add(pod); err == nil {
			return nil
		}
	}

	// Create new node
	var errs error
	for _, nodeClaimTemplate := range s.nodeClaimTemplates {
		instanceTypes := s.instanceTypes[nodeClaimTemplate.OwnerKey]
		// if limits have been applied to the provisioner, ensure we filter instance types to avoid violating those limits
		if remaining, ok := s.remainingResources[nodeClaimTemplate.OwnerKey]; ok {
			instanceTypes = filterByRemainingResources(s.instanceTypes[nodeClaimTemplate.OwnerKey], remaining)
			if len(instanceTypes) == 0 {
				errs = multierr.Append(errs, fmt.Errorf("all available instance types exceed limits for %s: %q", nodeClaimTemplate.OwnerKind(), nodeClaimTemplate.OwnerKey.Name))
				continue
			} else if len(s.instanceTypes[nodeClaimTemplate.OwnerKey]) != len(instanceTypes) && !s.opts.SimulationMode {
				logging.FromContext(ctx).With(nodeClaimTemplate.OwnerKind(), nodeClaimTemplate.OwnerKey.Name).Debugf("%d out of %d instance types were excluded because they would breach limits",
					len(s.instanceTypes[nodeClaimTemplate.OwnerKey])-len(instanceTypes), len(s.instanceTypes[nodeClaimTemplate.OwnerKey]))
			}
		}
		nodeClaim := NewNodeClaim(nodeClaimTemplate, s.topology, s.daemonOverhead[nodeClaimTemplate], instanceTypes)
		if err := nodeClaim.Add(pod); err != nil {
			errs = multierr.Append(errs, fmt.Errorf("incompatible with %s %q, daemonset overhead=%s, %w",
				nodeClaimTemplate.OwnerKind(),
				nodeClaimTemplate.OwnerKey.Name,
				resources.String(s.daemonOverhead[nodeClaimTemplate]),
				err))
			continue
		}
		// we will launch this nodeClaim and need to track its maximum possible resource usage against our remaining resources
		s.newNodeClaims = append(s.newNodeClaims, nodeClaim)
		s.remainingResources[nodeClaimTemplate.OwnerKey] = subtractMax(s.remainingResources[nodeClaimTemplate.OwnerKey], nodeClaim.InstanceTypeOptions)
		return nil
	}
	return errs
}

func (s *Scheduler) calculateExistingNodeClaims(stateNodes []*state.StateNode, daemonSetPods []*v1.Pod) {
	// create our existing nodes
	for _, node := range stateNodes {
		// Calculate any daemonsets that should schedule to the inflight node
		var daemons []*v1.Pod
		for _, p := range daemonSetPods {
			if err := scheduling.Taints(node.Taints()).Tolerates(p); err != nil {
				continue
			}
			if err := scheduling.NewLabelRequirements(node.Labels()).StrictlyCompatible(scheduling.NewPodRequirements(p)); err != nil {
				continue
			}
			daemons = append(daemons, p)
		}
		s.existingNodes = append(s.existingNodes, NewExistingNode(node, s.topology, resources.RequestsForPods(daemons...)))

		// We don't use the status field and instead recompute the remaining resources to ensure we have a consistent view
		// of the cluster during scheduling.  Depending on how node creation falls out, this will also work for cases where
		// we don't create NodeClaim resources.
		if _, ok := s.remainingResources[node.OwnerKey()]; ok {
			s.remainingResources[node.OwnerKey()] = resources.Subtract(s.remainingResources[node.OwnerKey()], node.Capacity())
		}
	}
	// Order the existing nodes for scheduling with initialized nodes first
	// This is done specifically for consolidation where we want to make sure we schedule to initialized nodes
	// before we attempt to schedule un-initialized ones
	sort.SliceStable(s.existingNodes, func(i, j int) bool {
		if s.existingNodes[i].Initialized() && !s.existingNodes[j].Initialized() {
			return true
		}
		if !s.existingNodes[i].Initialized() && s.existingNodes[j].Initialized() {
			return false
		}
		return s.existingNodes[i].Name() < s.existingNodes[j].Name()
	})
}

func getDaemonOverhead(nodeClaimTemplates []*NodeClaimTemplate, daemonSetPods []*v1.Pod) map[*NodeClaimTemplate]v1.ResourceList {
	overhead := map[*NodeClaimTemplate]v1.ResourceList{}

	for _, nodeClaimTemplate := range nodeClaimTemplates {
		var daemons []*v1.Pod
		for _, p := range daemonSetPods {
			if err := scheduling.Taints(nodeClaimTemplate.Spec.Taints).Tolerates(p); err != nil {
				continue
			}
			if err := nodeClaimTemplate.Requirements.Compatible(scheduling.NewPodRequirements(p)); err != nil {
				continue
			}
			daemons = append(daemons, p)
		}
		overhead[nodeClaimTemplate] = resources.RequestsForPods(daemons...)
	}
	return overhead
}

// subtractMax returns the remaining resources after subtracting the max resource quantity per instance type. To avoid
// overshooting out, we need to pessimistically assume that if e.g. we request a 2, 4 or 8 CPU instance type
// that the 8 CPU instance type is all that will be available.  This could cause a batch of pods to take multiple rounds
// to schedule.
func subtractMax(remaining v1.ResourceList, instanceTypes []*cloudprovider.InstanceType) v1.ResourceList {
	// shouldn't occur, but to be safe
	if len(instanceTypes) == 0 {
		return remaining
	}
	var allInstanceResources []v1.ResourceList
	for _, it := range instanceTypes {
		allInstanceResources = append(allInstanceResources, it.Capacity)
	}
	result := v1.ResourceList{}
	itResources := resources.MaxResources(allInstanceResources...)
	for k, v := range remaining {
		cp := v.DeepCopy()
		cp.Sub(itResources[k])
		result[k] = cp
	}
	return result
}

// filterByRemainingResources is used to filter out instance types that if launched would exceed the provisioner limits
func filterByRemainingResources(instanceTypes []*cloudprovider.InstanceType, remaining v1.ResourceList) []*cloudprovider.InstanceType {
	var filtered []*cloudprovider.InstanceType
	for _, it := range instanceTypes {
		itResources := it.Capacity
		viableInstance := true
		for resourceName, remainingQuantity := range remaining {
			// if the instance capacity is greater than the remaining quantity for this resource
			if resources.Cmp(itResources[resourceName], remainingQuantity) > 0 {
				viableInstance = false
			}
		}
		if viableInstance {
			filtered = append(filtered, it)
		}
	}
	return filtered
}
