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

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	deprovisioningevents "github.com/aws/karpenter-core/pkg/controllers/deprovisioning/events"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/operator/controller"
)

// Controller is the deprovisioning controller.
type Controller struct {
	kubeClient     client.Client
	cluster        *state.Cluster
	provisioner    *provisioning.Provisioner
	recorder       events.Recorder
	clock          clock.Clock
	cloudProvider  cloudprovider.CloudProvider
	deprovisioners []Deprovisioner
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
		deprovisioners: []Deprovisioner{
			// Expire any nodes that must be deleted, allowing their pods to potentially land on currently
			NewExpiration(clk, kubeClient, cluster, provisioner, recorder),
			// Terminate any nodes that have drifted from provisioning specifications, allowing the pods to reschedule.
			NewDrift(kubeClient, cluster, provisioner, recorder),
			// Delete any remaining empty nodes as there is zero cost in terms of dirsuption.  Emptiness and
			// emptyNodeConsolidation are mutually exclusive, only one of these will operate
			NewEmptiness(clk),
			NewEmptyNodeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder),
			// Attempt to identify multiple nodes that we can consolidate simultaneously to reduce pod churn
			NewMultiNodeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder),
			// And finally fall back our single node consolidation to further reduce cluster cost.
			NewSingleNodeConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder),
		},
	}
}

func (c *Controller) Name() string {
	return "deprovisioning"
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) controller.Builder {
	return controller.NewSingletonManagedBy(m)
}

func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	if !c.cluster.Synced(ctx) {
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	// Attempt different deprovisioning methods. We'll only let one method perform an action
	isConsolidated := c.cluster.Consolidated()
	for _, d := range c.deprovisioners {
		candidates, err := candidateNodes(ctx, c.cluster, c.kubeClient, c.clock, c.cloudProvider, d.ShouldDeprovision)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("determining candidate nodes, %w", err)
		}
		// If there are no candidate nodes, move to the next deprovisioner
		if len(candidates) == 0 {
			continue
		}

		// Determine the deprovisioning action
		cmd, err := d.ComputeCommand(ctx, candidates...)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("computing deprovisioning decision, %w", err)
		}
		if cmd.action == actionDoNothing {
			continue
		}
		if cmd.action == actionRetry {
			return reconcile.Result{Requeue: true}, nil
		}

		// Attempt to deprovision
		if err := c.executeCommand(ctx, d, cmd); err != nil {
			return reconcile.Result{}, fmt.Errorf("deprovisioning nodes, %w", err)
		}
		return reconcile.Result{Requeue: true}, nil
	}

	if !isConsolidated {
		// Mark cluster as consolidated, only if the deprovisioners ran and were not able to perform any work.
		c.cluster.SetConsolidated(true)
	}
	// All deprovisioners did nothing, so return nothing to do
	return reconcile.Result{RequeueAfter: pollingPeriod}, nil
}

func (c *Controller) executeCommand(ctx context.Context, d Deprovisioner, command Command) error {
	deprovisioningActionsPerformedCounter.With(prometheus.Labels{"action": fmt.Sprintf("%s/%s", d, command.action)}).Add(1)
	logging.FromContext(ctx).Infof("deprovisioning via %s %s", d, command)

	reason := fmt.Sprintf("%s/%s", d, command.action)
	if command.action == actionReplace {
		if err := c.launchReplacementMachines(ctx, command, reason); err != nil {
			// If we failed to launch the replacement, don't deprovision.  If this is some permanent failure,
			// we don't want to disrupt workloads with no way to provision new nodes for them.
			return fmt.Errorf("launching replacement node, %w", err)
		}
	}

	for _, oldNode := range command.nodesToRemove {
		c.recorder.Publish(deprovisioningevents.TerminatingMachine(oldNode.Machine, command.String()))
		if err := c.kubeClient.Delete(ctx, oldNode.Machine); client.IgnoreNotFound(err) != nil {
			logging.FromContext(ctx).Errorf("Deleting machine, %s", err)
		} else {
			metrics.MachinesTerminatedCounter.With(prometheus.Labels{
				metrics.ReasonLabel:      reason,
				metrics.ProvisionerLabel: oldNode.Labels()[v1alpha5.ProvisionerNameLabelKey],
			}).Inc()
		}
	}

	// We wait for nodes to delete to ensure we don't start another round of deprovisioning until this node is fully
	// deleted.
	for _, oldnode := range command.nodesToRemove {
		c.waitForDeletion(ctx, oldnode.Machine)
	}
	return nil
}

