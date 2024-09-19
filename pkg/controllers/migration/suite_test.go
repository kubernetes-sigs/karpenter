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

package migration_test

import (
	"context"
	"strings"
	"testing"

	operatorexpectations "github.com/awslabs/operatorpkg/test/expectations"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/migration"
	"sigs.k8s.io/karpenter/pkg/test"

	corev1 "k8s.io/api/core/v1"

	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
)

var ctx context.Context
var migrationController *migration.Controller[*v1.NodeClaim]
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Migration")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	migrationController = migration.NewController[*v1.NodeClaim](env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Migration", func() {
	var node *corev1.Node
	var nodeClaim *v1.NodeClaim
	var nodeClass *v1alpha1.TestNodeClass

	BeforeEach(func() {
		nodeClaim, node = test.NodeClaimAndNode()
		nodeClass = test.NodeClass()
		node.Labels[v1.NodePoolLabelKey] = test.NodePool().Name
	})

	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
	})

	Context("Annotations", func() {
		It("should add stored version", func() {
			ExpectApplied(ctx, env.Client, node, nodeClaim)
			operatorexpectations.ExpectSingletonReconciled(ctx, migrationController)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.StoredVersionMigrated, "true"))
		})
		It("should patch CRD status stored versions", func() {
			ExpectApplied(ctx, env.Client, node, nodeClaim, nodeClass)
			operatorexpectations.ExpectSingletonReconciled(ctx, migrationController)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			nodeClass = ExpectExists(ctx, env.Client, nodeClass)
			for _, crd := range env.CRDs {
				if strings.Contains(crd.Name, strings.ToLower(nodeClass.Name)) {
					Expect(crd.Status.StoredVersions).To(HaveExactElements("v1"))
				}
			}
		})
	})
})
