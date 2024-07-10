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

package kwok

import (
	"context"
	_ "embed"
	"fmt"
	"math/rand"
	"strings"

	"github.com/awslabs/operatorpkg/status"

	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func NewCloudProvider(ctx context.Context, kubeClient client.Client, instanceTypes []*cloudprovider.InstanceType) *CloudProvider {
	return &CloudProvider{
		kubeClient:    kubeClient,
		instanceTypes: instanceTypes,
	}
}

type CloudProvider struct {
	kubeClient    client.Client
	instanceTypes []*cloudprovider.InstanceType
}

func (c CloudProvider) Create(ctx context.Context, nodeClaim *v1.NodeClaim) (*v1.NodeClaim, error) {
	// Create the Node because KwoK nodes don't have a kubelet, which is what Karpenter normally relies on to create the node.
	node, err := c.toNode(nodeClaim)
	if err != nil {
		return nil, fmt.Errorf("translating nodeclaim to node, %w", err)
	}
	if err := c.kubeClient.Create(ctx, node); err != nil {
		return nil, fmt.Errorf("creating node, %w", err)
	}
	// convert the node back into a node claim to get the chosen resolved requirement values.
	return c.toNodeClaim(node)
}

func (c CloudProvider) Delete(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("deleting node, %w", cloudprovider.NewNodeClaimNotFoundError(err))
		}
		return fmt.Errorf("deleting node, %w", err)
	}
	return nil
}

func (c CloudProvider) Get(ctx context.Context, providerID string) (*v1.NodeClaim, error) {
	nodeName := strings.Replace(providerID, kwokProviderPrefix, "", -1)
	node := &corev1.Node{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("finding node, %w", cloudprovider.NewNodeClaimNotFoundError(err))
		}
		return nil, fmt.Errorf("finding node, %w", err)
	}
	if node.DeletionTimestamp != nil {
		return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("nodeclaim not found"))
	}
	return c.toNodeClaim(node)
}

func (c CloudProvider) List(ctx context.Context) ([]*v1.NodeClaim, error) {
	nodeList := &corev1.NodeList{}
	if err := c.kubeClient.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes, %w", err)
	}
	var nodeClaims []*v1.NodeClaim
	for i, node := range nodeList.Items {
		if !strings.HasPrefix(node.Spec.ProviderID, kwokProviderPrefix) {
			continue
		}
		nc, err := c.toNodeClaim(&nodeList.Items[i])
		if err != nil {
			return nil, fmt.Errorf("converting nodeclaim, %w", err)
		}
		nodeClaims = append(nodeClaims, nc)
	}

	return nodeClaims, nil
}

// Return the hard-coded instance types.
func (c CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	return c.instanceTypes, nil
}

// Return nothing since there's no cloud provider drift.
func (c CloudProvider) IsDrifted(ctx context.Context, nodeClaim *v1.NodeClaim) (cloudprovider.DriftReason, error) {
	return "", nil
}

func (c CloudProvider) Name() string {
	return "kwok"
}

func (c CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.KWOKNodeClass{}}
}

func (c CloudProvider) getInstanceType(instanceTypeName string) (*cloudprovider.InstanceType, error) {
	it, found := lo.Find(c.instanceTypes, func(it *cloudprovider.InstanceType) bool {
		return it.Name == instanceTypeName
	})
	if !found {
		return nil, fmt.Errorf("unable to find instance type %q", instanceTypeName)
	}
	return it, nil
}

