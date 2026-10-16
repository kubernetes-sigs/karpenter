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

package scheduling_test

import (
	"github.com/awslabs/operatorpkg/object"
	. "github.com/onsi/ginkgo/v2"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test/schedulingproperties"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
)

var _ = Describe("Scheduling Properties", func() {
	var cfg schedulingproperties.Config
	BeforeEach(func() {
		launcher := fake.NewCloudProvider()
		launcher.InstanceTypes = fake.InstanceTypesAssorted()
		nodeClass := object.GVK(&v1alpha1.TestNodeClass{}).GroupKind()
		cfg = schedulingproperties.Config{
			CloudProvider: launcher,
			InstanceTypes: launcher.InstanceTypes,
			Launch:        launcher.Create,
			// Real providers restrict their own label domain, which also covers the NodeClass label that core stamps.
			RestrictedLabelDomains: []string{nodeClass.Group},
			// Karpenter rejects pods selecting the NodeClass label when the provider restricts its domain, even though it
			// stamps that label on every node. See #3422.
			KnownDeviations: sets.New(v1.NodeClassLabelKey(nodeClass)),
			Checks:          300,
		}
	})
	It("should place pods that a launchable node satisfies", func() {
		schedulingproperties.ExpectSatisfiablePodsArePlaced(ctx, GinkgoT(), cfg)
	})
	It("should only place pods on nodes that fit them", func() {
		schedulingproperties.ExpectPlacementsFitNodes(ctx, GinkgoT(), cfg)
	})
})
