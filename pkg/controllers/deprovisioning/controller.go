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

package deprovisioning

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/avast/retry-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/operator/controller"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	deprovisioningevents "github.com/aws/karpenter-core/pkg/controllers/deprovisioning/events"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	nodeutils "github.com/aws/karpenter-core/pkg/utils/node"
)

// Controller is the deprovisioning controller.
type Controller struct {
	kubeClient    client.Client
	cluster       *state.Cluster
	provisioner   *provisioning.Provisioner
	recorder      events.Recorder
	clock         clock.Clock
	cloudProvider cloudprovider.CloudProvider
	emptiness     *Emptiness
	expiration    *Expiration
	consolidation *Consolidation
}

// pollingPeriod that we inspect cluster to look for opportunities to deprovision
const pollingPeriod = 10 * time.Second

var errCandidateNodeDeleting = fmt.Errorf("candidate node is deleting")

// waitRetryOptions are the retry options used when waiting on a node to become ready or to be deleted
// readiness can take some time as the node needs to come up, have any daemonset extended resoruce plugins register, etc.
// deletion can take some time in the case of restrictive PDBs that throttle the rate at which the node is drained
var waitRetryOptions = []retry.Option{
	retry.Delay(2 * time.Second),
	retry.LastErrorOnly(true),
	retry.Attempts(60),
	retry.MaxDelay(10 * time.Second), // 22 + (60-5)*10 =~ 9.5 minutes in total
}

func NewController(clk clock.Clock, kubeClient client.Client, provisioner *provisioning.Provisioner,
	cp cloudprovider.CloudProvider, recorder events.Recorder, cluster *state.Cluster) *Controller {
	return &Controller{
		clock:         clk,
		kubeClient:    kubeClient,
		cluster:       cluster,
		provisioner:   provisioner,
		recorder:      recorder,
		cloudProvider: cp,
		expiration: &Expiration{
			kubeClient:  kubeClient,
			clock:       clk,
			cluster:     cluster,
			provisioner: provisioner,
		},
		emptiness: &Emptiness{
			kubeClient: kubeClient,
			clock:      clk,
			cluster:    cluster,
		},
		consolidation: &Consolidation{
			clock:         clk,
			kubeClient:    kubeClient,
			cluster:       cluster,
			provisioner:   provisioner,
			recorder:      recorder,
			cloudProvider: cp,
		},
	}
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(m).
		Named("deprovisioning")
}

func (c *Controller) LivenessProbe(_ *http.Request) error {
	return nil
}

func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	result, err := c.ProcessCluster(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("deprovisioning cluster, %w", err)
	} else if result == ResultRetry {
		return reconcile.Result{Requeue: true}, nil
	} else if result == ResultNothingToDo {
		c.consolidation.lastConsolidationState = c.cluster.ClusterConsolidationState()
	}
	return reconcile.Result{RequeueAfter: pollingPeriod}, nil
}

// CandidateNode is a node that we are considering for deprovisioning along with extra information to be used in
// making that determination
type CandidateNode struct {
	*v1.Node
	instanceType   cloudprovider.InstanceType
	capacityType   string
	zone           string
	provisioner    *v1alpha5.Provisioner
	disruptionCost float64
	pods           []*v1.Pod
}

// ProcessCluster is exposed for unit testing purposes
// ProcessCluster loops through implemented deprovisioners
func (c *Controller) ProcessCluster(ctx context.Context) (Result, error) {
	// range over the different deprovisioning methods. We'll only let one method perform an action
	for _, d := range []Deprovisioner{
		// Emptiness and Consolidation are mutually exclusive, so this loop only executes one deprovisioner
		c.emptiness,
		c.expiration,
		c.consolidation,
	} {
		var result Result
		var err error
		// Capture new cluster state before each attempt
		candidates, err := c.candidateNodes(ctx, d.ShouldDeprovision)
		if err != nil {
			return ResultFailed, fmt.Errorf("determining candidate nodes, %w", err)
		}
		// If there are no candidate nodes, move to the next deprovisioner
		if len(candidates) == 0 {
			continue
		}
		// Sort candidates by each deprovisioner's priority, and deprovision the first one possible
		candidates = d.SortCandidates(candidates)
		result, err = c.executeDeprovisioning(ctx, d, candidates...)
		if err != nil {
			logging.FromContext(ctx).Errorf("deprovisioning nodes, %s", err)
			return ResultFailed, err
		}
		if result == ResultRetry || result == ResultSuccess {
			return result, nil
		}
		// Continue to next deprovisioner if nothing was done
		if result == ResultNothingToDo {
			continue
		}
		return result, nil
	}
	// All deprovisioners did nothing, so return nothing to do
	return ResultNothingToDo, nil
}

