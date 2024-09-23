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

package crd_test

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/migration/crd"
	"sigs.k8s.io/karpenter/pkg/controllers/migration/resource"
	"sigs.k8s.io/karpenter/pkg/test"

	corev1 "k8s.io/api/core/v1"

	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
)

var ctx context.Context
var env *test.Environment
var resourceController *resource.Controller[*v1.NodeClaim]
var crdController *crd.Controller
var cloudProvider *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "MigrationCRD")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	cloudProvider = fake.NewCloudProvider()
	resourceController = resource.NewController[*v1.NodeClaim](env.Client)
	crdController = crd.NewController(env.Client, cloudProvider)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Migration", func() {
	var node *corev1.Node
	var nodeClaim *v1.NodeClaim
	var nodeClass *v1alpha1.TestNodeClass

	BeforeEach(func() {
		nodeClass = test.NodeClass()
		nodeClaim, node = test.NodeClaimAndNode()
		node.Labels[v1.NodePoolLabelKey] = test.NodePool().Name
	})

	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
	})

	Context("Stored Version", func() {
		It("should update to v1 after custom resources have migrated", func() {
			ExpectApplied(ctx, env.Client, node, nodeClaim)
			ExpectReconcileSucceeded(ctx, resourceController, client.ObjectKeyFromObject(nodeClaim))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.StoredVersionMigrated, "true"))
			ExpectObjectReconciled(ctx, env.Client, crdController, v1alpha1.CRDs[0])
			for _, crd := range env.CRDs {
				if strings.Contains(crd.Name, strings.ToLower(nodeClass.Name)) {
					Expect(crd.Status.StoredVersions).To(HaveExactElements("v1"))
				}
			}
		})
	})
})
