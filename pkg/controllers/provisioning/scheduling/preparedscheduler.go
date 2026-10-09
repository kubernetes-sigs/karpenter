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

package scheduling

import (
	"context"

	"github.com/awslabs/operatorpkg/option"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

type preparedExistingNode struct {
	taints                  []corev1.Taint
	daemonResources         corev1.ResourceList
	instanceType            *cloudprovider.InstanceType
	isUnderConsolidateAfter bool
}

// PreparedSchedulerInputs contains candidate-independent scheduler construction
// results that are safe to share across fresh speculative solves in one
// controller pass. Mutable solve state is never stored here.
type PreparedSchedulerInputs struct {
	templates                []*NodeClaimTemplate
	daemonOverheadGroups     map[*NodeClaimTemplate][]DaemonOverheadGroup
	domainGroups             map[string]TopologyDomainGroup
	existingNodes            map[string]preparedExistingNode
	remainingResources       map[string]corev1.ResourceList
	deletingNodeNames        map[string]struct{}
	toleratePreferNoSchedule bool
	stats                    PreparedSchedulerStats
	reservationManager       *ReservationManager
}

// PreparedSchedulerStats describes immutable inputs shared by one pass-scoped session.
type PreparedSchedulerStats struct {
	Nodes         int
	NodePools     int
	Templates     int
	InstanceTypes int
	DaemonSetPods int
	TopologyKeys  int
}

// Stats returns the bounded size of prepared immutable inputs.
func (p *PreparedSchedulerInputs) Stats() PreparedSchedulerStats {
	if p == nil {
		return PreparedSchedulerStats{}
	}
	return p.stats
}

// NewPreparedSchedulerInputs performs pass-scoped, candidate-independent
// scheduler preparation. The returned object is immutable after publication.
//
//nolint:gocyclo
func NewPreparedSchedulerInputs(
	ctx context.Context,
	nodePools []*v1.NodePool,
	stateNodes []*state.StateNode,
	instanceTypes map[string][]*cloudprovider.InstanceType,
	daemonSetPods []*corev1.Pod,
	recorder events.Recorder,
	clk clock.Clock,
	opts ...Options,
) (*PreparedSchedulerInputs, error) {
	resolvedOptions := option.Resolve(opts...)
	minValuesPolicy := resolvedOptions.minValuesPolicy

	toleratePreferNoSchedule := hasPreferNoScheduleTaint(nodePools)

	templates := make([]*NodeClaimTemplate, 0, len(nodePools))
	for _, nodePool := range nodePools {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if template, ok := newFilteredNodeClaimTemplate(ctx, nodePool, instanceTypes[nodePool.Name], recorder, minValuesPolicy); ok {
			templates = append(templates, template)
		}
	}

	daemonGroups := buildDaemonOverheadGroups(ctx, templates, daemonSetPods)
	nodePoolByName := lo.SliceToMap(nodePools, func(nodePool *v1.NodePool) (string, *v1.NodePool) {
		return nodePool.Name, nodePool
	})
	helper := &Scheduler{instanceTypes: instanceTypes}
	existing := make(map[string]preparedExistingNode, len(stateNodes))
	deleting := map[string]struct{}{}
	for _, node := range stateNodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if node.MarkedForDeletion() {
			deleting[node.Name()] = struct{}{}
		}
		taints := node.Taints()
		daemons := helper.getCompatibleDaemonPods(ctx, node, taints, daemonSetPods)
		existing[node.Name()] = preparedExistingNode{
			taints:                  taints,
			daemonResources:         resources.RequestsForPods(daemons...),
			instanceType:            preparedInstanceTypeForNode(node, instanceTypes),
			isUnderConsolidateAfter: resolvedOptions.enforceConsolidateAfter && disruptionutils.IsUnderConsolidateAfter(nodePoolByName[node.Labels()[v1.NodePoolLabelKey]], node.NodeClaim, clk),
		}
	}

	remainingResources := lo.SliceToMap(nodePools, func(nodePool *v1.NodePool) (string, corev1.ResourceList) {
		return nodePool.Name, corev1.ResourceList(nodePool.Spec.Limits).DeepCopy()
	})
	instanceTypeCount := 0
	for _, values := range instanceTypes {
		instanceTypeCount += len(values)
	}
	domainGroups := buildDomainGroups(nodePools, instanceTypes)
	return &PreparedSchedulerInputs{
		templates:                templates,
		daemonOverheadGroups:     daemonGroups,
		domainGroups:             domainGroups,
		existingNodes:            existing,
		remainingResources:       remainingResources,
		deletingNodeNames:        deleting,
		toleratePreferNoSchedule: toleratePreferNoSchedule,
		reservationManager:       NewReservationManager(instanceTypes),
		stats: PreparedSchedulerStats{
			Nodes:         len(existing),
			NodePools:     len(nodePools),
			Templates:     len(templates),
			InstanceTypes: instanceTypeCount,
			DaemonSetPods: len(daemonSetPods),
			TopologyKeys:  len(domainGroups),
		},
	}, nil
}

func preparedInstanceTypeForNode(node *state.StateNode, instanceTypes map[string][]*cloudprovider.InstanceType) *cloudprovider.InstanceType {
	nodePoolName := node.Labels()[v1.NodePoolLabelKey]
	instanceTypeName := node.Labels()[corev1.LabelInstanceTypeStable]
	instanceType, _ := lo.Find(instanceTypes[nodePoolName], func(instanceType *cloudprovider.InstanceType) bool {
		return instanceType.Name == instanceTypeName
	})
	return instanceType
}

func (p *PreparedSchedulerInputs) cloneRemainingResources() map[string]corev1.ResourceList {
	return lo.MapValues(p.remainingResources, func(value corev1.ResourceList, _ string) corev1.ResourceList {
		return value.DeepCopy()
	})
}

func (p *PreparedSchedulerInputs) cloneDeletingNodeNames() sets.Set[string] {
	out := sets.New[string]()
	for name := range p.deletingNodeNames {
		out.Insert(name)
	}
	return out
}
