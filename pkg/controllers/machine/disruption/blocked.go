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

package disruption

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/apis"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning"
	deprovisioningevents "github.com/aws/karpenter-core/pkg/controllers/deprovisioning/events"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
)

// Blocked is a machine sub-controller that adds or removes status conditions on machines if they're blocked for Deprovisioning
type Blocked struct {
	kubeClient    client.Client
	cluster       *state.Cluster
	recorder      events.Recorder
	cloudProvider cloudprovider.CloudProvider
}

// Blocked will wait for cluster state to be synced before checking if a machine should not be deprovisioned. Karpenter will check:
// 1. If the node is in a deprovisionable state: (1) Initialized (2) Not Deleting (3) Not Recently Nominated
// 2. Has all necesssary labels for computation
// 3. Does not have pods blocking eviction - PDBs/do-not-evict
//
//nolint:gocyclo
func (b *Blocked) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	if nodeClaim == nil {
		return reconcile.Result{}, nil
	}
	if !b.cluster.Synced(ctx) {
		return reconcile.Result{}, fmt.Errorf("waiting for cluster state to sync")
	}

	condition := apis.Condition{
		Type:     v1beta1.NodeDeprovisioningBlocked,
		Status:   v1.ConditionTrue,
		Severity: apis.ConditionSeverityWarning,
	}

	// Get the node from cluster state before we can check anything else.
	// If we fail to get a node, this means cluster state is no longer synced after we thought it was.
	node, err := b.cluster.GetNode(nodeClaim.Name)
	if err != nil {
		condition.Reason = fmt.Sprintf("state doesn't have nodeclaim, %s", err)
		nodeClaim.StatusConditions().SetCondition(condition)
		return reconcile.Result{}, nil
	}
	// If there's no node or no machine, we may have incomplete information, so add that it's blocked.
	if node.Node == nil || node.Machine == nil {
		condition.Reason = "state node doesn't contain both a node and a machine"
		nodeClaim.StatusConditions().SetCondition(condition)
		return reconcile.Result{}, nil
	}
	// Cluster state is synced, so append all blocked reasons together and set the condition for any failed reasons.
	var reasons []string
	defer func() {
		if len(reasons) == 0 {
			nodeClaim.StatusConditions().SetCondition(apis.Condition{
				Type:   v1beta1.NodeDeprovisioningBlocked,
				Status: v1.ConditionFalse,
			})
		} else {
			condition.Reason = strings.Join(reasons, ";")
			nodeClaim.StatusConditions().SetCondition(condition)
		}
	}()

	// 1. Ensure the node is in a deprovisionable state.

	// skip any nodes that are already marked for deletion and being handled
	if node.MarkedForDeletion() {
		reasons = append(reasons, "node claim is marked for deletion")
	}
	// skip nodes that aren't initialized
	// This also means that the real Node doesn't exist for it
	if !node.Initialized() {
		reasons = append(reasons, "node claim is not initialized")
	}
	// skip the node if it is nominated by a recent provisioning pass to be the target of a pending pod.
	if node.Nominated() {
		reasons = append(reasons, "state node is nominated for a pending pod")
		b.recorder.Publish(deprovisioningevents.Blocked(node.Node, node.Machine, "Nominated for a pending pod")...)
	}

	// 2. Ensure the node has all the labels we need
	var missingLabelKeys []string
	for _, label := range []string{
		v1alpha5.LabelCapacityType,
		v1.LabelTopologyZone,
		v1alpha5.ProvisionerNameLabelKey,
	} {
		if _, ok := node.Labels()[label]; !ok {
			missingLabelKeys = append(missingLabelKeys, label)
			// return nil, fmt.Errorf("state node doesn't have required label '%s'", label)
		}
		missingLabelKeys = append(missingLabelKeys, label)
	}
	if missingLabelKeys != nil {
		reasons = append(reasons, fmt.Sprintf("node claim doesn't have required label(s) '%s'", strings.Join(missingLabelKeys, ",")))
		b.recorder.Publish(deprovisioningevents.Blocked(node.Node, node.Machine, fmt.Sprintf("Required label(s) %q do not exist", strings.Join(missingLabelKeys, ",")))...)
	}

	provisionerMap, provisionerToInstanceTypes, err := deprovisioning.BuildProvisionerMap(ctx, b.kubeClient, b.cloudProvider)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("building provisioner mapping")
	}

	provisioner := provisionerMap[node.Labels()[v1alpha5.ProvisionerNameLabelKey]]
	instanceTypeMap := provisionerToInstanceTypes[node.Labels()[v1alpha5.ProvisionerNameLabelKey]]
	// skip any nodes where we can't determine the provisioner
	if nodePool == nil || provisioner == nil || instanceTypeMap == nil {
		reasons = append(reasons, fmt.Sprintf("provisioner '%s' can't be resolved for state node", node.Labels()[v1alpha5.ProvisionerNameLabelKey]))
		b.recorder.Publish(deprovisioningevents.Blocked(node.Node, node.Machine, fmt.Sprintf("Owning provisioner %q not found", node.Labels()[v1alpha5.ProvisionerNameLabelKey]))...)
	}
	instanceType := instanceTypeMap[node.Labels()[v1.LabelInstanceTypeStable]]
	// skip any nodes that we can't determine the instance of
	if instanceType == nil {
		reasons = append(reasons, fmt.Sprintf("instance type '%s' can't be resolved", node.Labels()[v1.LabelInstanceTypeStable]))
		b.recorder.Publish(deprovisioningevents.Blocked(node.Node, node.Machine, fmt.Sprintf("Instance type %q not found", node.Labels()[v1.LabelInstanceTypeStable]))...)
	}

	// 3. Check that there are no pods blocking eviction.
	pdbs, err := deprovisioning.NewPDBLimits(ctx, b.kubeClient)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("tracking PodDisruptionBudgets, %w", err)
	}

	pods, err := node.Pods(ctx, b.kubeClient)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting pods from state node, %w", err)
	}

	if pdb, ok := pdbs.CanEvictPods(pods); !ok {
		reasons = append(reasons, fmt.Sprintf("has blocking PDB %q preventing pod evictions", pdb.Name))
		b.recorder.Publish(deprovisioningevents.Blocked(node.Node, node.Machine, fmt.Sprintf("PDB %q prevents pod evictions", pdb))...)
	}
	if p, ok := deprovisioning.HasDoNotEvictPod(pods); ok {
		reasons = append(reasons, fmt.Sprintf("has do-not-evict pod %s", p))
		b.recorder.Publish(deprovisioningevents.Blocked(node.Node, node.Machine, fmt.Sprintf("Pod %q has do not evict annotation", client.ObjectKeyFromObject(p)))...)
	}
	return reconcile.Result{}, nil
}
