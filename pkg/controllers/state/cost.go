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

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

var NecessaryLabels = []string{corev1.LabelInstanceTypeStable, v1.CapacityTypeLabelKey, corev1.LabelTopologyZone}

// This cost store is an alpha store. Do not depend on it, as it may change
type ClusterCost struct {
	sync.RWMutex
	npCostMap    map[string]*NodePoolCost
	nodeClaimMap map[string]bool

	// We need to get all the pre-node overlay instance types for modification when overlays are
	cloudProvider cloudprovider.CloudProvider
	client        client.Client
}

type NodePoolCost struct {
	cost                 float64
	overlayedInstanceMap map[string]*cloudprovider.InstanceType
	offeringCounts       map[OfferingKey]OfferingCount
}

type OfferingKey struct {
	zone, capacity, instanceName string
}

type OfferingCount struct {
	Count int
	Cost  float64
}

func NewClusterCost(ctx context.Context, cloudprovider cloudprovider.CloudProvider, client client.Client) *ClusterCost {
	return &ClusterCost{
		npCostMap:     make(map[string]*NodePoolCost),
		nodeClaimMap:  make(map[string]bool),
		cloudProvider: cloudprovider,
		client:        client,
	}
}

func (cc *ClusterCost) UpdateOfferings(ctx context.Context, np *v1.NodePool, instanceTypes []*cloudprovider.InstanceType) {
	cc.Lock()
	defer cc.Unlock()
	npCost, exists := cc.npCostMap[np.Name]

	if !exists {
		cc.createNewNodePoolCost(np, instanceTypes)
	} else {
		npCost.overlayedInstanceMap = lo.SliceToMap(instanceTypes, func(it *cloudprovider.InstanceType) (string, *cloudprovider.InstanceType) {
			return it.Name, it
		})
		// re-calculate the cost as the instances have changed
		cc.npCostMap[np.Name].cost = npCost.UpdateCost()
	}
}

