package provisioning_test

import (
	"context"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

func TestPendingNodeClaimPreventsOverProvisioning(t *testing.T) {
	t.Parallel()

	ctx := options.ToContext(context.Background(), test.Options())
	kubeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		}).
		Build()

	cloudProvider := fake.NewCloudProvider()
	fakeClock := clock.NewFakeClock(time.Now())
	cluster := state.NewCluster(fakeClock, kubeClient, cloudProvider)
	prov := provisioning.NewProvisioner(kubeClient, events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster, fakeClock)

	nodePool := test.NodePool(v1.NodePool{
		Spec: v1.NodePoolSpec{
			Limits: v1.Limits(corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"),
			}),
		},
	})
	nodePool.StatusConditions().SetTrue(status.ConditionReady)
	if err := kubeClient.Create(ctx, nodePool); err != nil {
		t.Fatalf("creating nodepool, %v", err)
	}

	pod1 := test.UnschedulablePod(test.PodOptions{
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1.75"),
			},
		},
	})
	if err := kubeClient.Create(ctx, pod1); err != nil {
		t.Fatalf("creating first pod, %v", err)
	}

	firstResults, err := prov.Schedule(ctx)
	if err != nil {
		t.Fatalf("scheduling first pod, %v", err)
	}
	if got := len(firstResults.NewNodeClaims); got != 1 {
		t.Fatalf("expected first scheduling round to create 1 nodeclaim, got %d", got)
	}
	if _, err = prov.Create(ctx, firstResults.NewNodeClaims[0]); err != nil {
		t.Fatalf("creating first nodeclaim, %v", err)
	}

	if err := kubeClient.Delete(ctx, pod1); err != nil {
		t.Fatalf("deleting first pod, %v", err)
	}

	pod2 := test.UnschedulablePod(test.PodOptions{
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1.75"),
			},
		},
	})
	if err := kubeClient.Create(ctx, pod2); err != nil {
		t.Fatalf("creating second pod, %v", err)
	}

	secondResults, err := prov.Schedule(ctx)
	if err != nil {
		t.Fatalf("scheduling second pod, %v", err)
	}
	if got := len(secondResults.NewNodeClaims); got != 0 {
		t.Fatalf("expected second scheduling round to create 0 nodeclaims after an unlaunched nodeclaim consumed the nodepool limit, got %d", got)
	}
}
