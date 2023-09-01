package lifecycle_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/test"
)

var _ = Describe("NodeClaim/Finalizer", func() {
	var nodePool *v1beta1.NodePool

	BeforeEach(func() {
		nodePool = test.NodePool()
	})
	It("should add the finalizer if it doesn't exist", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		_, ok := lo.Find(nodeClaim.Finalizers, func(f string) bool {
			return f == v1beta1.TerminationFinalizer
		})
		Expect(ok).To(BeTrue())
	})
})
