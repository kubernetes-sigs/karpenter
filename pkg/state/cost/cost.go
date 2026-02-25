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

package cost

import (
	"context"
	"fmt"
	"sync"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
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
	// nodeClaimSet tracks which NodeClaims are currently being monitored for cost
	nodeClaimMap map[types.NamespacedName]NodeClaimMetaData // nodeClaim object key -> NodeClaimMetaData

	cloudProvider cloudprovider.CloudProvider
	client        client.Client
}

// NodePoolCost represents the cost tracking information for a single NodePool.
// It maintains the current cost, available instance types, and count of active offerings.
type NodePoolCost struct {
	cost float64
	// offeringCounts tracks how many instances of each offering type are currently active
	offeringCounts map[OfferingKey]OfferingCount
}

// OfferingKey uniquely identifies a specific compute offering by its zone,
// capacity type (e.g., spot/on-demand), and instance type name.
// This is not a hard invariant
type OfferingKey struct {
	Zone, CapacityType, InstanceName string
}

// OfferingCount tracks the number and cost of instances for a specific offering.
type OfferingCount struct {
	Count int
	Price float64 // Price of the offering, not Price * count
}

type NodeClaimMetaData struct {
	NodePoolName string
	NodeClaimKey OfferingKey
}

// NewClusterCost creates and initializes a new ClusterCost instance for tracking
// compute costs across the cluster. It requires a cloud provider for accessing
// instance type and pricing information, and a Kubernetes client for NodePool loofferingKeyups.
func NewClusterCost(ctx context.Context, cloudProvider cloudprovider.CloudProvider, client client.Client) *ClusterCost {
	return &ClusterCost{
		npCostMap:     make(map[string]*NodePoolCost),
		nodeClaimMap:  make(map[types.NamespacedName]NodeClaimMetaData),
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
func (cc *ClusterCost) UpdateOfferings(ctx context.Context, np *v1.NodePool, instanceTypes []*cloudprovider.InstanceType) {
	cc.Lock()
	defer cc.Unlock()
	cc.internalUpdateOfferings(np, instanceTypes)
}

func (cc *ClusterCost) internalNodepoolUpdate(ctx context.Context, np *v1.NodePool) error {
	instanceTypes, err := cc.cloudProvider.GetInstanceTypes(ctx, np)
	if err != nil {
		return fmt.Errorf("failed to get instance types for nodepool %q, %w", np.Name, err)
	}
	cc.internalUpdateOfferings(np, instanceTypes)
	return nil
}

func (cc *ClusterCost) internalUpdateOfferings(np *v1.NodePool, instanceTypes []*cloudprovider.InstanceType) {
	npCost, exists := cc.npCostMap[np.Name]

	if !exists {
		cc.createNewNodePoolCost(np.Name, instanceTypes)
	} else {
		instanceTypes = lo.Filter(instanceTypes, func(it *cloudprovider.InstanceType, _ int) bool {
			return it != nil
		})
		newMap := map[OfferingKey]OfferingCount{}
		for _, it := range instanceTypes {
			for _, o := range it.Offerings {
				offeringKey := OfferingKey{InstanceName: it.Name, Zone: o.Zone(), CapacityType: o.CapacityType()}
				oldCount, exists := npCost.offeringCounts[offeringKey]
				newMap[offeringKey] = OfferingCount{
					Count: lo.Ternary(exists, oldCount.Count, 0),
					Price: o.Price,
				}
			}
		}
		// Add back all of the offering counts that don't exist in the new instance types
		// This can't occur on container restart, so we may lose cost data from offerings that are no longer returned
		// from the cloud provider but still have nodeclaims.
		for key, count := range npCost.offeringCounts {
			_, exists := newMap[key]
			if !exists {
				newMap[key] = count
			}
		}

		npCost.offeringCounts = newMap
		// re-calculate the cost as the instances have changed
		cost := npCost.updateCost()
		cc.npCostMap[np.Name].cost = cost
	}
}

func (npc *NodePoolCost) updateCost() float64 {
	cost := 0.0
	for _, oc := range npc.offeringCounts {
		// add the new price times the count of that offering
		cost = cost + (float64(oc.Count) * oc.Price)
	}
	return cost
}

func (cc *ClusterCost) createNewNodePoolCost(npName string, instanceTypes []*cloudprovider.InstanceType) {
	// create the new npc
	cc.npCostMap[npName] = &NodePoolCost{
		offeringCounts: make(map[OfferingKey]OfferingCount),
		cost:           0.0,
	}
	for _, it := range instanceTypes {
		for _, o := range it.Offerings {
			cc.npCostMap[npName].offeringCounts[OfferingKey{InstanceName: it.Name, Zone: o.Zone(), CapacityType: o.CapacityType()}] = OfferingCount{
				Count: 0,
				Price: o.Price,
			}
		}
	}
}

// UpdateNodeClaim adds a NodeClaim to cost tracking. The NodeClaim must have
// all required labels or it will be ignored and logged as an error.
func (cc *ClusterCost) UpdateNodeClaim(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	cc.RLock()
	_, exists := cc.nodeClaimMap[client.ObjectKeyFromObject(nodeClaim)]
	cc.RUnlock()

	if exists {
		return nil
	}

	failed := false
	defer func() {
		if failed {
			CostTrackingErrorsTotal.Inc(map[string]string{
				metrics.NodePoolLabel: nodeClaim.Labels[v1.NodePoolLabelKey],
			})
		}
	}()

	// First lets check if the right labels are there
	if nodeClaimMissingLabels(*nodeClaim) {
		// not technically a failure mode as we expect to retry once the
		// labels are propagated
		return nil
	}

	nodePoolName := nodeClaim.Labels[v1.NodePoolLabelKey]
	offeringKey := OfferingKey{CapacityType: nodeClaim.Labels[v1.CapacityTypeLabelKey], Zone: nodeClaim.Labels[corev1.LabelTopologyZone], InstanceName: nodeClaim.Labels[corev1.LabelInstanceTypeStable]}

	cc.Lock()
	defer cc.Unlock()
	err := cc.internalAddOffering(ctx, nodePoolName, offeringKey)
	if err != nil {
		failed = true
		return serrors.Wrap(err, "nodeclaim", klog.KObj(nodeClaim), "nodepool", nodePoolName)
	}
	cc.nodeClaimMap[client.ObjectKeyFromObject(nodeClaim)] = NodeClaimMetaData{
		NodePoolName: nodePoolName,
		NodeClaimKey: offeringKey,
	}
	return nil
}

// DeleteNodeClaim removes a NodeClaim from cost tracking. If the NodeClaim
// was not being tracked, this operation is a no-op.
func (cc *ClusterCost) DeleteNodeClaim(ctx context.Context, nn types.NamespacedName) error {
	cc.RLock()
	metadata, exists := cc.nodeClaimMap[nn]
	cc.RUnlock()

	if !exists {
		return nil
	}

	failed := false
	defer func() {
		if failed {
			CostTrackingErrorsTotal.Inc(map[string]string{
				metrics.NodePoolLabel: metadata.NodePoolName,
			})
		}
	}()

	cc.Lock()
	defer cc.Unlock()

	err := cc.internalRemoveOffering(metadata.NodePoolName, metadata.NodeClaimKey)
	if err != nil {
		failed = true
		return serrors.Wrap(err, "namespacedName", nn, "nodepool", metadata.NodePoolName)
	}

	// If it succeeds, we can remove the metadata
	delete(cc.nodeClaimMap, nn)
	return nil
}

func (cc *ClusterCost) DeleteNodePool(ctx context.Context, npName string) {
	cc.Lock()
	defer cc.Unlock()

	cc.nodeClaimMap = lo.PickBy(cc.nodeClaimMap, func(_ types.NamespacedName, metadata NodeClaimMetaData) bool {
		return metadata.NodePoolName != npName
	})
	delete(cc.npCostMap, npName)
}

// internalAddOffering updates the internal clusterCost state to include a new offering for a given nodepool.
// It is used to increment the overall cost when a node joins the cluster. It is only called by UpdateNodeClaim
// after that function has determined if a nodeclaim is new.
func (cc *ClusterCost) internalAddOffering(ctx context.Context, npName string, offeringKey OfferingKey) error {
	np := &v1.NodePool{}
	if err := cc.client.Get(ctx, client.ObjectKey{Name: npName}, np, &client.GetOptions{}); err != nil {
		return err
	}

	_, exists := cc.npCostMap[npName]
	if !exists {
		// create the new npc
		instanceTypes, err := cc.cloudProvider.GetInstanceTypes(ctx, np)
		if err != nil {
			return fmt.Errorf("failed to get instance types for new nodepool %q while adding offering for instance %q, %w", np.Name, offeringKey.InstanceName, err)
		}
		cc.createNewNodePoolCost(npName, instanceTypes)
	}

	oc, exists := cc.npCostMap[npName].offeringCounts[offeringKey]
	if !exists {
		// our offerings must be out of date, we should update and retry
		err := cc.internalNodepoolUpdate(ctx, np)
		if err != nil {
			return fmt.Errorf("failed to update nodepool %q during retry while searching for offering for instance %q in zone %q with capacity %q, %w", np.Name, offeringKey.InstanceName, offeringKey.Zone, offeringKey.CapacityType, err)
		}
		oc, exists = cc.npCostMap[npName].offeringCounts[offeringKey]
		if !exists {
			oc = OfferingCount{Count: 1, Price: 0.0}
			log.FromContext(ctx).Error(fmt.Errorf("failed to find offering %q during retry while searching for instance %q in zone %q with capacity %q in nodepool %q", offeringKey, offeringKey.InstanceName, offeringKey.Zone, offeringKey.CapacityType, npName), "offering won't be counted towards total cluster cost")
		}
	} else {
		oc.Count += 1
	}
	cc.npCostMap[npName].offeringCounts[offeringKey] = oc
	cc.npCostMap[npName].cost += oc.Price
	return nil
}

// internalRemoveOffering updates the internal clusterCost state to remove an existing offering for a given nodepool.
// It is used to decrement the overall cost when a node leeaves the cluster. It is only called by DeleteNodeClaim
// after that function has determined if a nodeclaim is already being accounted for.
func (cc *ClusterCost) internalRemoveOffering(npName string, offeringKey OfferingKey) error {
	npc, exists := cc.npCostMap[npName]
	if !exists {
		return fmt.Errorf("attempted to remove offering from nonexistent nodepool %q (instance, %q, zone, %q, capacity, %q)", npName, offeringKey.InstanceName, offeringKey.Zone, offeringKey.CapacityType)
	}

	oc, exists := npc.offeringCounts[offeringKey]
	if !exists {
		return fmt.Errorf("attempted to remove nonexistent offering from nodepool %q (instance, %q, zone, %q, capacity, %q)", npName, offeringKey.InstanceName, offeringKey.Zone, offeringKey.CapacityType)
	}

	oc.Count -= 1
	npc.offeringCounts[offeringKey] = oc
	npc.cost -= oc.Price
	if oc.Count == 0 {
		delete(npc.offeringCounts, offeringKey)
	}
	if len(lo.Values(npc.offeringCounts)) == 0 {
		delete(cc.npCostMap, npName)
	}
	return nil
}

func (cc *ClusterCost) Reset() {
	cc.Lock()
	defer cc.Unlock()
	cc.npCostMap = make(map[string]*NodePoolCost)
	cc.nodeClaimMap = make(map[types.NamespacedName]NodeClaimMetaData)
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

func nodeClaimMissingLabels(nc v1.NodeClaim) bool {
	var missingLabels []string
	for _, key := range NecessaryLabels {
		_, exists := nc.Labels[key]
		if !exists {
			missingLabels = append(missingLabels, key)
		}
	}
	return len(missingLabels) > 0
}
