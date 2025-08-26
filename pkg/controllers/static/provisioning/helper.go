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

package static

import (
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
)

func GetStaticNodeClaimsToProvision(
	np *v1.NodePool,
	instanceTypes []*cloudprovider.InstanceType,
	count int64,
) []*scheduling.NodeClaim {
	nodeClaims := make([]*scheduling.NodeClaim, 0, count)
	for range count {
		nct := GetStaticNodeClaimTemplate(np, instanceTypes)
		nodeClaims = append(nodeClaims, &scheduling.NodeClaim{
			NodeClaimTemplate: *nct,
			IsStaticNode:      true,
		})
	}
	return nodeClaims
}

func GetStaticNodeClaimTemplate(np *v1.NodePool, instanceTypes []*cloudprovider.InstanceType) *scheduling.NodeClaimTemplate {
	nct := scheduling.NewNodeClaimTemplate(np)
	nct.InstanceTypeOptions, _, _ = scheduling.FilterInstanceTypesByRequirements(
		instanceTypes,
		nct.Requirements,
		corev1.ResourceList{},
		corev1.ResourceList{},
		corev1.ResourceList{},
		true,
	)
	return nct
}