func (c CloudProvider) toNode(nodeClaim *v1.NodeClaim) (*corev1.Node, error) {
	newName := strings.Replace(namesgenerator.GetRandomName(0), "_", "-", -1)
	//nolint
	newName = fmt.Sprintf("%s-%d", newName, rand.Uint32())

	requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	req, found := lo.Find(nodeClaim.Spec.Requirements, func(req v1.NodeSelectorRequirementWithMinValues) bool {
		return req.Key == corev1.LabelInstanceTypeStable
	})
	if !found {
		return nil, fmt.Errorf("instance type requirement not found")
	}

	var instanceType *cloudprovider.InstanceType
	var cheapestOffering *cloudprovider.Offering
	// Loop through instance type values, as the node claim will only have the In operator.
	for _, val := range req.Values {
		it, err := c.getInstanceType(val)
		if err != nil {
			return nil, fmt.Errorf("instance type %s not found", val)
		}

		availableOfferings := it.Offerings.Available().Compatible(requirements)

		offeringsByPrice := lo.GroupBy(availableOfferings, func(of cloudprovider.Offering) float64 { return of.Price })
		minOfferingPrice := lo.Min(lo.Keys(offeringsByPrice))
		if cheapestOffering == nil || minOfferingPrice < cheapestOffering.Price {
			cheapestOffering = lo.ToPtr(lo.Sample(offeringsByPrice[minOfferingPrice]))
			instanceType = it
		}
	}

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        newName,
			Labels:      addInstanceLabels(nodeClaim.Labels, instanceType, nodeClaim, cheapestOffering),
			Annotations: addKwokAnnotation(nodeClaim.Annotations),
		},
		Spec: corev1.NodeSpec{
			ProviderID: kwokProviderPrefix + newName,
			Taints:     []corev1.Taint{v1.UnregisteredNoExecuteTaint},
		},
		Status: corev1.NodeStatus{
			Capacity:    instanceType.Capacity,
			Allocatable: instanceType.Allocatable(),
			Phase:       corev1.NodePending,
		},
	}, nil
}

func addInstanceLabels(labels map[string]string, instanceType *cloudprovider.InstanceType, nodeClaim *v1.NodeClaim, offering *cloudprovider.Offering) map[string]string {
	ret := make(map[string]string, len(labels))
	// start with labels on the nodeclaim
	for k, v := range labels {
		ret[k] = v
	}

	// add the derived nodeclaim requirement labels
	for _, r := range nodeClaim.Spec.Requirements {
		if len(r.Values) == 1 && r.Operator == corev1.NodeSelectorOpIn {
			ret[r.Key] = r.Values[0]
		}
	}

	// ensure we have an instance type and then any instance type requirements
	ret[corev1.LabelInstanceTypeStable] = instanceType.Name
	for _, r := range instanceType.Requirements {
		if r.Len() == 1 && r.Operator() == corev1.NodeSelectorOpIn {
			ret[r.Key] = r.Values()[0]
		}
	}
	// add in github.com/awslabs/eks-node-viewer label so that it shows up.
	ret[v1alpha1.NodeViewerLabelKey] = fmt.Sprintf("%f", offering.Price)
	// Kwok has some scalability limitations.
	// Randomly add each new node to one of the pre-created kwokPartitions.

	ret[v1alpha1.KwokPartitionLabelKey] = lo.Sample(kwokPartitions)
	ret[v1.CapacityTypeLabelKey] = offering.Requirements.Get(v1.CapacityTypeLabelKey).Any()
	ret[corev1.LabelTopologyZone] = offering.Requirements.Get(corev1.LabelTopologyZone).Any()
	ret[corev1.LabelHostname] = nodeClaim.Name

	ret[v1alpha1.KwokLabelKey] = v1alpha1.KwokLabelValue
	return ret
}

func addKwokAnnotation(annotations map[string]string) map[string]string {
	ret := make(map[string]string, len(annotations)+1)
	for k, v := range annotations {
		ret[k] = v
	}
	ret[v1alpha1.KwokLabelKey] = v1alpha1.KwokLabelValue
	return ret
}

func (c CloudProvider) toNodeClaim(node *corev1.Node) (*v1.NodeClaim, error) {
	return &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        node.Name,
			Labels:      node.Labels,
			Annotations: addKwokAnnotation(node.Annotations),
		},
		Spec: v1.NodeClaimSpec{
			Taints:        nil,
			StartupTaints: nil,
			Requirements:  nil,
			Resources:     v1.ResourceRequirements{},
			NodeClassRef:  nil,
		},
		Status: v1.NodeClaimStatus{
			NodeName:    node.Name,
			ProviderID:  node.Spec.ProviderID,
			Capacity:    node.Status.Capacity,
			Allocatable: node.Status.Allocatable,
		},
	}, nil
}
