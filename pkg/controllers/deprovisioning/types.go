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

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
)

type Deprovisioner interface {
	ShouldDeprovision(context.Context, *state.Node, *v1alpha5.Provisioner, []*v1.Pod) bool
	ComputeCommand(context.Context, ...CandidateNode) (Command, error)
	String() string
}

type action byte

const (
	actionDelete action = iota
	actionReplace
	actionRetry
	actionDoNothing
)

func (a action) String() string {
	switch a {
	// Deprovisioning action with no replacement nodes
	case actionDelete:
		return "delete"
	// Deprovisioning action with replacement nodes
	case actionReplace:
		return "replace"
	// Deprovisioning failed for a retryable reason
	case actionRetry:
		return "retry"
	case actionDoNothing:
		return "do nothing"
	default:
		return fmt.Sprintf("unknown (%d)", a)
	}
}

type Command struct {
	nodesToRemove    []*v1.Node
	action           action
	replacementNodes []*scheduling.Machine
}

func (o Command) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s, terminating %d nodes ", o.action, len(o.nodesToRemove))
	for i, old := range o.nodesToRemove {
		if i != 0 {
			fmt.Fprint(&buf, ", ")
		}
		fmt.Fprintf(&buf, "%s", old.Name)
		if instanceType, ok := old.Labels[v1.LabelInstanceTypeStable]; ok {
			fmt.Fprintf(&buf, "/%s", instanceType)
		}
		if capacityType, ok := old.Labels[v1alpha5.LabelCapacityType]; ok {
			fmt.Fprintf(&buf, "/%s", capacityType)
		}
	}
	if len(o.replacementNodes) == 0 {
		return buf.String()
	}
	odNodes := 0
	spotNodes := 0
	for _, node := range o.replacementNodes {
		ct := node.Requirements.Get(v1alpha5.LabelCapacityType)
		if ct.Has(v1alpha5.CapacityTypeOnDemand) {
			odNodes++
		}
		if ct.Has(v1alpha5.CapacityTypeSpot) {
			spotNodes++
		}
	}
	// Print list of instance types for the first replacementNode.
	if len(o.replacementNodes) > 1 {
		fmt.Fprintf(&buf, " and replacing with %d spot and %d on-demand nodes from types %s",
			spotNodes, odNodes,
			scheduling.InstanceTypeList(o.replacementNodes[0].InstanceTypeOptions))
		return buf.String()
	}
	ct := o.replacementNodes[0].Requirements.Get(v1alpha5.LabelCapacityType)
	nodeDesc := "node"
	if ct.Len() == 1 {
		nodeDesc = fmt.Sprintf("%s node", ct.Any())
	}
	fmt.Fprintf(&buf, " and replacing with %s from types %s",
		nodeDesc,
		scheduling.InstanceTypeList(o.replacementNodes[0].InstanceTypeOptions))
	return buf.String()
}

// CandidateNode is a node that we are considering for deprovisioning along with extra information to be used in
// making that determination
type CandidateNode struct {
	*v1.Node
	instanceType   *cloudprovider.InstanceType
	capacityType   string
	zone           string
	provisioner    *v1alpha5.Provisioner
	disruptionCost float64
	pods           []*v1.Pod
}
