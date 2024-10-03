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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	"sigs.k8s.io/controller-runtime/pkg/client"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

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

var (
	ctx                context.Context
	env                *test.Environment
	resourceController *resource.Controller[*v1.NodeClaim]
	crdController      *crd.Controller
	cloudProvider      *fake.CloudProvider
)

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

	BeforeEach(func() {
		nodeClaim, node = test.NodeClaimAndNode()
		node.Labels[v1.NodePoolLabelKey] = test.NodePool().Name
	})

	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
	})

	Context("Stored Version", func() {
		It("should update to v1 after custom resources have migrated", func() {
			for _, item := range apis.CRDs {
				crd := &apiextensionsv1.CustomResourceDefinition{}
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(item), crd)).To(Succeed())
				stored := crd.DeepCopy()
				crd.Status.StoredVersions = append(crd.Status.StoredVersions, "v1beta1")
				Eventually(func(g Gomega) {
					g.Expect(env.Client.Status().Patch(ctx, crd, client.StrategicMergeFrom(stored, client.MergeFromWithOptimisticLock{}))).To(Succeed())
				}).WithTimeout(time.Second * 10).Should(Succeed())
			}
			ExpectApplied(ctx, env.Client, node, nodeClaim)
			ExpectReconcileSucceeded(ctx, resourceController, client.ObjectKeyFromObject(nodeClaim))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.StoredVersionMigratedKey, "true"))
			for _, item := range apis.CRDs {
				crd := &apiextensionsv1.CustomResourceDefinition{}
				ExpectObjectReconciled(ctx, env.Client, crdController, item)
				Eventually(func(g Gomega) {
					g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(item), crd)).To(Succeed())
					g.Expect(crd.Status.StoredVersions).To(HaveExactElements("v1"))
				}).WithTimeout(time.Second * 10).Should(Succeed())
			}
		})
		It("shouldn't update the stored version to v1 if the storage version is v1beta1", func() {
			v1beta1CRDs := lo.Map(env.CRDs, func(crd *apiextensionsv1.CustomResourceDefinition, _ int) *apiextensionsv1.CustomResourceDefinition {
				v1beta1CRD := crd.DeepCopy()
				for i := range v1beta1CRD.Spec.Versions {
					version := &v1beta1CRD.Spec.Versions[i]
					version.Storage = version.Name == "v1beta1"
				}
				v1beta1CRD.Status.StoredVersions = []string{"v1beta1"}
				return v1beta1CRD
			})
			for _, crd := range v1beta1CRDs {
				ExpectObjectReconciled(ctx, env.Client, crdController, crd)
				// Note: since we're passing the CRD in by pointer, we don't need to re-read from the API server
				Expect(crd.Status.StoredVersions).To(HaveExactElements("v1beta1"))
			}

		})
	})
})
