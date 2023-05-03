package disruption_test

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"
)

var _ = Describe("Drift", func() {
	var provisioner *v1alpha5.Provisioner
	BeforeEach(func() {
		provisioner = test.Provisioner()
	})

	It("should not detect drift if the feature flag is disabled", func() {
		cp.Drifted = true
		ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: false}))
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1.LabelInstanceTypeStable:       test.RandomName(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Annotations).ToNot(HaveKeyWithValue(v1alpha5.VoluntaryDisruptionAnnotationKey, v1alpha5.VoluntaryDisruptionDriftedAnnotationValue))
	})
	It("should not detect drift if the provisioner does not exist", func() {
		cp.Drifted = true
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1.LabelInstanceTypeStable:       test.RandomName(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Annotations).ToNot(HaveKeyWithValue(v1alpha5.VoluntaryDisruptionAnnotationKey, v1alpha5.VoluntaryDisruptionDriftedAnnotationValue))
	})
	It("should annotate the node when it has drifted in the cloud provider", func() {
		cp.Drifted = true
		machine, node := test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1.LabelInstanceTypeStable:       test.RandomName(),
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeMachinesReady(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Annotations).To(HaveKeyWithValue(v1alpha5.VoluntaryDisruptionAnnotationKey, v1alpha5.VoluntaryDisruptionDriftedAnnotationValue))
	})
	It("should remove the annotation from nodes if drift is disabled", func() {
		cp.Drifted = true
		ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: false}))
		node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
			Annotations: map[string]string{
				v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionDriftedAnnotationValue,
			},
		}})
		ExpectApplied(ctx, env.Client, provisioner, node)

		// step forward to make the node expired
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Annotations).ToNot(HaveKey(v1alpha5.VoluntaryDisruptionAnnotationKey))
	})
})
