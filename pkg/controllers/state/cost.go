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

package state

import (
	"context"
	"fmt"
	"sync"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// NecessaryLabels defines the set of required Kubernetes labels that must be present
// on NodeClaim objects for cost tracking to function properly.
var NecessaryLabels = []string{corev1.LabelInstanceTypeStable, v1.CapacityTypeLabelKey, corev1.LabelTopologyZone, v1.NodePoolLabelKey}

var (
	CostTrackingErrorsTotal = opmetrics.NewPrometheusCounter(
		crmetrics.Registry,
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: metrics.NodePoolSubsystem,
			Name:      "cost_tracker_errors_total",
			Help:      "Number of errors encountered during cost tracking operations. Labeled by nodepool and nodeclaim.",
		},
		[]string{
			metrics.NodePoolLabel,
		},
	)
)

// ClusterCost tracks the cost of compute resources across all NodePools in a cluster.
// This is an alpha-level component and its API may change without notice.
//
// The ClusterCost maintains real-time cost information by:
// - Tracking NodeClaim additions and removals
// - Managing instance type offerings and their prices
// - Calculating aggregate costs per NodePool and cluster-wide
//
// All operations are thread-safe through internal locking mechanisms.
type ClusterCost struct {
	sync.RWMutex
	npCostMap map[string]*NodePoolCost // nodepool.Name -> NodePoolCost
	// nodeClaimMap tracks which NodeClaims are currently being monitored for cost
	nodeClaimMap map[string]bool

	cloudProvider cloudprovider.CloudProvider
	client        client.Client
}

// NodePoolCost represents the cost tracking information for a single NodePool.
// It maintains the current cost, available instance types, and count of active offerings.
type NodePoolCost struct {
	cost                 float64
	overlayedInstanceMap map[string]*cloudprovider.InstanceType // instance name -> instance
	// offeringCounts tracks how many instances of each offering type are currently active
	offeringCounts map[OfferingKey]OfferingCount
}

// OfferingKey uniquely identifies a specific compute offering by its zone,
// capacity type (e.g., spot/on-demand), and instance type name.
type OfferingKey struct {
	Zone, Capacity, InstanceName string
}

// OfferingCount tracks the number and cost of instances for a specific offering.
type OfferingCount struct {
	Count int
	Cost  float64 // cost of the offering, not cost * count
}

// NewClusterCost creates and initializes a new ClusterCost instance for tracking
// compute costs across the cluster. It requires a cloud provider for accessing
// instance type and pricing information, and a Kubernetes client for NodePool lookups.
func NewClusterCost(ctx context.Context, cloudProvider cloudprovider.CloudProvider, client client.Client) *ClusterCost {
	return &ClusterCost{
		npCostMap:     make(map[string]*NodePoolCost),
		nodeClaimMap:  make(map[string]bool),
		cloudProvider: cloudProvider,
		client:        client,
	}
}

// UpdateOfferings updates the available instance types and their pricing information
// for a specific NodePool. This method is typically called when NodePool configurations
// change or when cloud provider pricing information is refreshed.
//
// Returns an error if instance type information cannot be updated or if cost
// recalculation fails.
func (cc *ClusterCost) UpdateOfferings(ctx context.Context, np *v1.NodePool, instanceTypes []*cloudprovider.InstanceType) error {
	cc.Lock()
	defer cc.Unlock()
	err := cc.internalUpdateOfferings(np, instanceTypes)
	if err != nil {
		return fmt.Errorf("failed to update offerings for nodepool %q: %w", np.Name, err)
	}
	return nil
}

func (cc *ClusterCost) internalNodepoolUpdate(ctx context.Context, np *v1.NodePool) error {
	instanceTypes, err := cc.cloudProvider.GetInstanceTypes(ctx, np)
	if err != nil {
		return fmt.Errorf("failed to get instance types for nodepool %q: %w", np.Name, err)
	}
	err = cc.internalUpdateOfferings(np, instanceTypes)
	if err != nil {
		return fmt.Errorf("failed to update offerings for nodepool %q: %w", np.Name, err)
	}
	return nil
}

func (cc *ClusterCost) internalUpdateOfferings(np *v1.NodePool, instanceTypes []*cloudprovider.InstanceType) error {
	npCost, exists := cc.npCostMap[np.Name]

	if !exists {
		cc.createNewNodePoolCost(np, instanceTypes)
	} else {
		npCost.overlayedInstanceMap = lo.FilterSliceToMap(instanceTypes, func(it *cloudprovider.InstanceType) (string, *cloudprovider.InstanceType, bool) {
			return it.Name, it, it != nil
		})
		// re-calculate the cost as the instances have changed
		cost, err := npCost.updateCost()
		if err != nil {
			return fmt.Errorf("failed to update cost for nodepool %q: %w", np.Name, err)
		}
		cc.npCostMap[np.Name].cost = cost
	}
	return nil
}

