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

package disruption

import (
	"bytes"
	"context"
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/utils/pod"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	disruptionevents "sigs.k8s.io/karpenter/pkg/controllers/disruption/events"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption/orchestration"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
)

type Method interface {
	ShouldDisrupt(context.Context, *Candidate) bool
	ComputeCommand(context.Context, map[string]int, ...*Candidate) (Command, error)
	Type() string
	ConsolidationType() string
}

type CandidateFilter func(context.Context, *Candidate) bool

// Candidate is a state.StateNode that we are considering for disruption along with extra information to be used in
// making that determination
type Candidate struct {
	*state.StateNode
	instanceType      *cloudprovider.InstanceType
	nodePool          *v1beta1.NodePool
	zone              string
	capacityType      string
	disruptionCost    float64
	reschedulablePods []*v1.Pod
}

//nolint:gocyclo
func NewCandidate(ctx context.Context, kubeClient client.Client, recorder events.Recorder, clk clock.Clock, node *state.StateNode, pdbs *PDBLimits,
	nodePoolMap map[string]*v1beta1.NodePool, nodePoolToInstanceTypesMap map[string]map[string]*cloudprovider.InstanceType, queue *orchestration.Queue) (*Candidate, error) {

	if node.Node == nil || node.NodeClaim == nil {
		return nil, fmt.Errorf("state node doesn't contain both a node and a nodeclaim")
	}
	// skip any candidates that are already marked for deletion and being handled
	if node.MarkedForDeletion() {
		return nil, fmt.Errorf("state node is marked for deletion")
	}
	// skip candidates that aren't initialized
	if !node.Initialized() {
		return nil, fmt.Errorf("state node isn't initialized")
	}
	// If the orchestration queue is already considering a candidate we want to disrupt, don't consider it a candidate.
	if queue.HasAny(node.ProviderID()) {
		return nil, fmt.Errorf("candidate is already being deprovisioned")
	}
	if _, ok := node.Annotations()[v1beta1.DoNotDisruptAnnotationKey]; ok {
		recorder.Publish(disruptionevents.Blocked(node.Node, node.NodeClaim, fmt.Sprintf("Disruption is blocked with the %q annotation", v1beta1.DoNotDisruptAnnotationKey))...)
		return nil, fmt.Errorf("disruption is blocked through the %q annotation", v1beta1.DoNotDisruptAnnotationKey)
	}
	// check whether the node has all the labels we need
	for _, label := range []string{
		v1beta1.CapacityTypeLabelKey,
		v1.LabelTopologyZone,
	} {
		if _, ok := node.Labels()[label]; !ok {
			recorder.Publish(disruptionevents.Blocked(node.Node, node.NodeClaim, fmt.Sprintf("Required label %q doesn't exist", label))...)
			return nil, fmt.Errorf("state node doesn't have required label %q", label)
		}
	}
	nodePoolName, ok := node.Labels()[v1beta1.NodePoolLabelKey]
	if !ok {
		return nil, fmt.Errorf("state node doesn't have the Karpenter owner label")
	}
	nodePool := nodePoolMap[nodePoolName]
	instanceTypeMap := nodePoolToInstanceTypesMap[nodePoolName]
	// skip any candidates where we can't determine the nodePool
	if nodePool == nil || instanceTypeMap == nil {
		recorder.Publish(disruptionevents.Blocked(node.Node, node.NodeClaim, fmt.Sprintf("Owning nodepool %q not found", nodePoolName))...)
		return nil, fmt.Errorf("nodepool %q can't be resolved for state node", nodePoolName)
	}
	instanceType := instanceTypeMap[node.Labels()[v1.LabelInstanceTypeStable]]
	// skip any candidates that we can't determine the instance of
	if instanceType == nil {
		recorder.Publish(disruptionevents.Blocked(node.Node, node.NodeClaim, fmt.Sprintf("Instance type %q not found", node.Labels()[v1.LabelInstanceTypeStable]))...)
		return nil, fmt.Errorf("instance type %q can't be resolved", node.Labels()[v1.LabelInstanceTypeStable])
	}
	// skip the node if it is nominated by a recent provisioning pass to be the target of a pending pod.
	if node.Nominated() {
		recorder.Publish(disruptionevents.Blocked(node.Node, node.NodeClaim, "Nominated for a pending pod")...)
		return nil, fmt.Errorf("state node is nominated for a pending pod")
	}
	pods, err := node.Pods(ctx, kubeClient)
	if err != nil {
		logging.FromContext(ctx).Errorf("determining node pods, %s", err)
		return nil, fmt.Errorf("getting pods from state node, %w", err)
	}
	for _, po := range pods {
		// We only consider pods that are actively running for "karpenter.sh/do-not-disrupt"
		// This means that we will allow Mirror Pods and DaemonSets to block disruption using this annotation
		if !pod.IsDisruptable(po) {
			recorder.Publish(disruptionevents.Blocked(node.Node, node.NodeClaim, fmt.Sprintf(`Pod %q has "karpenter.sh/do-not-disrupt" annotation`, client.ObjectKeyFromObject(po)))...)
			return nil, fmt.Errorf(`pod %q has "karpenter.sh/do-not-disrupt" annotation`, client.ObjectKeyFromObject(po))
		}
	}
	if pdbKey, ok := pdbs.CanEvictPods(pods); !ok {
		recorder.Publish(disruptionevents.Blocked(node.Node, node.NodeClaim, fmt.Sprintf("PDB %q prevents pod evictions", pdbKey))...)
		return nil, fmt.Errorf("pdb %q prevents pod evictions", pdbKey)
	}
	return &Candidate{
		StateNode:         node.DeepCopy(),
		instanceType:      instanceType,
		nodePool:          nodePool,
		capacityType:      node.Labels()[v1beta1.CapacityTypeLabelKey],
		zone:              node.Labels()[v1.LabelTopologyZone],
		reschedulablePods: lo.Filter(pods, func(p *v1.Pod, _ int) bool { return pod.IsReschedulable(p) }),
		// We get the disruption cost from all pods in the candidate, not just the reschedulable pods
		disruptionCost: disruptionCost(ctx, pods) * lifetimeRemaining(clk, nodePool, node.Node),
	}, nil
}