func (c *Controller) executeDeprovisioning(ctx context.Context, d Deprovisioner, nodes ...CandidateNode) (Result, error) {
	// Given candidate nodes, compute best deprovisioning action
	attempts := 0
	var cmd Command
	var err error
	success := false
	// Each attempt will try at least one node, limit to that many attempts.
	for attempts < len(nodes) {
		cmd, err = d.ComputeCommand(ctx, attempts, nodes...)
		if err != nil {
			return ResultFailed, err
		}
		// Validate the command with a TTL
		ok, err := c.validateCommand(ctx, cmd, d)
		if err != nil {
			return ResultFailed, fmt.Errorf("validating command, %w", err)
		}
		// If validation succeeds, execute the command. Otherwise, we need to compute another command and
		// try the rest of the candidate nodes. If any of the remaining candidateNodes are still valid
		// for an action, continue with deprovisioning, even though we might not be taking the most optimal decision.
		// This is meant to stop an issue if another controller in the cluster coincidentally creates churn at the same interval.
		if ok {
			success = true
			break
		}
		attempts++
	}
	// Validation for all candidateNodes failed, so nothing can be done for this deprovisioner.
	if !success {
		return ResultNothingToDo, nil
	}
	// Execute the command
	result, err := c.executeCommand(ctx, cmd, d)
	if err != nil {
		return ResultFailed, err
	}
	return result, nil
}

func (c *Controller) validateCommand(ctx context.Context, cmd Command, d Deprovisioner) (bool, error) {
	// Only continue with the rest of the logic if there is something to deprovision
	if len(cmd.nodesToRemove) == 0 || !(cmd.action == actionDelete || cmd.action == actionReplace) {
		return false, nil
	}
	remainingDelay := d.TTL() - c.clock.Since(cmd.createdAt)
	// deprovisioningTTL is how long we wait before re-validating our lifecycle command
	if remainingDelay > 0 {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("context canceled during %s validation", d)
		case <-c.clock.After(remainingDelay):
		}
	}
	candidateNodes, err := c.candidateNodes(ctx, d.ShouldDeprovision)
	if err != nil {
		return false, fmt.Errorf("determining candidate node, %w", err)
	}
	// Get the nodesToRemove that are still deprovisionable after the delay
	nodes := mapNodes(cmd.nodesToRemove, candidateNodes)

	// None of the chosen candidate nodes are valid for execution, so retry
	if len(nodes) == 0 {
		return false, nil
	}

	// Validate the command with the (maybe new) list of candidate nodes
	ok, err := d.ValidateCommand(ctx, nodes, cmd)
	if err != nil {
		return false, fmt.Errorf("determining candidate node, %w", err)
	}
	if !ok {
		return false, nil
	}
	return true, nil
}

func (c *Controller) executeCommand(ctx context.Context, command Command, d Deprovisioner) (Result, error) {
	deprovisioningActionsPerformedCounter.With(prometheus.Labels{"action": fmt.Sprintf("%s/%s", d, command.action)}).Add(1)
	logging.FromContext(ctx).Infof("deprovisioning via %s/%s", d, command.action)

	if command.action == actionReplace {
		if err := c.launchReplacementNodes(ctx, command); err != nil {
			// If we failed to launch the replacement, don't deprovision.  If this is some permanent failure,
			// we don't want to disrupt workloads with no way to provision new nodes for them.
			return ResultFailed, fmt.Errorf("launching replacement node, %w", err)
		}
	}

	for _, oldNode := range command.nodesToRemove {
		c.recorder.Publish(deprovisioningevents.TerminatingNode(oldNode, command.String()))
		if err := c.kubeClient.Delete(ctx, oldNode); err != nil {
			logging.FromContext(ctx).Errorf("Deleting node, %s", err)
		} else {
			metrics.NodesTerminatedCounter.WithLabelValues(fmt.Sprintf("%s/%s", d, command.action)).Inc()
		}
	}

	// We wait for nodes to delete to ensure we don't start another round of deprovisioning until this node is fully
	// deleted.
	for _, oldnode := range command.nodesToRemove {
		c.waitForDeletion(ctx, oldnode)
	}
	return ResultSuccess, nil
}

