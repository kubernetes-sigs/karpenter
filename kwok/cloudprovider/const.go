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

const (
	// Scheme Labels
	Group              = "karpenter.kwok.sh"
	kwokProviderPrefix = "kwok://"

	// Internal labels that are propagated to the node
	kwokLabelKey          = "kwok.x-k8s.io/node"
	kwokLabelValue        = "fake"
	nodeViewerLabelKey    = "eks-node-viewer/instance-price"
	kwokPartitionLabelKey = "kwok-partition"

	// Labels that can be selected on and are propagated to the node
	InstanceTypeLabelKey   = Group + "/instance-type"
	InstanceSizeLabelKey   = Group + "/instance-size"
	InstanceFamilyLabelKey = Group + "/instance-family"
	InstanceMemoryLabelKey = Group + "/instance-memory"
	InstanceCPULabelKey    = Group + "/instance-cpu"

	// Environment variables for configuration
	kwokInstanceTypeFileKey = "KWOK_INSTANCE_TYPES_FILE"
)

// Hard coded Kwok values
var (
	KwokZones      = []string{"test-zone-a", "test-zone-b", "test-zone-c", "test-zone-d"}
	KwokPartitions = []string{"partition-a", "partition-b", "partition-c", "partition-d", "partition-e",
		"partition-f", "partition-g", "partition-h", "partition-i", "partition-j"}
)