// waitForDeletion waits for the specified node to be removed from the API server. This deletion can take some period
// of time if there are PDBs that govern pods on the node as we need to wait until the node drains before
// it's actually deleted.
func (c *Controller) waitForDeletion(ctx context.Context, machine *v1alpha5.Machine) {
	if err := retry.Do(func() error {
		m := &v1alpha5.Machine{}
		nerr := c.kubeClient.Get(ctx, client.ObjectKeyFromObject(machine), m)
		// We expect the not node found error, at which point we know the node is deleted.
		if errors.IsNotFound(nerr) {
			return nil
		}
		// make the user aware of why deprovisioning is paused
		c.recorder.Publish(deprovisioningevents.WaitingOnDeletion(machine))
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

// launchReplacementMachines launches replacement machines and blocks until it is ready
// nolint:gocyclo
func (c *Controller) launchReplacementMachines(ctx context.Context, action Command, reason string) error {
	defer metrics.Measure(deprovisioningReplacementNodeInitializedHistogram)()

	// cordon the old nodes before we launch the replacements to prevent new pods from scheduling to the old nodes
	if err := c.setNodesUnschedulable(ctx, true, action.nodesToRemove...); err != nil {
		return fmt.Errorf("cordoning nodes, %w", err)
	}

	machineNames, err := c.provisioner.LaunchMachines(ctx, action.replacementMachines, provisioning.WithReason(reason))
	if err != nil {
		// uncordon the nodes as the launch may fail (e.g. ICE or incompatible AMI)
		err = multierr.Append(err, c.setNodesUnschedulable(ctx, false, action.nodesToRemove...))
		return err
	}
	if len(machineNames) != len(action.replacementMachines) {
		// shouldn't ever occur since a partially failed LaunchMachines should return an error
		return fmt.Errorf("expected %d machines, got %d", len(action.replacementMachines), len(machineNames))
	}

	// We have the new nodes created at the API server so mark the old nodes for deletion
	c.cluster.MarkForDeletion(lo.Map(action.nodesToRemove, func(c *CandidateNode, _ int) string { return c.Name() })...)
	// Wait for nodes to be ready
	// TODO @njtran: Allow to bypass this check for certain deprovisioners
	errs := make([]error, len(machineNames))
	workqueue.ParallelizeUntil(ctx, len(machineNames), len(machineNames), func(i int) {
		// Wait for the node to be ready
		var once sync.Once
		machine := &v1alpha5.Machine{}
		if err = retry.Do(func() error {
			if err = c.kubeClient.Get(ctx, client.ObjectKey{Name: machineNames[i]}, machine); err != nil {
				return fmt.Errorf("getting node, %w", err)
			}
			once.Do(func() {
				c.recorder.Publish(deprovisioningevents.LaunchingMachine(machine, action.String()))
			})
			if !machine.StatusConditions().GetCondition(v1alpha5.MachineInitialized).IsTrue() {
				// make the user aware of why deprovisioning is paused
				c.recorder.Publish(deprovisioningevents.WaitingOnReadiness(machine))
				return fmt.Errorf("machine is not initialized")
			}
			return nil
		}, waitRetryOptions...); err != nil {
			// nodes never become ready, so uncordon the nodes we were trying to delete and report the error
			errs[i] = err
		}
	})
	multiErr := multierr.Combine(errs...)
	if multiErr != nil {
		c.cluster.UnmarkForDeletion(lo.Map(action.nodesToRemove, func(c *CandidateNode, _ int) string { return c.Name() })...)
		return multierr.Combine(c.setNodesUnschedulable(ctx, false, action.nodesToRemove...),
			fmt.Errorf("timed out checking node readiness, %w", multiErr))
	}
	return nil
}

func (c *Controller) setNodesUnschedulable(ctx context.Context, isUnschedulable bool, nodes ...*CandidateNode) error {
	var multiErr error
	for _, n := range nodes {
		node := &v1.Node{}
		if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: n.Node.Node.Name}, node); err != nil {
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

		stored := node.DeepCopy()
		node.Spec.Unschedulable = isUnschedulable
		if err := c.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			multiErr = multierr.Append(multiErr, fmt.Errorf("patching node %s, %w", node.Name, err))
		}
	}
	return multiErr
}