func (npc *NodePoolCost) updateCost() (float64, error) {
	cost := 0.0
	for ok, oc := range npc.offeringCounts {
		overlayedInstanceType := npc.overlayedInstanceMap[ok.InstanceName]
		if overlayedInstanceType == nil {
			return 0, fmt.Errorf("instance type %q not found in overlayed instance map for offering in zone %q with capacity %q", ok.InstanceName, ok.Zone, ok.Capacity)
		}
		// get the offering price from the overlayed instance type
		newOffering, exists := lo.Find(overlayedInstanceType.Offerings, func(o *cloudprovider.Offering) bool {
			return o.CapacityType() == ok.Capacity && o.Zone() == ok.Zone
		})
		if !exists {
			return 0, fmt.Errorf("offering not found for instance type %q in zone %q with capacity type %q", ok.InstanceName, ok.Zone, ok.Capacity)
		}
		// add the new price times the count of that offering
		cost = cost + (float64(oc.Count) * newOffering.Price)
	}
	return cost, nil
}

func (cc *ClusterCost) createNewNodePoolCost(np *v1.NodePool, instanceTypes []*cloudprovider.InstanceType) {
	// create the new npc
	cc.npCostMap[np.Name] = &NodePoolCost{
		overlayedInstanceMap: lo.SliceToMap(instanceTypes, func(it *cloudprovider.InstanceType) (string, *cloudprovider.InstanceType) {
			return it.Name, it
		}),
		offeringCounts: make(map[OfferingKey]OfferingCount),
		cost:           0.0,
	}
}

// UpdateNodeClaim adds a NodeClaim to cost tracking. The NodeClaim must have
// all required labels or it will be ignored and logged as an error.
func (cc *ClusterCost) UpdateNodeClaim(ctx context.Context, nodeClaim *v1.NodeClaim) {
	cc.RLock()
	_, exists := cc.nodeClaimMap[client.ObjectKeyFromObject(nodeClaim).String()]
	cc.RUnlock()
	if !exists {
		// First lets check if the right labels are there
		var missingLabels []string
		for _, key := range NecessaryLabels {
			_, exists := nodeClaim.Labels[key]
			if !exists {
				missingLabels = append(missingLabels, key)
			}
		}
		if len(missingLabels) > 0 {
			log.FromContext(ctx).Error(fmt.Errorf("nodeclaim %q is missing required labels: %v", nodeClaim.Name, missingLabels), "failed to process nodeclaim for cost tracking")
			CostTrackingErrorsTotal.Inc(map[string]string{
				metrics.NodePoolLabel: nodeClaim.Labels[v1.NodePoolLabelKey],
			})
			return
		}

		np := &v1.NodePool{}
		err := cc.client.Get(ctx, client.ObjectKey{Name: nodeClaim.Labels[v1.NodePoolLabelKey]}, np)
		if err != nil {
			log.FromContext(ctx).Error(fmt.Errorf("failed to get nodepool %q for nodeclaim %q: %w", nodeClaim.Labels[v1.NodePoolLabelKey], nodeClaim.Name, err), "failed to process nodeclaim for cost tracking")
			CostTrackingErrorsTotal.Inc(map[string]string{
				metrics.NodePoolLabel: nodeClaim.Labels[v1.NodePoolLabelKey],
			})
			return
		}
		cc.Lock()
		defer cc.Unlock()
		err = cc.internalAddOffering(ctx, np, nodeClaim.Labels[corev1.LabelInstanceTypeStable], nodeClaim.Labels[v1.CapacityTypeLabelKey], nodeClaim.Labels[corev1.LabelTopologyZone], true)
		if err != nil {
			log.FromContext(ctx).Error(fmt.Errorf("failed to add offering for nodeclaim %q in nodepool %q: %w", nodeClaim.Name, np.Name, err), "failed to process nodeclaim for cost tracking")
			CostTrackingErrorsTotal.Inc(map[string]string{
				metrics.NodePoolLabel: np.Name,
			})
			return
		}
		cc.nodeClaimMap[client.ObjectKeyFromObject(nodeClaim).String()] = true
		return
	}
}

// DeleteNodeClaim removes a NodeClaim from cost tracking. If the NodeClaim
// was not being tracked, this operation is a no-op.
func (cc *ClusterCost) DeleteNodeClaim(ctx context.Context, nodeClaim *v1.NodeClaim) {
	cc.RLock()
	_, exists := cc.nodeClaimMap[client.ObjectKeyFromObject(nodeClaim).String()]
	cc.RUnlock()
	if !exists {
		return
	}

	// First lets check if the right labels are there
	var missingLabels []string
	for _, key := range NecessaryLabels {
		_, exists = nodeClaim.Labels[key]
		if !exists {
			missingLabels = append(missingLabels, key)
		}
	}
	if len(missingLabels) > 0 {
		log.FromContext(ctx).Error(fmt.Errorf("nodeclaim %q is missing required labels: %v", nodeClaim.Name, missingLabels), "failed to remove nodeclaim from cost tracking")
		CostTrackingErrorsTotal.Inc(map[string]string{
			metrics.NodePoolLabel: nodeClaim.Labels[v1.NodePoolLabelKey],
		})
		return
	}

	nodePoolName := nodeClaim.Labels[v1.NodePoolLabelKey]
	np := &v1.NodePool{}
	err := cc.client.Get(ctx, client.ObjectKey{Name: nodePoolName}, np)
	if err != nil {
		log.FromContext(ctx).Error(fmt.Errorf("failed to get nodepool %q for nodeclaim %q: %w", nodePoolName, nodeClaim.Name, err), "failed to remove nodeclaim from cost tracking")
		CostTrackingErrorsTotal.Inc(map[string]string{
			metrics.NodePoolLabel: nodePoolName,
		})
		return
	}
	cc.Lock()
	defer cc.Unlock()
	err = cc.internalRemoveOffering(np, nodeClaim.Labels[corev1.LabelInstanceTypeStable], nodeClaim.Labels[v1.CapacityTypeLabelKey], nodeClaim.Labels[corev1.LabelTopologyZone])
	if err != nil {
		log.FromContext(ctx).Error(fmt.Errorf("failed to remove offering for nodeclaim %q in nodepool %q: %w", nodeClaim.Name, np.Name, err), "failed to remove nodeclaim from cost tracking")
		CostTrackingErrorsTotal.Inc(map[string]string{
			metrics.NodePoolLabel: np.Name,
		})
		return
	}
	delete(cc.nodeClaimMap, client.ObjectKeyFromObject(nodeClaim).String())
}

