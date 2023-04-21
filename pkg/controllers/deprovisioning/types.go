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
	"bytes"
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
)

type Deprovisioner interface {
	ShouldDeprovision(context.Context, *Candidate) bool
	ComputeCommand(context.Context, ...*Candidate) (Command, error)
	String() string
}

type CandidateFilter func(context.Context, *Candidate) bool

// Candidate is a state.StateNode that we are considering for deprovisioning along with extra information to be used in
// making that determination
type Candidate struct {
	*state.StateNode
	instanceType   *cloudprovider.InstanceType
	provisioner    *v1alpha5.Provisioner
	zone           string
	capacityType   string
	disruptionCost float64
	pods           []*v1.Pod
}

//nolint:gocyclo
func NewCandidate(ctx context.Context, kubeClient client.Client, clk clock.Clock, node *state.StateNode,
	provisionerMap map[string]*v1alpha5.Provisioner, provisionerToInstanceTypes map[string]map[string]*cloudprovider.InstanceType) (*Candidate, error) {

	// check whether the node has all the labels we need
	for _, label := range []string{
		v1alpha5.LabelCapacityType,
		v1.LabelTopologyZone,
		v1alpha5.ProvisionerNameLabelKey,
	} {
		if _, ok := node.Labels()[label]; !ok {
			return nil, fmt.Errorf("state node doesn't have required label '%s'", label)
		}
	}

	provisioner := provisionerMap[node.Labels()[v1alpha5.ProvisionerNameLabelKey]]
	instanceTypeMap := provisionerToInstanceTypes[node.Labels()[v1alpha5.ProvisionerNameLabelKey]]
	// skip any nodes where we can't determine the provisioner
	if provisioner == nil || instanceTypeMap == nil {
		return nil, fmt.Errorf("provisioner '%s' can't be resolved for state node", node.Labels()[v1alpha5.ProvisionerNameLabelKey])
	}
	instanceType := instanceTypeMap[node.Labels()[v1.LabelInstanceTypeStable]]
	// skip any nodes that we can't determine the instance of
	if instanceType == nil {
		return nil, fmt.Errorf("instance type '%s' can't be resolved", node.Labels()[v1.LabelInstanceTypeStable])
	}

	// skip any nodes that are already marked for deletion and being handled
	if node.MarkedForDeletion() {
		return nil, fmt.Errorf("state node is marked for deletion")
	}
	// skip nodes that aren't initialized
	// This also means that the real Node doesn't exist for it
	if !node.Initialized() {
		return nil, fmt.Errorf("state node isn't initialized")
	}
	// skip the node if it is nominated by a recent provisioning pass to be the target of a pending pod.
	if node.Nominated() {
		return nil, fmt.Errorf("state node is nominated")
	}
	if node.Node == nil || node.Machine == nil {
		return nil, fmt.Errorf("state node doesn't contain both a node and a machine")
	}

	pods, err := node.Pods(ctx, kubeClient)
	if err != nil {
		logging.FromContext(ctx).Errorf("Determining node pods, %s", err)
		return nil, fmt.Errorf("getting pods from state node, %w", err)
	}
	cn := &Candidate{
		StateNode:    node.DeepCopy(),
		instanceType: instanceType,
		provisioner:  provisioner,
		capacityType: node.Labels()[v1alpha5.LabelCapacityType],
		zone:         node.Labels()[v1.LabelTopologyZone],
		pods:         pods,
	}
	cn.disruptionCost = disruptionCost(ctx, pods) * cn.lifetimeRemaining(clk)
	return cn, nil
}

// lifetimeRemaining calculates the fraction of node lifetime remaining in the range [0.0, 1.0].  If the TTLSecondsUntilExpired
// is non-zero, we use it to scale down the disruption costs of nodes that are going to expire.  Just after creation, the
// disruption cost is highest, and it approaches zero as the node ages towards its expiration time.
func (c *Candidate) lifetimeRemaining(clock clock.Clock) float64 {
	remaining := 1.0
	if c.provisioner.Spec.TTLSecondsUntilExpired != nil {
		ageInSeconds := clock.Since(c.Node.CreationTimestamp.Time).Seconds()
		totalLifetimeSeconds := float64(*c.provisioner.Spec.TTLSecondsUntilExpired)
		lifetimeRemainingSeconds := totalLifetimeSeconds - ageInSeconds
		remaining = clamp(0.0, lifetimeRemainingSeconds/totalLifetimeSeconds, 1.0)
	}
	return remaining
}

type action byte

const (
	actionDelete action = iota
	actionReplace
	actionDoNothing
)

func (a action) String() string {
	switch a {
	// Deprovisioning action with no replacement machines
	case actionDelete:
		return "delete"
	// Deprovisioning action with replacement machines
	case actionReplace:
		return "replace"
	case actionDoNothing:
		return "do nothing"
	default:
		return fmt.Sprintf("unknown (%d)", a)
	}
}

type Command struct {
	candidates   []*Candidate
	action       action
	replacements []*scheduling.Machine
}

func (o Command) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s, terminating %d machines ", o.action, len(o.candidates))
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
	odMachines := 0
	spotMachines := 0
	for _, machine := range o.replacements {
		ct := machine.Requirements.Get(v1alpha5.LabelCapacityType)
		if ct.Has(v1alpha5.CapacityTypeOnDemand) {
			odMachines++
		}
		if ct.Has(v1alpha5.CapacityTypeSpot) {
			spotMachines++
		}
	}
	// Print list of instance types for the first replacements.
	if len(o.replacements) > 1 {
		fmt.Fprintf(&buf, " and replacing with %d spot and %d on-demand machines from types %s",
			spotMachines, odMachines,
			scheduling.InstanceTypeList(o.replacements[0].InstanceTypeOptions))
		return buf.String()
	}
	ct := o.replacements[0].Requirements.Get(v1alpha5.LabelCapacityType)
	machineDesc := "machine"
	if ct.Len() == 1 {
		machineDesc = fmt.Sprintf("%s machine", ct.Any())
	}
	fmt.Fprintf(&buf, " and replacing with %s from types %s",
		machineDesc,
		scheduling.InstanceTypeList(o.replacements[0].InstanceTypeOptions))
	return buf.String()
}