func (npc *NodePoolCost) UpdateCost() float64 {
	cost := 0.0
	for ok, oc := range npc.offeringCounts {
		overlayedInstanceType := npc.overlayedInstanceMap[ok.instanceName]
		// get the offering price from the overlayed instance type
		newOffering, exists := lo.Find(overlayedInstanceType.Offerings, func(o *cloudprovider.Offering) bool {
			return o.CapacityType() == ok.capacity && o.Zone() == ok.zone
		})
		if !exists {
			// todo
			panic("Couldn't find an existing offering in the new set of offerings.")
		}
		// add the new price times the count of that offering
		cost = cost + (float64(oc.Count) * newOffering.Price)
		//oc.Cost = newOffering.Price
	}
	return cost
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

func (cc *ClusterCost) UpdateNodeClaim(ctx context.Context, nodeClaim v1.NodeClaim) {
	cc.RLock()
	_, exists := cc.nodeClaimMap[client.ObjectKeyFromObject(&nodeClaim).String()]
	cc.RUnlock()
	if !exists {
		// First lets check if the right labels are there
		for _, key := range NecessaryLabels {
			_, exists := nodeClaim.Labels[key]
			if !exists {
				log.FromContext(ctx).Error(fmt.Errorf("getting node details from nodeclaim"), "nodeclaim", nodeClaim.Name)
				return
			}
		}
		np := &v1.NodePool{}
		err := cc.client.Get(ctx, client.ObjectKey{Name: nodeClaim.Labels[v1.NodePoolLabelKey]}, np)
		if err != nil {
			log.FromContext(ctx).Error(fmt.Errorf("getting nodepool from nodeclaim"), "nodeclaim", nodeClaim.Name)
			return
		}
		err = cc.AddOffering(ctx, np, nodeClaim.Labels[corev1.LabelInstanceTypeStable], nodeClaim.Labels[v1.CapacityTypeLabelKey], nodeClaim.Labels[corev1.LabelTopologyZone])
		if err != nil {
			// todo
			return
		}
		cc.Lock()
		cc.nodeClaimMap[client.ObjectKeyFromObject(&nodeClaim).String()] = true
		cc.Unlock()
		return
	}
}

func (cc *ClusterCost) RemoveNodeClaim(ctx context.Context, nodeClaim v1.NodeClaim) {
	cc.RLock()
	_, exists := cc.nodeClaimMap[client.ObjectKeyFromObject(&nodeClaim).String()]
	cc.RUnlock()
	if !exists {
		return
	} else {
		// First lets check if the right labels are there
		for _, key := range NecessaryLabels {
			_, exists := nodeClaim.Labels[key]
			if !exists {
				log.FromContext(ctx).Error(fmt.Errorf("getting node details from nodeclaim"), "nodeclaim", nodeClaim.Name)
				return
			}
		}
		np := &v1.NodePool{}
		err := cc.client.Get(ctx, client.ObjectKey{Name: nodeClaim.Labels[v1.NodePoolLabelKey]}, np)
		if err != nil {
			log.FromContext(ctx).Error(fmt.Errorf("getting nodepool from nodeclaim"), "nodeclaim", nodeClaim.Name)
			return
		}
		err = cc.RemoveOffering(ctx, np, nodeClaim.Labels[corev1.LabelInstanceTypeStable], nodeClaim.Labels[v1.CapacityTypeLabelKey], nodeClaim.Labels[corev1.LabelTopologyZone])
		if err != nil {
			//todo
			return
		}
		cc.Lock()
		delete(cc.nodeClaimMap, client.ObjectKeyFromObject(&nodeClaim).String())
		cc.Unlock()
		return
	}
}

func (cc *ClusterCost) AddOffering(ctx context.Context, np *v1.NodePool, instanceName, capacityType, zone string) error {
	cc.Lock()
	defer cc.Unlock()

	_, exists := cc.npCostMap[np.Name]
	if !exists {
		// create the new npc
		instanceTypes, err := cc.cloudProvider.GetInstanceTypes(ctx, np)
		if err != nil {
			//todo
			return nil
		}
		cc.createNewNodePoolCost(np, instanceTypes)

	}
	ok := OfferingKey{capacity: capacityType, zone: zone, instanceName: instanceName}
	oc, exists := cc.npCostMap[np.Name].offeringCounts[ok]
	if !exists {
		foundOffering, exists := lo.Find(cc.npCostMap[np.Name].overlayedInstanceMap[instanceName].Offerings, func(o *cloudprovider.Offering) bool {
			return capacityType == o.CapacityType() && zone == o.Zone()
		})
		if !exists {
			// our offerings must be out of date, we should update
			instanceTypes, err := cc.cloudProvider.GetInstanceTypes(ctx, np)
			if err != nil {
				//todo
				return nil
			}
			cc.Unlock()
			fmt.Printf("Updating Offerings\n")
			cc.UpdateOfferings(ctx, np, instanceTypes)
			cc.Lock()
			// check again to see if it worked
			_, exists := lo.Find(cc.npCostMap[np.Name].overlayedInstanceMap[instanceName].Offerings, func(o *cloudprovider.Offering) bool {
				return o.CapacityType() == ok.capacity && o.Zone() == ok.zone
			})
			if !exists {
				//throw irrecoverable error, we shouldn't be able to get here
				return nil
			} else {
				// okay it now exists, lets retry
				fmt.Printf("Retryin\n")
				cc.Unlock()
				return cc.AddOffering(ctx, np, instanceName, capacityType, zone)
			}
		}
		oc = OfferingCount{
			Count: 1,
			Cost:  foundOffering.Price,
		}
	} else {
		oc.Count += oc.Count
	}
	cc.npCostMap[np.Name].offeringCounts[ok] = oc
	cc.npCostMap[np.Name].cost += oc.Cost
	return nil
}

func (cc *ClusterCost) RemoveOffering(ctx context.Context, np *v1.NodePool, instanceName, capacityType, zone string) error {
	cc.Lock()
	defer cc.Unlock()

	npc, exists := cc.npCostMap[np.Name]
	if !exists {
		// throw irrecoverable error, we shouldn't be able to get here
		return nil

	}
	ok := OfferingKey{capacity: capacityType, zone: zone, instanceName: instanceName}
	oc, exists := npc.offeringCounts[ok]
	if !exists {
		// throw irrecoverable error, we shouldn't be able to get here
		return nil
	}
	oc.Count -= 1
	cc.npCostMap[np.Name].offeringCounts[ok] = oc
	cc.npCostMap[np.Name].cost -= oc.Cost
	if oc.Count == 0 {
		delete(npc.offeringCounts, ok)
	}
	return nil
}

func (cc *ClusterCost) GetClusterCost() float64 {
	cc.RLock()
	defer cc.RUnlock()
	return lo.SumBy(lo.Values(cc.npCostMap), func(npc *NodePoolCost) float64 { return npc.cost })
}

func (cc *ClusterCost) GetNodepoolCost(np *v1.NodePool) float64 {
	cc.RLock()
	defer cc.RUnlock()

	npc, exists := cc.npCostMap[np.Name]
	if !exists {
		return 0
	}
	return npc.cost
}
