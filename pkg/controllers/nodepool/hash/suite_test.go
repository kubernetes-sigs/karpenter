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

package hash_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	provcontroller "github.com/aws/karpenter-core/pkg/controllers/nodepool/hash"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"
)

var provisionerController controller.Controller
var provisioner *v1alpha5.Provisioner
var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "ProvisionerController")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	provisionerController = provcontroller.NewProvisionerController(env.Client)
	provisioner = test.Provisioner(test.ProvisionerOptions{
		Taints: []v1.Taint{
			{
				Key:    "key",
				Effect: v1.TaintEffectNoExecute,
			},
		},
		StartupTaints: []v1.Taint{
			{
				Key:    "key",
				Effect: v1.TaintEffectNoExecute,
			},
		},
		Labels: map[string]string{
			"keyLabel": "valueLabel",
		},
		Kubelet: &v1alpha5.KubeletConfiguration{
			MaxPods: ptr.Int32(10),
		},
		Annotations: map[string]string{
			"keyAnnotation":  "valueAnnotation",
			"keyAnnotation2": "valueAnnotation2",
		},
	})
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Provisioner Static Drift Hash", func() {
	It("should maintain the same hash, before and after the NodePool conversion", func() {
		hash := provisioner.Hash()
		nodePool := nodepoolutil.New(provisioner)
		convertedHash := nodepoolutil.HashAnnotation(nodePool)
		Expect(convertedHash).To(HaveKeyWithValue(v1alpha5.ProvisionerHashAnnotationKey, hash))
	})
	It("should update the static drift hash when provisioner static field is updated", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)

		expectedHash := provisioner.Hash()
		Expect(provisioner.ObjectMeta.Annotations[v1alpha5.ProvisionerHashAnnotationKey]).To(Equal(expectedHash))

		provisioner.Spec.Labels = map[string]string{"keyLabeltest": "valueLabeltest"}
		provisioner.Spec.Annotations = map[string]string{"keyAnnotation2": "valueAnnotation2", "keyAnnotation": "valueAnnotation"}
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)

		expectedHashTwo := provisioner.Hash()
		Expect(provisioner.ObjectMeta.Annotations[v1alpha5.ProvisionerHashAnnotationKey]).To(Equal(expectedHashTwo))
	})
	It("should not update the static drift hash when provisioner behavior field is updated", func() {
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)

		expectedHash := provisioner.Hash()
		Expect(provisioner.ObjectMeta.Annotations[v1alpha5.ProvisionerHashAnnotationKey]).To(Equal(expectedHash))

		provisioner.Spec.Limits = &v1alpha5.Limits{Resources: v1.ResourceList{"cpu": resource.MustParse("16")}}
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{Enabled: lo.ToPtr(true)}
		provisioner.Spec.TTLSecondsAfterEmpty = lo.ToPtr(int64(30))
		provisioner.Spec.TTLSecondsUntilExpired = lo.ToPtr(int64(50))
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
			{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1"}},
			{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}},
		}
		provisioner.Spec.Provider = &v1alpha5.Provider{}
		provisioner.Spec.ProviderRef = &v1alpha5.MachineTemplateRef{Kind: "NodeTemplate", Name: "default"}
		provisioner.Spec.Weight = lo.ToPtr(int32(80))
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		provisioner = ExpectExists(ctx, env.Client, provisioner)

		Expect(provisioner.ObjectMeta.Annotations[v1alpha5.ProvisionerHashAnnotationKey]).To(Equal(expectedHash))
	})
})
