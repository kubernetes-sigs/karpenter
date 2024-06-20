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

package instancetype_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/instancetype"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("Provider", func() {

	var _ = BeforeEach(func() {
		cloudProvider.Reset()
		cloudProvider.InstanceTypes = fake.InstanceTypes(6)
		nodePool = test.NodePool()
	})

	It("should cache instance types", func() {
		instancetypes, err := instanceTypeProvider.Get(ctx, nodePool)
		Expect(err).ToNot(HaveOccurred())
		Expect(instancetypes).To(HaveLen(6))
		cloudProvider.InstanceTypes = cloudProvider.InstanceTypes[1:]
		// Read from cache
		instancetypes, err = instanceTypeProvider.Get(ctx, nodePool, instancetype.FromCache(true))
		Expect(err).ToNot(HaveOccurred())
		Expect(instancetypes).To(HaveLen(6))
		// Update the cache
		instancetypes, err = instanceTypeProvider.Get(ctx, nodePool, instancetype.FromCache(false))
		Expect(err).ToNot(HaveOccurred())
		Expect(instancetypes).To(HaveLen(5))
	})
})
