package disruption_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"
)

var _ = Describe("Emptiness", func() {
	var provisioner *v1alpha5.Provisioner
	BeforeEach(func() {
		provisioner = test.Provisioner()
	})

	It("should not TTL nodes that are not initialized", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		node := test.Node(test.NodeOptions{
			ObjectMeta:  metav1.ObjectMeta{Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name}},
			ReadyStatus: v1.ConditionFalse,
		})

		ExpectApplied(ctx, env.Client, provisioner, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Annotations).ToNot(HaveKey(v1alpha5.EmptinessTimestampAnnotationKey))
	})
	It("should label nodes as underutilized and add TTL", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1alpha5.LabelNodeInitialized:    "true",
			},
		}})
		ExpectApplied(ctx, env.Client, provisioner, node)

		// mark it empty first to get past the debounce check
		fakeClock.Step(30 * time.Second)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		// make the node more than 5 minutes old
		fakeClock.Step(320 * time.Second)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Annotations).To(HaveKey(v1alpha5.EmptinessTimestampAnnotationKey))
	})
	It("should return a requeue polling interval when the node is underutilized and nominated", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1alpha5.LabelNodeInitialized:    "true",
				v1.LabelInstanceTypeStable:       "default-instance-type", // need the instance type for the cluster state update
			},
		}})
		ExpectApplied(ctx, env.Client, provisioner, node)

		// Add the node to the cluster state and nominate it in the internal cluster state
		Expect(cluster.UpdateNode(ctx, node)).To(Succeed())
		cluster.NominateNodeForPod(ctx, node.Name)

		result := ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		Expect(result.RequeueAfter).To(Equal(time.Second * 30))
		Expect(node.Labels).ToNot(HaveKey(v1alpha5.EmptinessTimestampAnnotationKey))
	})
	It("should remove labels from non-empty nodes", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1alpha5.LabelNodeInitialized:    "true",
			},
			Annotations: map[string]string{
				v1alpha5.EmptinessTimestampAnnotationKey: fakeClock.Now().Add(100 * time.Second).Format(time.RFC3339),
			}},
		})
		ExpectApplied(ctx, env.Client, provisioner, node, test.Pod(test.PodOptions{
			NodeName:   node.Name,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		}))
		// make the node more than 5 minutes old
		fakeClock.Step(320 * time.Second)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Annotations).ToNot(HaveKey(v1alpha5.EmptinessTimestampAnnotationKey))
	})
})
