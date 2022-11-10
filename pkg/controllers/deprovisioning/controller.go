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
)

// Controller is the deprovisioning controller.
type Controller struct {
	kubeClient              client.Client
	cluster                 *state.Cluster
	provisioner             *provisioning.Provisioner
	recorder                events.Recorder
	clock                   clock.Clock
	cloudProvider           cloudprovider.CloudProvider
	emptiness               *Emptiness
	expiration              *Expiration
	singleNodeConsolidation *SingleNodeConsolidation
	multiNodeConsolidation  *MultiNodeConsolidation
	emptyNodeConsolidation  *EmptyNodeConsolidation
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
		clock:                   clk,
		kubeClient:              kubeClient,
		cluster:                 cluster,
		provisioner:             provisioner,
		recorder:                recorder,
		cloudProvider:           cp,
		expiration:              NewExpiration(clk, kubeClient, cluster, provisioner),
		emptiness:               NewEmptiness(clk, kubeClient, cluster),
		emptyNodeConsolidation:  NewEmptyNodeConsolidation(clk, cluster, kubeClient, provisioner, cp),
		multiNodeConsolidation:  NewMultiNodeConsolidation(clk, cluster, kubeClient, provisioner, cp),
		singleNodeConsolidation: NewSingleNodeConsolidation(clk, cluster, kubeClient, provisioner, cp),
	}
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(m).
		Named("deprovisioning")
}

func (c *Controller) LivenessProbe(_ *http.Request) error {
	return nil
}

func (c *Controller) Reconcile(ctx context.Context, r reconcile.Request) (reconcile.Result, error) {
	// capture the state of the cluster before we do any analysis
	currentState := c.cluster.ClusterConsolidationState()
	result, err := c.ProcessCluster(ctx)

	switch result {
	case ResultFailed:
		return reconcile.Result{}, fmt.Errorf("processing cluster, %w", err)
	case ResultRetry:
		return reconcile.Result{Requeue: true}, nil
	case ResultNothingToDo:
		// we record the cluster state for consolidation methods as they are expensive to compute and this allows
		// them to defer calculations until something about the cluster has changed that may allow them to
		// succeed
		c.emptyNodeConsolidation.RecordLastState(currentState)
		c.singleNodeConsolidation.RecordLastState(currentState)
		c.multiNodeConsolidation.RecordLastState(currentState)
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
		// Expire any nodes that must be deleted, allowing their pods to potentially land on currently
		// empty nodes
		c.expiration,

		// Delete any remaining empty nodes as there is zero cost in terms of dirsuption.  Emptiness and
		// emptyNodeConsolidation are mutually exclusive, only one of these will operate
		c.emptiness,
		c.emptyNodeConsolidation,

		// Attempt to identify multiple nodes that we can consolidate simultaneously to reduce pod churn
		c.multiNodeConsolidation,

		// And finally fall back our single node consolidation to further reduce cluster cost.
		c.singleNodeConsolidation,
	} {
		candidates, err := candidateNodes(ctx, c.cluster, c.kubeClient, c.clock, c.cloudProvider, d.ShouldDeprovision)
		if err != nil {
			return ResultFailed, fmt.Errorf("determining candidate nodes, %w", err)
		}
		// If there are no candidate nodes, move to the next deprovisioner
		if len(candidates) == 0 {
			continue
		}

		result, err := c.executeDeprovisioning(ctx, d, candidates...)
		if err != nil {
			return ResultFailed, fmt.Errorf("deprovisioning nodes, %w", err)
		}

		switch result {
		case ResultFailed:
			return ResultFailed, err
		case ResultRetry, ResultSuccess:
			// the controller wants to retry, or was successful in deprovisioning
			return result, nil
		case ResultNothingToDo:
			// found nothing to do, so try the next deprovisioner
			continue
		default:
			logging.FromContext(ctx).Errorf("unexpected result %s", result)
		}
	}

	// All deprovisioners did nothing, so return nothing to do
	return ResultNothingToDo, nil
}

// Given candidate nodes, compute best deprovisioning action
func (c *Controller) executeDeprovisioning(ctx context.Context, d Deprovisioner, nodes ...CandidateNode) (Result, error) {
	// Each attempt will try at least one node, limit to that many attempts.
	cmd, err := d.ComputeCommand(ctx, nodes...)
	if err != nil {
		return ResultFailed, err
	}
	// Convert action to result
	switch cmd.action {
	case actionFailed:
		return ResultFailed, err
	case actionDoNothing:
		return ResultNothingToDo, nil
	case actionRetry:
		return ResultRetry, nil
	}
	// If delete or replace, execute command
	result, err := c.executeCommand(ctx, cmd, d)
	if err != nil {
		return ResultFailed, err
	}
	return result, nil
}

func (c *Controller) executeCommand(ctx context.Context, command Command, d Deprovisioner) (Result, error) {
	deprovisioningActionsPerformedCounter.With(prometheus.Labels{"action": fmt.Sprintf("%s/%s", d, command.action)}).Add(1)
	logging.FromContext(ctx).Infof("deprovisioning via %s %s", d, command)

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
