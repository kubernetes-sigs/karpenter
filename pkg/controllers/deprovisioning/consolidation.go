package deprovisioning

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	pscheduling "github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/scheduling"
	nodeutils "github.com/aws/karpenter-core/pkg/utils/node"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Controller is the consolidation controller.  It is not a standard controller-runtime controller in that it doesn't
// have a reconcile method.
type Consolidation struct {
	kubeClient             client.Client
	cluster                *state.Cluster
	provisioner            *provisioning.Provisioner
	recorder               events.Recorder
	clock                  clock.Clock
	cloudProvider          cloudprovider.CloudProvider
	lastConsolidationState int64
}

const consolidationName = "consolidation"

func NewConsolidation(clk clock.Clock, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cp cloudprovider.CloudProvider, recorder events.Recorder, cluster *state.Cluster) *Consolidation {
	return &Consolidation{
		clock:         clk,
		kubeClient:    kubeClient,
		cluster:       cluster,
		provisioner:   provisioner,
		recorder:      recorder,
		cloudProvider: cp,
	}
}

// Skip nodes with consolidation disabled or are annotated as do-not-consolidate
func (c *Consolidation) shouldNotBeDeprovisioned(ctx context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, pods []*v1.Pod) bool {
	return provisioner == nil || provisioner.Spec.Consolidation == nil || !ptr.BoolValue(provisioner.Spec.Consolidation.Enabled) ||
		n.Node.Annotations[v1alpha5.DoNotConsolidateNodeAnnotationKey] == "true"
}

func (c *Consolidation) sortCandidates(nodes []candidateNode) []candidateNode {
	sort.Slice(nodes, func(i int, j int) bool {
		return nodes[i].disruptionCost < nodes[j].disruptionCost
	})
	return nodes
}

func (c *Consolidation) validateCommand(ctx context.Context, cmd DeprovisioningCommand) (bool, error) {
	remainingDelay := deprovisioningTTL - c.clock.Since(cmd.created)
	if remainingDelay > 0 {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("context canceled")
		case <-c.clock.After(remainingDelay):
		}
	}
	// deletion is just a special case of replace where we don't launch a replacement node
	if cmd.action == deprovisioningActionDeleteConsolidation || cmd.action == deprovisioningActionReplaceConsolidation {
		// c.validateReplace(ctx, nodes, cmd.replacementNode)
	} else {
		return false, fmt.Errorf("unsupported action for consolidation %s", cmd.action)
	}

	// switch cmd.action {
	// // case deprovisioningActionDeleteEmpty:
	// // 	// delete empty isn't quite a special case of replace as we only want to perform the action if the nodes are
	// // 	// empty, not if they just won't create a new node if deleted
	// // 	return c.validateDeleteEmpty(nodes)
	// case deprovisioningActionDeleteConsolidation, deprovisioningActionReplaceConsolidation:
	// 	// deletion is just a special case of replace where we don't launch a replacement node
	// 	return c.validateReplace(ctx, nodes, cmd.replacementNode)
	// default:
	// 	return false, fmt.Errorf("unsupported action %s", cmd.action)
	// }
}

func (c *Consolidation)	isExecutableAction(action deprovisioningAction) bool {
	return action == deprovisioningActionDeleteConsolidation || action == deprovisioningActionReplaceConsolidation
}

// mapNodes maps from a list of *v1.Node to candidateNode
func (c *Consolidation) mapNodes(nodes []*v1.Node, candidateNodes []candidateNode) ([]candidateNode, error) {
	verifyNodeNames := sets.NewString(lo.Map(nodes, func(t *v1.Node, i int) string { return t.Name })...)
	var ret []candidateNode
	for _, c := range candidateNodes {
		if verifyNodeNames.Has(c.Name) {
			ret = append(ret, c)
		}
	}
	return ret, nil
}