// candidateNodes returns nodes that appear to be currently deprovisionable based off of their provisioner
// nolint:gocyclo
func (c *Controller) candidateNodes(ctx context.Context, shouldDeprovision func(context.Context, *state.Node, *v1alpha5.Provisioner, []*v1.Pod) bool) ([]CandidateNode, error) {
	provisioners, instanceTypesByProvisioner, err := c.buildProvisionerMap(ctx)
	if err != nil {
		return nil, err
	}

	var nodes []CandidateNode
	c.cluster.ForEachNode(func(n *state.Node) bool {
		ctx = logging.WithLogger(ctx, logging.FromContext(ctx).With("node", n.Node.Name))
		var provisioner *v1alpha5.Provisioner
		var instanceTypeMap map[string]cloudprovider.InstanceType
		if provName, ok := n.Node.Labels[v1alpha5.ProvisionerNameLabelKey]; ok {
			provisioner = provisioners[provName]
			instanceTypeMap = instanceTypesByProvisioner[provName]
		}
		// skip any nodes that are already marked for deletion and being handled
		if n.MarkedForDeletion {
			return true
		}
		// skip any nodes where we can't determine the provisioner
		if provisioner == nil || instanceTypeMap == nil {
			return true
		}

		instanceType := instanceTypeMap[n.Node.Labels[v1.LabelInstanceTypeStable]]
		// skip any nodes that we can't determine the instance of
		if instanceType == nil {
			return true
		}

		// skip any nodes that we can't determine the capacity type or the topology zone for
		ct, ok := n.Node.Labels[v1alpha5.LabelCapacityType]
		if !ok {
			return true
		}
		az, ok := n.Node.Labels[v1.LabelTopologyZone]
		if !ok {
			return true
		}

		// Skip nodes that aren't initialized
		if n.Node.Labels[v1alpha5.LabelNodeInitialized] != "true" {
			return true
		}

		// Skip the node if it is nominated by a recent provisioning pass to be the target of a pending pod.
		if c.cluster.IsNodeNominated(n.Node.Name) {
			return true
		}

		pods, err := nodeutils.GetNodePods(ctx, c.kubeClient, n.Node)
		if err != nil {
			logging.FromContext(ctx).Errorf("Determining node pods, %s", err)
			return true
		}

		if !shouldDeprovision(ctx, n, provisioner, pods) {
			return true
		}

		cn := CandidateNode{
			Node:           n.Node,
			instanceType:   instanceType,
			capacityType:   ct,
			zone:           az,
			provisioner:    provisioner,
			pods:           pods,
			disruptionCost: disruptionCost(ctx, pods),
		}
		// lifetimeRemaining is the fraction of node lifetime remaining in the range [0.0, 1.0].  If the TTLSecondsUntilExpired
		// is non-zero, we use it to scale down the disruption costs of nodes that are going to expire.  Just after creation, the
		// disruption cost is highest and it approaches zero as the node ages towards its expiration time.
		lifetimeRemaining := c.calculateLifetimeRemaining(cn)
		cn.disruptionCost *= lifetimeRemaining

		nodes = append(nodes, cn)
		return true
	})

	return nodes, nil
}

// buildProvisionerMap builds a provName -> provisioner map and a provName -> instanceName -> instance type map
func (c *Controller) buildProvisionerMap(ctx context.Context) (map[string]*v1alpha5.Provisioner, map[string]map[string]cloudprovider.InstanceType, error) {
	provisioners := map[string]*v1alpha5.Provisioner{}
	var provList v1alpha5.ProvisionerList
	if err := c.kubeClient.List(ctx, &provList); err != nil {
		return nil, nil, fmt.Errorf("listing provisioners, %w", err)
	}
	instanceTypesByProvisioner := map[string]map[string]cloudprovider.InstanceType{}
	for i := range provList.Items {
		p := &provList.Items[i]
		provisioners[p.Name] = p

		provInstanceTypes, err := c.cloudProvider.GetInstanceTypes(ctx, p)
		if err != nil {
			return nil, nil, fmt.Errorf("listing instance types for %s, %w", p.Name, err)
		}
		instanceTypesByProvisioner[p.Name] = map[string]cloudprovider.InstanceType{}
		for _, it := range provInstanceTypes {
			instanceTypesByProvisioner[p.Name][it.Name()] = it
		}
	}
	return provisioners, instanceTypesByProvisioner, nil
}

// waitForDeletion waits for the specified node to be removed from the API server. This deletion can take some period
// of time if there are PDBs that govern pods on the node as we need to  wait until the node drains before
// it's actually deleted.
func (c *Controller) waitForDeletion(ctx context.Context, node *v1.Node) {
	if err := retry.Do(func() error {
		var n v1.Node
		nerr := c.kubeClient.Get(ctx, client.ObjectKey{Name: node.Name}, &n)
		// We expect the not node found error, at which point we know the node is deleted.
		if errors.IsNotFound(nerr) {
			return nil
		}
		// make the user aware of why deprovisioning is paused
		c.recorder.Publish(deprovisioningevents.WaitingOnDeletion(node))
		if nerr != nil {
			return fmt.Errorf("expected node to be not found, %w", nerr)
		}
		// the node still exists
		return fmt.Errorf("expected node to be not found")
	}, waitRetryOptions...,
	); err != nil {
		logging.FromContext(ctx).Errorf("Waiting on node deletion, %s", err)
	}
}