func (cc *ClusterCost) internalAddOffering(ctx context.Context, np *v1.NodePool, instanceName, capacityType, zone string, firstTry bool) error {
	_, exists := cc.npCostMap[np.Name]
	if !exists {
		// create the new npc
		instanceTypes, err := cc.cloudProvider.GetInstanceTypes(ctx, np)
		if err != nil {
			return fmt.Errorf("failed to get instance types for new nodepool %q while adding offering for instance %q: %w", np.Name, instanceName, err)
		}
		cc.createNewNodePoolCost(np, instanceTypes)
	}

	ok := OfferingKey{Capacity: capacityType, Zone: zone, InstanceName: instanceName}
	oc, exists := cc.npCostMap[np.Name].offeringCounts[ok]
	if !exists {
		var foundOffering *cloudprovider.Offering
		it, exists := cc.npCostMap[np.Name].overlayedInstanceMap[instanceName]
		if exists {
			foundOffering, exists = lo.Find(it.Offerings, func(o *cloudprovider.Offering) bool {
				return capacityType == o.CapacityType() && zone == o.Zone()
			})
		}
		if !exists {
			// our offerings must be out of date, we should update and retry
			if firstTry {
				err := cc.internalNodepoolUpdate(ctx, np)
				if err != nil {
					return fmt.Errorf("failed to update nodepool %q during retry while searching for offering for instance %q in zone %q with capacity %q: %w", np.Name, instanceName, zone, capacityType, err)
				}
				return cc.internalAddOffering(ctx, np, instanceName, capacityType, zone, false)
			} else {
				return fmt.Errorf("offering not found for instance type %q in nodepool %q after retry attempt (zone: %q, capacity: %q)", instanceName, np.Name, zone, capacityType)
			}
		}
		oc = OfferingCount{
			Count: 1,
			Cost:  foundOffering.Price,
		}
	} else {
		oc.Count += 1
	}
	cc.npCostMap[np.Name].offeringCounts[ok] = oc
	cc.npCostMap[np.Name].cost += oc.Cost
	return nil
}

func (cc *ClusterCost) internalRemoveOffering(np *v1.NodePool, instanceName, capacityType, zone string) error {
	npc, exists := cc.npCostMap[np.Name]
	if !exists {
		return fmt.Errorf("attempted to remove offering from nonexistent nodepool %q (instance: %q, zone: %q, capacity: %q)", np.Name, instanceName, zone, capacityType)
	}

	ok := OfferingKey{Capacity: capacityType, Zone: zone, InstanceName: instanceName}
	oc, exists := npc.offeringCounts[ok]
	if !exists {
		return fmt.Errorf("attempted to remove nonexistent offering from nodepool %q (instance: %q, zone: %q, capacity: %q)", np.Name, instanceName, zone, capacityType)
	}

	oc.Count -= 1
	cc.npCostMap[np.Name].offeringCounts[ok] = oc
	cc.npCostMap[np.Name].cost -= oc.Cost
	if oc.Count == 0 {
		delete(npc.offeringCounts, ok)
	}
	if len(lo.Values(npc.offeringCounts)) == 0 {
		delete(cc.npCostMap, np.Name)
	}
	return nil
}

// GetClusterCost returns the total cost of all compute resources across
// all NodePools in the cluster.
func (cc *ClusterCost) GetClusterCost() float64 {
	cc.RLock()
	defer cc.RUnlock()
	return lo.SumBy(lo.Values(cc.npCostMap), func(npc *NodePoolCost) float64 { return npc.cost })
}

// GetNodepoolCost returns the total cost of compute resources for a specific
// NodePool. Returns 0 if the NodePool is not being tracked.
func (cc *ClusterCost) GetNodepoolCost(np *v1.NodePool) float64 {
	cc.RLock()
	defer cc.RUnlock()

	npc, exists := cc.npCostMap[np.Name]
	if !exists {
		return 0
	}
	return npc.cost
}
