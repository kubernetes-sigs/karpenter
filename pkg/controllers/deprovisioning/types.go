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
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"

	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
)

// Result is used to indicate the action of consolidating so we can optimize by not trying to consolidate if
// we were unable to consolidate the cluster and it hasn't changed state with respect to pods/nodes.
type Result byte

const (
	ResultNothingToDo Result = iota // there are no actions that can be performed given the current cluster state
	ResultRetry                     // we attempted an action, but its validation failed so retry soon
	ResultFailed                    // the action failed entirely
	ResultSuccess                   // the action was successful
)

func (r Result) String() string {
	switch r {
	case ResultNothingToDo:
		return "Nothing to do"
	case ResultRetry:
		return "Retry"
	case ResultFailed:
		return "Failed"
	case ResultSuccess:
		return "Success"
	default:
		return fmt.Sprintf("Unknown (%d)", r)
	}
}

type Deprovisioner interface {
	// ShouldDeprovision is a predicate used to filter deprovisionable nodes
	ShouldDeprovision(context.Context, *state.Node, *v1alpha5.Provisioner, []*v1.Pod) bool
	// sortCandidates orders deprovisionable nodes by the deprovisioner's pre-determined priority
	SortCandidates([]CandidateNode) []CandidateNode
	// computeCommand generates a deprovisioning command given deprovisionable nodes
	ComputeCommand(context.Context, int, ...CandidateNode) (Command, error)
	// validateCommand validates a command for a deprovisioner
	ValidateCommand(context.Context, []CandidateNode, Command) (bool, error)
	// TTL returns the time to wait for a deprovisioner's validation
	TTL() time.Duration
	// String is the String representation of the deprovisioner
	String() string
}

type action byte

const (
	actionUnknown action = iota
	actionNotPossible
	actionDelete
	actionReplace
	actionDoNothing
	actionFailed
)

func (a action) String() string {
	switch a {
	case actionUnknown:
		return "unknown"
	case actionNotPossible:
		return "not-possible"
	case actionDelete:
		return "delete"
	case actionReplace:
		return "replace"
	case actionDoNothing:
		return "no-action"
	case actionFailed:
		return "failed"
	default:
		return fmt.Sprintf("unknown (%d)", a)
	}
}

type Command struct {
	nodesToRemove    []*v1.Node
	action           action
	replacementNodes []*scheduling.Node
	createdAt        time.Time
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