// launchReplacementNodes launches replacement nodes and blocks until it is ready
// nolint:gocyclo
func (c *Controller) launchReplacementNodes(ctx context.Context, action Command) error {
	defer metrics.Measure(deprovisioningReplacementNodeInitializedHistogram)()
	nodeNamesToRemove := lo.Map(action.nodesToRemove, func(n *v1.Node, _ int) string { return n.Name })
	// cordon the old nodes before we launch the replacements to prevent new pods from scheduling to the old nodes
	if err := c.setNodesUnschedulable(ctx, true, nodeNamesToRemove...); err != nil {
		return fmt.Errorf("cordoning nodes, %w", err)
	}

	nodeNames, err := c.provisioner.LaunchNodes(ctx, provisioning.LaunchOptions{RecordPodNomination: false}, action.replacementNodes...)
	if err != nil {
		// uncordon the nodes as the launch may fail (e.g. ICE or incompatible AMI)
		err = multierr.Append(err, c.setNodesUnschedulable(ctx, false, nodeNamesToRemove...))
		return err
	}
	if len(nodeNames) != len(action.replacementNodes) {
		// shouldn't ever occur since a partially failed LaunchNodes should return an error
		return fmt.Errorf("expected %d node names, got %d", len(action.replacementNodes), len(nodeNames))
	}
	metrics.NodesCreatedCounter.WithLabelValues(metrics.DeprovisioningReason).Add(float64(len(nodeNames)))

	// We have the new nodes created at the API server so mark the old nodes for deletion
	c.cluster.MarkForDeletion(nodeNamesToRemove...)
	// Wait for nodes to be ready
	// TODO @njtran: Allow to bypass this check for certain deprovisioners
	errs := make([]error, len(nodeNames))
	workqueue.ParallelizeUntil(ctx, len(nodeNames), len(nodeNames), func(i int) {
		var k8Node v1.Node
		// Wait for the node to be ready
		var once sync.Once
		if err := retry.Do(func() error {
			if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: nodeNames[i]}, &k8Node); err != nil {
				return fmt.Errorf("getting node, %w", err)
			}
			once.Do(func() {
				c.recorder.Publish(deprovisioningevents.LaunchingNode(&k8Node, action.String()))
			})

			if _, ok := k8Node.Labels[v1alpha5.LabelNodeInitialized]; !ok {
				// make the user aware of why deprovisioning is paused
				c.recorder.Publish(deprovisioningevents.WaitingOnReadiness(&k8Node))
				return fmt.Errorf("node is not initialized")
			}
			return nil
		}, waitRetryOptions...); err != nil {
			// nodes never become ready, so uncordon the nodes we were trying to delete and report the error
			errs[i] = err
		}
	})
	multiErr := multierr.Combine(errs...)
	if multiErr != nil {
		c.cluster.UnmarkForDeletion(nodeNamesToRemove...)
		return multierr.Combine(c.setNodesUnschedulable(ctx, false, nodeNamesToRemove...),
			fmt.Errorf("timed out checking node readiness, %w", multiErr))
	}
	return nil
}

// calculateLifetimeRemaining calculates the fraction of node lifetime remaining in the range [0.0, 1.0].  If the TTLSecondsUntilExpired
// is non-zero, we use it to scale down the disruption costs of nodes that are going to expire.  Just after creation, the
// disruption cost is highest and it approaches zero as the node ages towards its expiration time.
func (c *Controller) calculateLifetimeRemaining(node CandidateNode) float64 {
	remaining := 1.0
	if node.provisioner.Spec.TTLSecondsUntilExpired != nil {
		ageInSeconds := c.clock.Since(node.CreationTimestamp.Time).Seconds()
		totalLifetimeSeconds := float64(*node.provisioner.Spec.TTLSecondsUntilExpired)
		lifetimeRemainingSeconds := totalLifetimeSeconds - ageInSeconds
		remaining = clamp(0.0, lifetimeRemainingSeconds/totalLifetimeSeconds, 1.0)
	}
	return remaining
}

func (c *Controller) setNodesUnschedulable(ctx context.Context, isUnschedulable bool, nodeNames ...string) error {
	var multiErr error
	for _, nodeName := range nodeNames {
		var node v1.Node
		if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			multiErr = multierr.Append(multiErr, fmt.Errorf("getting node, %w", err))
		}

		// node is being deleted already, so no need to un-cordon
		if !isUnschedulable && !node.DeletionTimestamp.IsZero() {
			continue
		}

		// already matches the state we want to be in
		if node.Spec.Unschedulable == isUnschedulable {
			continue
		}

		persisted := node.DeepCopy()
		node.Spec.Unschedulable = isUnschedulable
		if err := c.kubeClient.Patch(ctx, &node, client.MergeFrom(persisted)); err != nil {
			multiErr = multierr.Append(multiErr, fmt.Errorf("patching node %s, %w", node.Name, err))
		}
	}
	return multiErr
}
