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

package fake

import (
	v1 "k8s.io/api/core/v1"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
)

const (
	LabelInstanceSize                       = "size"
	ExoticInstanceLabelKey                  = "special"
	IntegerInstanceLabelKey                 = "integer"
	ResourceGPUVendorA      v1.ResourceName = "fake.com/vendor-a"
	ResourceGPUVendorB      v1.ResourceName = "fake.com/vendor-b"
)

func AddFakeLabels() {
	v1beta1.WellKnownLabels.Insert(
		LabelInstanceSize,
		ExoticInstanceLabelKey,
		IntegerInstanceLabelKey,
	)
}

func init() {
	AddFakeLabels()
}