func (c *Consolidation) computeCommand(ctx context.Context, node candidateNode) (DeprovisioningCommand, error) {
	defer metrics.Measure(consolidationDurationHistogram.WithLabelValues("Replace/Delete"))()
	newNodes, allPodsScheduled, err := c.simulateScheduling(ctx, node)
	if err != nil {
		// if a candidate node is now deleting, just retry
		if errors.Is(err, errCandidateNodeDeleting) {
			return DeprovisioningCommand{action: deprovisioningActionDoNothing}, nil
		}
		return DeprovisioningCommand{}, err
	}

	// if not all of the pods were scheduled, we can't do anything
	if !allPodsScheduled {
		return DeprovisioningCommand{action: deprovisioningActionNotPossible}, nil
	}

	// were we able to schedule all the pods on the inflight nodes?
	if len(newNodes) == 0 {
		return DeprovisioningCommand{
			nodesToRemove: []*v1.Node{node.Node},
			action:        deprovisioningActionDeleteConsolidation,
			created:       c.clock.Now(),
		}, nil
	}

	// we're not going to turn a single node into multiple nodes
	if len(newNodes) != 1 {
		return DeprovisioningCommand{action: deprovisioningActionNotPossible}, nil
	}

	// get the current node price based on the offering
	// fallback if we can't find the specific zonal pricing data
	offering, ok := cloudprovider.GetOffering(node.instanceType, node.capacityType, node.zone)
	if !ok {
		return DeprovisioningCommand{action: deprovisioningActionFailed}, fmt.Errorf("getting offering price from candidate node, %w", err)
	}
	newNodes[0].InstanceTypeOptions = filterByPrice(newNodes[0].InstanceTypeOptions, newNodes[0].Requirements, offering.Price)
	if len(newNodes[0].InstanceTypeOptions) == 0 {
		// no instance types remain after filtering by price
		return DeprovisioningCommand{action: deprovisioningActionNotPossible}, nil
	}

	// If the existing node is spot and the replacement is spot, we don't consolidate.  We don't have a reliable
	// mechanism to determine if this replacement makes sense given instance type availability (e.g. we may replace
	// a spot node with one that is less available and more likely to be reclaimed).
	if node.capacityType == v1alpha5.CapacityTypeSpot &&
		newNodes[0].Requirements.Get(v1alpha5.LabelCapacityType).Has(v1alpha5.CapacityTypeSpot) {
		return DeprovisioningCommand{action: deprovisioningActionNotPossible}, nil
	}

	// We are consolidating a node from OD -> [OD,Spot] but have filtered the instance types by cost based on the
	// assumption, that the spot variant will launch. We also need to add a requirement to the node to ensure that if
	// spot capacity is insufficient we don't replace the node with a more expensive on-demand node.  Instead the launch
	// should fail and we'll just leave the node alone.
	ctReq := newNodes[0].Requirements.Get(v1alpha5.LabelCapacityType)
	if ctReq.Has(v1alpha5.CapacityTypeSpot) && ctReq.Has(v1alpha5.CapacityTypeOnDemand) {
		newNodes[0].Requirements.Add(scheduling.NewRequirement(v1alpha5.LabelCapacityType, v1.NodeSelectorOpIn, v1alpha5.CapacityTypeSpot))
	}

	return DeprovisioningCommand{
		nodesToRemove:   []*v1.Node{node.Node},
		action:          deprovisioningActionReplaceConsolidation,
		replacementNode: newNodes[0],
		created:         c.clock.Now(),
	}, nil
}

func (c *Consolidation) simulateScheduling(ctx context.Context, nodesToDelete ...candidateNode) (newNodes []*pscheduling.Node, allPodsScheduled bool, err error) {
	var stateNodes []*state.Node
	var markedForDeletionNodes []*state.Node
	candidateNodeIsDeleting := false
	candidateNodeNames := sets.NewString(lo.Map(nodesToDelete, func(t candidateNode, i int) string { return t.Name })...)
	c.cluster.ForEachNode(func(n *state.Node) bool {
		// not a candidate node
		if _, ok := candidateNodeNames[n.Node.Name]; !ok {
			if !n.MarkedForDeletion {
				stateNodes = append(stateNodes, n.DeepCopy())
			} else {
				markedForDeletionNodes = append(markedForDeletionNodes, n.DeepCopy())
			}
		} else if n.MarkedForDeletion {
			// candidate node and marked for deletion
			candidateNodeIsDeleting = true
		}
		return true
	})
	// We do one final check to ensure that the node that we are attempting to consolidate isn't
	// already handled for deletion by some other controller. This could happen if the node was markedForDeletion
	// between returning the candidateNodes and getting the stateNodes above
	if candidateNodeIsDeleting {
		return nil, false, errCandidateNodeDeleting
	}

	// We get the pods that are on nodes that are deleting
	deletingNodePods, err := nodeutils.GetNodePods(ctx, c.kubeClient, lo.Map(markedForDeletionNodes, func(n *state.Node, _ int) *v1.Node { return n.Node })...)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get pods from deleting nodes, %w", err)
	}
	var pods []*v1.Pod
	for _, n := range nodesToDelete {
		pods = append(pods, n.pods...)
	}
	pods = append(pods, deletingNodePods...)
	scheduler, err := c.provisioner.NewScheduler(ctx, pods, stateNodes, pscheduling.SchedulerOptions{
		SimulationMode: true,
	})

	if err != nil {
		return nil, false, fmt.Errorf("creating scheduler, %w", err)
	}

	newNodes, ifn, err := scheduler.Solve(ctx, pods)
	if err != nil {
		return nil, false, fmt.Errorf("simulating scheduling, %w", err)
	}

	podsScheduled := 0
	for _, n := range newNodes {
		podsScheduled += len(n.Pods)
	}
	for _, n := range ifn {
		podsScheduled += len(n.Pods)
	}

	return newNodes, podsScheduled == len(pods), nil
}

func (c *Consolidation) string() string {
	return consolidationName
}



	// func (c *Consolidation) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// 	// the last cluster consolidation wasn't able to improve things and nothing has changed regarding
	// 	// the cluster that makes us think we would be successful now
	// 	if c.lastConsolidationState == c.cluster.ClusterConsolidationState() {
	// 		return reconcile.Result{}, nil
	// 	}

	// 	clusterState := c.cluster.ClusterConsolidationState()
	// 	result, err := c.ProcessCluster(ctx)
	// 	if err != nil {
	// 		logging.FromContext(ctx).Errorf("consolidating cluster, %s", err)
	// 	} else if result == DeprovisioningResultNothingToDo {
	// 		c.lastConsolidationState = clusterState
	// 	}

	// 	return reconcile.Result{}, nil
	// }
