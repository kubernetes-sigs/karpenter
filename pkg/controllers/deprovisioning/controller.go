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
	"k8s.io/apimachinery/pkg/types"
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
	mu             sync.Mutex
	lastRun        map[string]time.Time
}

// pollingPeriod that we inspect cluster to look for opportunities to deprovision
const pollingPeriod = 10 * time.Second
const immediately = time.Millisecond

var errCandidateDeleting = fmt.Errorf("candidate is deleting")

// waitRetryOptions are the retry options used when waiting on a machine to become ready or to be deleted
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
		lastRun:       map[string]time.Time{},
		deprovisioners: []Deprovisioner{
			// Expire any machines that must be deleted, allowing their pods to potentially land on currently
			NewExpiration(clk, kubeClient, cluster, provisioner, recorder),
			// Terminate any machines that have drifted from provisioning specifications, allowing the pods to reschedule.
			NewDrift(kubeClient, cluster, provisioner, recorder),
			// Delete any remaining empty machines as there is zero cost in terms of disruption.  Emptiness and
			// emptyNodeConsolidation are mutually exclusive, only one of these will operate
			NewEmptiness(clk),
			NewEmptyMachineConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder),
			// Attempt to identify multiple machines that we can consolidate simultaneously to reduce pod churn
			NewMultiMachineConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder),
			// And finally fall back our single machines consolidation to further reduce cluster cost.
			NewSingleMachineConsolidation(clk, cluster, kubeClient, provisioner, cp, recorder),
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
	// this won't catch if the reconcile loop hangs forever, but it will catch other issues
	c.logAbnormalRuns(ctx)
	defer c.logAbnormalRuns(ctx)
	c.recordRun("deprovisioning-loop")

	// We need to ensure that our internal cluster state mechanism is synced before we proceed
	// with making any scheduling decision off of our state nodes. Otherwise, we have the potential to make
	// a scheduling decision based on a smaller subset of nodes in our cluster state than actually exist.
	if !c.cluster.Synced(ctx) {
		logging.FromContext(ctx).Debugf("waiting on cluster sync")
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	// Attempt different deprovisioning methods. We'll only let one method perform an action
	isConsolidated := c.cluster.Consolidated()
	for _, d := range c.deprovisioners {
		c.recordRun(fmt.Sprintf("%T", d))
		success, err := c.deprovision(ctx, d)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("deprovisioning via %q, %w", d, err)
		}
		if success {
			return reconcile.Result{RequeueAfter: immediately}, nil
		}
	}

	if !isConsolidated {
		// Mark cluster as consolidated, only if the deprovisioners ran and were not able to perform any work.
		c.cluster.SetConsolidated(true)
	}
	// All deprovisioners did nothing, so return nothing to do
	return reconcile.Result{RequeueAfter: pollingPeriod}, nil
}

func (c *Controller) deprovision(ctx context.Context, deprovisioner Deprovisioner) (bool, error) {
	defer metrics.Measure(deprovisioningDurationHistogram.WithLabelValues(deprovisioner.String()))()
	candidates, err := GetCandidates(ctx, c.cluster, c.kubeClient, c.recorder, c.clock, c.cloudProvider, deprovisioner.ShouldDeprovision)
	if err != nil {
		return false, fmt.Errorf("determining candidates, %w", err)
	}
	// If there are no candidate nodes, move to the next deprovisioner
	if len(candidates) == 0 {
		return false, nil
	}

	// Determine the deprovisioning action
	cmd, err := deprovisioner.ComputeCommand(ctx, candidates...)
	if err != nil {
		return false, fmt.Errorf("computing deprovisioning decision, %w", err)
	}
	if cmd.Action() == NoOpAction {
		return false, nil
	}

	// Attempt to deprovision
	if err := c.executeCommand(ctx, deprovisioner, cmd); err != nil {
		return false, fmt.Errorf("deprovisioning candidates, %w", err)
	}

	return true, nil
}

func (c *Controller) executeCommand(ctx context.Context, d Deprovisioner, command Command) error {
	deprovisioningActionsPerformedCounter.With(map[string]string{
		// TODO: make this just command.Action() since we've added the deprovisioner as its own label.
		actionLabel:        fmt.Sprintf("%s/%s", d, command.Action()),
		deprovisionerLabel: d.String(),
	}).Inc()
	logging.FromContext(ctx).Infof("deprovisioning via %s %s", d, command)

	reason := fmt.Sprintf("%s/%s", d, command.Action())
	if command.Action() == ReplaceAction {
		if err := c.launchReplacementMachines(ctx, command, reason); err != nil {
			// If we failed to launch the replacement, don't deprovision.  If this is some permanent failure,
			// we don't want to disrupt workloads with no way to provision new nodes for them.
			return fmt.Errorf("launching replacement machine, %w", err)
		}
	}

	for _, candidate := range command.candidates {
		c.recorder.Publish(deprovisioningevents.Terminating(candidate.Node, candidate.Machine, command.String())...)

		if err := c.kubeClient.Delete(ctx, candidate.Machine); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			logging.FromContext(ctx).Errorf("terminating machine, %s", err)
		} else {
			metrics.MachinesTerminatedCounter.With(prometheus.Labels{
				metrics.ReasonLabel:      reason,
				metrics.ProvisionerLabel: candidate.provisioner.Name,
			}).Inc()
		}
	}

	// We wait for nodes to delete to ensure we don't start another round of deprovisioning until this node is fully
	// deleted.
	for _, oldCandidate := range command.candidates {
		c.waitForDeletion(ctx, oldCandidate.Machine)
	}
	return nil
}