// lifetimeRemaining calculates the fraction of node lifetime remaining in the range [0.0, 1.0].  If the TTLSecondsUntilExpired
// is non-zero, we use it to scale down the disruption costs of candidates that are going to expire.  Just after creation, the
// disruption cost is highest, and it approaches zero as the node ages towards its expiration time.
func lifetimeRemaining(clock clock.Clock, nodePool *v1beta1.NodePool, node *v1.Node) float64 {
	remaining := 1.0
	if nodePool.Spec.Disruption.ExpireAfter.Duration != nil {
		ageInSeconds := clock.Since(node.CreationTimestamp.Time).Seconds()
		totalLifetimeSeconds := nodePool.Spec.Disruption.ExpireAfter.Duration.Seconds()
		lifetimeRemainingSeconds := totalLifetimeSeconds - ageInSeconds
		remaining = clamp(0.0, lifetimeRemainingSeconds/totalLifetimeSeconds, 1.0)
	}
	return remaining
}

type Command struct {
	candidates   []*Candidate
	replacements []*scheduling.NodeClaim
}

type Action string

var (
	NoOpAction    Action = "no-op"
	ReplaceAction Action = "replace"
	DeleteAction  Action = "delete"
)

func (o Command) Action() Action {
	switch {
	case len(o.candidates) > 0 && len(o.replacements) > 0:
		return ReplaceAction
	case len(o.candidates) > 0 && len(o.replacements) == 0:
		return DeleteAction
	default:
		return NoOpAction
	}
}

func (o Command) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s, terminating %d candidates ", o.Action(), len(o.candidates))
	for i, old := range o.candidates {
		if i != 0 {
			fmt.Fprint(&buf, ", ")
		}
		fmt.Fprintf(&buf, "%s", old.Name())
		fmt.Fprintf(&buf, "/%s", old.instanceType.Name)
		fmt.Fprintf(&buf, "/%s", old.capacityType)
	}
	if len(o.replacements) == 0 {
		return buf.String()
	}
	odNodeClaims := 0
	spotNodeClaims := 0
	for _, nodeClaim := range o.replacements {
		ct := nodeClaim.Requirements.Get(v1beta1.CapacityTypeLabelKey)
		if ct.Has(v1beta1.CapacityTypeOnDemand) {
			odNodeClaims++
		}
		if ct.Has(v1beta1.CapacityTypeSpot) {
			spotNodeClaims++
		}
	}
	// Print list of instance types for the first replacements.
	if len(o.replacements) > 1 {
		fmt.Fprintf(&buf, " and replacing with %d spot and %d on-demand, from types %s",
			spotNodeClaims, odNodeClaims,
			scheduling.InstanceTypeList(o.replacements[0].InstanceTypeOptions))
		return buf.String()
	}
	ct := o.replacements[0].Requirements.Get(v1beta1.CapacityTypeLabelKey)
	nodeDesc := "node"
	if ct.Len() == 1 {
		nodeDesc = fmt.Sprintf("%s node", ct.Any())
	}
	fmt.Fprintf(&buf, " and replacing with %s from types %s",
		nodeDesc,
		scheduling.InstanceTypeList(o.replacements[0].InstanceTypeOptions))
	return buf.String()
}
