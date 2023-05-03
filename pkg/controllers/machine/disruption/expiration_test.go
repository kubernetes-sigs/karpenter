package disruption_test

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Expiration", func() {
	var provisioner *v1alpha5.Provisioner
	BeforeEach(func() {
		provisioner = test.Provisioner()
	})

	It("should remove the annotation from nodes when expiration is disabled", func() {
		node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
			Annotations: map[string]string{
				v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionExpiredAnnotationValue,
			},
		}})
		ExpectApplied(ctx, env.Client, provisioner, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Annotations).ToNot(HaveKey(v1alpha5.VoluntaryDisruptionAnnotationKey))
	})
	It("should annotate nodes as expired", func() {
		provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(30)
		node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
		}})
		ExpectApplied(ctx, env.Client, provisioner, node)

		// step forward to make the node expired
		fakeClock.Step(60 * time.Second)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Annotations).To(HaveKeyWithValue(v1alpha5.VoluntaryDisruptionAnnotationKey, v1alpha5.VoluntaryDisruptionExpiredAnnotationValue))
	})
	It("should remove the annotation from non-expired nodes", func() {
		provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(200)
		node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
			Annotations: map[string]string{
				v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionExpiredAnnotationValue,
			}},
		})
		ExpectApplied(ctx, env.Client, provisioner, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Annotations).ToNot(HaveKey(v1alpha5.VoluntaryDisruptionAnnotationKey))
	})
})
