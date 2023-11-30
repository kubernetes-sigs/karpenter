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

package scheduling

import (
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/ptr"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// NodeClaimTemplate encapsulates the fields required to create a node and mirrors
// the fields in NodePool. These structs are maintained separately in order
// for fields like Requirements to be able to be stored more efficiently.
type NodeClaimTemplate struct {
	v1beta1.NodeClaimTemplate

	NodePoolName        string
	InstanceTypeOptions cloudprovider.InstanceTypes
	Requirements        scheduling.Requirements
}

func NewNodeClaimTemplate(nodePool *v1beta1.NodePool) *NodeClaimTemplate {
	nct := &NodeClaimTemplate{
		NodeClaimTemplate: nodePool.Spec.Template,
		NodePoolName:      nodePool.Name,
		Requirements:      scheduling.NewRequirements(),
	}
	nct.Labels = lo.Assign(nct.Labels, map[string]string{v1beta1.NodePoolLabelKey: nodePool.Name})
	nct.Requirements.Add(scheduling.NewNodeSelectorRequirements(nct.Spec.Requirements...).Values()...)
	nct.Requirements.Add(scheduling.NewLabelRequirements(nct.Labels).Values()...)
	return nct
}

func (i *NodeClaimTemplate) ToNodeClaim(nodePool *v1beta1.NodePool) *v1beta1.NodeClaim {
	// Order the instance types by price and only take the first 100 of them to decrease the instance type size in the requirements
	instanceTypes := lo.Slice(i.InstanceTypeOptions.OrderByPrice(i.Requirements), 0, 100)
	i.Requirements.Add(scheduling.NewRequirement(v1.LabelInstanceTypeStable, v1.NodeSelectorOpIn, lo.Map(instanceTypes, func(i *cloudprovider.InstanceType, _ int) string {
		return i.Name
	})...))

	nc := &v1beta1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", i.NodePoolName),
			Annotations:  lo.Assign(i.Annotations, map[string]string{v1beta1.NodePoolHashAnnotationKey: nodePool.Hash()}),
			Labels:       i.Labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         v1beta1.SchemeGroupVersion.String(),
					Kind:               "NodePool",
					Name:               nodePool.Name,
					UID:                nodePool.UID,
					BlockOwnerDeletion: ptr.Bool(true),
				},
			},
		},
		Spec: i.Spec,
	}
	nc.Spec.Requirements = i.Requirements.NodeSelectorRequirements()
	return nc
}
