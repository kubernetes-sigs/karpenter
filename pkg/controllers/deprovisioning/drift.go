package deprovisioning

import (
	"context"
	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sort"
	"time"
)

// Consolidation is the consolidation controller.
type Drift struct {
	kubeClient client.Client
	clock      clock.Clock
	cluster    *state.Cluster
}

func (d *Drift) ShouldDeprovision(ctx context.Context, n *state.Node, provisioner *v1alpha5.Provisioner, _ []*v1.Pod) bool {
	if provisioner == nil {
		return false
	}
	_, hasDriftedAnnotation := n.Node.Annotations[v1alpha5.DriftedAnnotationKey]
	return hasDriftedAnnotation
}

func (d *Drift) SortCandidates(nodes []CandidateNode) []CandidateNode {
	sort.Slice(nodes, func(i int, j int) bool {
		return nodes[i].disruptionCost < nodes[j].disruptionCost
	})
	return nodes
}

func (d *Drift) ComputeCommand(ctx context.Context, _ int, nodes ...CandidateNode) (Command, error) {
	driftedNodes := lo.Filter(nodes, func(n CandidateNode, _ int) bool {
		_, hasDrifted := n.Node.Annotations[v1alpha5.DriftedAnnotationKey]
		return hasDrifted
	})
	if len(driftedNodes) == 0 {
		return Command{action: actionDoNothing}, nil
	}
	return Command{
		nodesToRemove: lo.Map(driftedNodes, func(n CandidateNode, _ int) *v1.Node { return n.Node }),
		action:        actionReplace,
		created:       d.clock.Now(),
	}, nil
}

func (d *Drift) ValidateCommand(ctx context.Context, nodes []CandidateNode, cmd Command) (bool, error) {
	//Check the replacement node
	return false, nil
}

func (d *Drift) TTL() time.Duration {
	return 15 * time.Second
}

func (d *Drift) String() string {
	return metrics.DriftReason
}

func (d *Drift) ShouldProvision() {

}
