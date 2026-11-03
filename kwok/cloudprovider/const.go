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

import "sigs.k8s.io/karpenter/pkg/cloudprovider"

const (
	kwokProviderPrefix = "kwok://"

	// CapacityTypeOnDemandAlt is an example of a provider-defined capacity type in the on-demand class: on-demand
	// capacity bought another way, at a discount. A NodePool only launches it when it requires it.
	CapacityTypeOnDemandAlt = "on-demand-alt"
	onDemandAltDiscount     = 0.1
)

// CapacityTypes registers on-demand-alt in the on-demand class.
var CapacityTypes = cloudprovider.DefaultCapacityTypeClasses.With(cloudprovider.CapacityTypeClasses{
	CapacityTypeOnDemandAlt: cloudprovider.CapacityTypeClassOnDemand,
})

var kwokPartitions = []string{"a"}
