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
	"fmt"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

// Create delegates to the cloud provider. When a NodeOverlay sets maxCapacity for the launched instance type, Create
// then corrects the returned status, so that it reports the chosen amount of each flexible resource instead of the
// ceiling. Cloud providers therefore do not have to do this themselves.
func (d *decorator) Create(ctx context.Context, nodeClaim *v1.NodeClaim) (*v1.NodeClaim, error) {
	if !options.FromContext(ctx).FeatureGates.NodeOverlay {
		return d.CloudProvider.Create(ctx, nodeClaim)
	}
	// Load what the correction needs before the launch. If that fails, no instance exists yet, so Create can return
	// the error and Karpenter retries the launch. Otherwise the NodeClaim would keep an uncorrected status.
	inputs, err := d.flexibleCapacityInputs(ctx, nodeClaim)
	if err != nil {
		return nil, err
	}
	created, err := d.CloudProvider.Create(ctx, nodeClaim)
	if err != nil || inputs == nil {
		return created, err
	}
	// The instance exists now. If Create returned an error, Karpenter would launch again and leak this instance, so a
	// failed correction is only logged. The correction uses only the loaded inputs, so it is not expected to fail.
	if err := d.correctFlexibleCapacity(inputs, created); err != nil {
		log.FromContext(ctx).Error(err, "failed setting flexible capacity in nodeclaim status", "NodeClaim", klog.KObj(nodeClaim))
	}
	return created, nil
}

// flexibleCapacityInputs holds what correctFlexibleCapacity needs, loaded before the launch.
type flexibleCapacityInputs struct {
	nodePoolName  string
	flexible      corev1.ResourceList
	instanceTypes []*cloudprovider.InstanceType
}

// flexibleCapacityInputs returns nil when the NodeClaim needs no correction: it has no amounts in the annotation, and
// no NodeOverlay sets maxCapacity in its NodePool.
func (d *decorator) flexibleCapacityInputs(ctx context.Context, nodeClaim *v1.NodeClaim) (*flexibleCapacityInputs, error) {
	nodePoolName, ok := nodeClaim.Labels[v1.NodePoolLabelKey]
	if !ok {
		return nil, nil
	}
	// The annotation holds the amounts that the cloud provider gives the node. The NodeOverlays may have changed since
	// Karpenter chose them, so the annotation decides whether to correct the status, not only the current overlays.
	flexible, err := cloudprovider.FlexibleCapacityFromNodeClaim(nodeClaim)
	if err != nil {
		return nil, err
	}
	if len(flexible) == 0 && !d.store.NodePoolHasFlexibleCapacity(nodePoolName) {
		return nil, nil
	}
	nodePool := &v1.NodePool{}
	if err := d.kubeClient.Get(ctx, client.ObjectKey{Name: nodePoolName}, nodePool); err != nil {
		return nil, fmt.Errorf("getting nodepool, %w", err)
	}
	its, err := d.CloudProvider.GetInstanceTypes(ctx, nodePool)
	if err != nil {
		return nil, fmt.Errorf("getting cloud provider instance types, %w", err)
	}
	return &flexibleCapacityInputs{nodePoolName: nodePoolName, flexible: flexible, instanceTypes: its}, nil
}

// correctFlexibleCapacity sets the chosen amount of each flexible resource in the status of the launched NodeClaim.
func (d *decorator) correctFlexibleCapacity(inputs *flexibleCapacityInputs, created *v1.NodeClaim) error {
	instanceTypeName, ok := created.Labels[corev1.LabelInstanceTypeStable]
	if !ok {
		return nil
	}
	nodePoolName, flexible := inputs.nodePoolName, inputs.flexible
	if len(flexible) == 0 && !d.store.HasFlexibleCapacity(nodePoolName, instanceTypeName) {
		return nil
	}
	it, ok := lo.Find(inputs.instanceTypes, func(it *cloudprovider.InstanceType) bool { return it.Name == instanceTypeName })
	if !ok {
		return serrors.Wrap(fmt.Errorf("instance type not found"), "instance-type", instanceTypeName)
	}
	overlaid, err := d.store.Apply(nodePoolName, it)
	switch {
	case cloudprovider.IsUnevaluatedNodePoolError(err):
		// The NodeOverlay controller has not evaluated the NodePool yet, for example right after Karpenter starts. Use
		// the instance type without NodeOverlays. The status then still reports the amounts in the annotation.
	case err != nil:
		return fmt.Errorf("applying nodeoverlays, %w", err)
	default:
		it = overlaid
	}
	created.Status.Capacity, created.Status.Allocatable = cloudprovider.FlexibleCapacityStatus(it, created.Status.Capacity, created.Status.Allocatable, flexible)
	return nil
}
