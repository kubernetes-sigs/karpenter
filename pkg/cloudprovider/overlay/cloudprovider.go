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

package overlay

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/samber/lo"

	"github.com/awslabs/operatorpkg/serrors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

type decorator struct {
	cloudprovider.CloudProvider
	kubeClient client.Client
	store      *nodeoverlay.InstanceTypeStore
}

// Decorate returns a new `CloudProvider` instance that will delegate the GetInstanceTypes
// calls to the argument, `cloudProvider`, and provide instance types with NodeOverlays applied to them. The
func Decorate(cloudProvider cloudprovider.CloudProvider, kubeClient client.Client, store *nodeoverlay.InstanceTypeStore) cloudprovider.CloudProvider {
	return &decorator{CloudProvider: cloudProvider, kubeClient: kubeClient, store: store}
}

func (d *decorator) GetInstanceTypes(ctx context.Context, nodePool *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	its, err := d.CloudProvider.GetInstanceTypes(ctx, nodePool)
	if err != nil {
		return []*cloudprovider.InstanceType{}, serrors.Wrap(fmt.Errorf("getting cloud provider instance types, %w", err), "NodePool", klog.KObj(nodePool))
	}
	if options.FromContext(ctx).FeatureGates.NodeOverlay {
		its, err = d.store.ApplyAll(nodePool.Name, its)
		if err != nil {
			return []*cloudprovider.InstanceType{}, fmt.Errorf("applying nodeoverlays, %w", err)
		}
	}
	return its, nil
}

func (d *decorator) RefreshNodeClaimStates(ctx context.Context, claims []*v1.NodeClaim) (map[string]cloudprovider.NodeClaimState, error) {
	states, err := cloudprovider.RefreshNodeClaimStates(ctx, d.CloudProvider, claims)
	states = maps.Clone(states)
	if !options.FromContext(ctx).FeatureGates.NodeOverlay {
		return states, err
	}
	for _, nc := range claims {
		s, ok := states[nc.Status.ProviderID]
		if !ok || s.ObservedAt.IsZero() {
			continue
		}
		labels := lo.Assign(nc.Labels)
		// Observed absence clears the previous reservation tuple before matching
		// overlays. A stale reservation-ID label must not price an uncovered node.
		for key := range cloudprovider.ReservedCapacityLabels {
			delete(labels, key)
		}
		maps.Copy(labels, s.Labels)
		price, overlayErr := d.store.ApplyNodeClaimPrice(labels, s.Price)
		if overlayErr != nil {
			states[nc.Status.ProviderID] = cloudprovider.NodeClaimState{Scope: s.Scope}
			err = errors.Join(err, overlayErr)
			continue
		}
		s.Price = price
		states[nc.Status.ProviderID] = s
	}
	return states, err
}