// launchReplacementMachines launches replacement machines and blocks until it is ready
// nolint:gocyclo
func (c *Controller) launchReplacementMachines(ctx context.Context, action Command, reason string) error {
	defer metrics.Measure(deprovisioningReplacementNodeInitializedHistogram)()
	candidateNodeNames := lo.Map(action.candidates, func(c *Candidate, _ int) string { return c.Node.Name })

	// cordon the old nodes before we launch the replacements to prevent new pods from scheduling to the old nodes
	if err := c.setNodesUnschedulable(ctx, true, candidateNodeNames...); err != nil {
		return fmt.Errorf("cordoning nodes, %w", err)
	}

	machineNames, err := c.provisioner.LaunchMachines(ctx, action.replacements, provisioning.WithReason(reason))
	if err != nil {
		// uncordon the nodes as the launch may fail (e.g. ICE)
		err = multierr.Append(err, c.setNodesUnschedulable(ctx, false, candidateNodeNames...))
		return err
	}
	if len(machineNames) != len(action.replacements) {
		// shouldn't ever occur since a partially failed LaunchMachines should return an error
		return fmt.Errorf("expected %d machines, got %d", len(action.replacements), len(machineNames))
	}

	// We have the new machines created at the API server so mark the old machines for deletion
	c.cluster.MarkForDeletion(candidateNodeNames...)

	errs := make([]error, len(machineNames))
	workqueue.ParallelizeUntil(ctx, len(machineNames), len(machineNames), func(i int) {
		// machine never became ready or the machines that we tried to launch got Insufficient Capacity or some
		// other transient error
		errs[i] = c.waitForReadiness(ctx, action, machineNames[i])
	})
	if err = multierr.Combine(errs...); err != nil {
		c.cluster.UnmarkForDeletion(candidateNodeNames...)
		return multierr.Combine(c.setNodesUnschedulable(ctx, false, candidateNodeNames...),
			fmt.Errorf("timed out checking machine readiness, %w", err))
	}
	return nil
}

// TODO @njtran: Allow to bypass this check for certain deprovisioners
func (c *Controller) waitForReadiness(ctx context.Context, action Command, name string) error {
	// Wait for the machine to be initialized
	var once sync.Once
	pollStart := time.Now()
	return retry.Do(func() error {
		machine := &v1alpha5.Machine{}
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: name}, machine); err != nil {
			// If the machine was deleted after a few seconds (to give the cache time to update), then we assume
			// that the machine was deleted due to an Insufficient Capacity error
			if errors.IsNotFound(err) && c.clock.Since(pollStart) > time.Second*5 {
				return retry.Unrecoverable(fmt.Errorf("getting machine, %w", err))
			}
			return fmt.Errorf("getting machine, %w", err)
		}
		once.Do(func() {
			c.recorder.Publish(deprovisioningevents.Launching(machine, action.String()))
		})
		if !machine.StatusConditions().GetCondition(v1alpha5.MachineInitialized).IsTrue() {
			// make the user aware of why deprovisioning is paused
			c.recorder.Publish(deprovisioningevents.WaitingOnReadiness(machine))
			return fmt.Errorf("machine is not initialized")
		}
		return nil
	}, waitRetryOptions...)
}

// waitForDeletion waits for the specified machine to be removed from the API server. This deletion can take some period
// of time if there are PDBs that govern pods on the machine as we need to wait until the node drains before
// it's actually deleted.
func (c *Controller) waitForDeletion(ctx context.Context, machine *v1alpha5.Machine) {
	if err := retry.Do(func() error {
		m := &v1alpha5.Machine{}
		nerr := c.kubeClient.Get(ctx, client.ObjectKeyFromObject(machine), m)
		// We expect the not machine found error, at which point we know the machine is deleted.
		if errors.IsNotFound(nerr) {
			return nil
		}
		// make the user aware of why deprovisioning is paused
		c.recorder.Publish(deprovisioningevents.WaitingOnDeletion(machine))
		if nerr != nil {
			return fmt.Errorf("expected machine to be not found, %w", nerr)
		}
		// the machine still exists
		return fmt.Errorf("expected machine to be not found")
	}, waitRetryOptions...,
	); err != nil {
		logging.FromContext(ctx).Errorf("Waiting on machine deletion, %s", err)
	}
}

func (c *Controller) setNodesUnschedulable(ctx context.Context, isUnschedulable bool, names ...string) error {
	var multiErr error
	for _, name := range names {
		node := &v1.Node{}
		if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
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

func (c *Controller) recordRun(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRun[s] = c.clock.Now()
}

func (c *Controller) logAbnormalRuns(ctx context.Context) {
	const AbnormalTimeLimit = 15 * time.Minute
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, runTime := range c.lastRun {
		if timeSince := c.clock.Since(runTime); timeSince > AbnormalTimeLimit {
			logging.FromContext(ctx).Debugf("abnormal time between runs of %s = %s", name, timeSince)
		}
	}
}
