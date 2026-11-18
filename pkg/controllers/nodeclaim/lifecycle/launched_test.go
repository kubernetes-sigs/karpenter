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

package lifecycle_test

import (
	"context"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	nodeclaimlifecycle "sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle/launched"
	"sigs.k8s.io/karpenter/pkg/state/nodepoolhealth"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Launched Registry", func() {
	var registry *launched.Registry
	var throttle *atomic.Bool
	var controller *nodeclaimlifecycle.Controller
	var nodePool *v1.NodePool

	BeforeEach(func() {
		registry = launched.NewRegistry(env.Clock)
		throttle = &atomic.Bool{}
		controller = nodeclaimlifecycle.NewController(env.Clock, &throttledStatusClient{Client: env.Client, throttle: throttle}, cloudProvider, recorder, cluster, nodepoolhealth.NewState(), registry, nil)
		nodePool = test.NodePool()
	})

	It("should keep a launched NodeClaim alive and launch once while its status write is throttled past the launch timeout", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}}})
		nodeClaim.Status.ProviderID = "" // not launched yet
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		throttle.Store(true)

		// Throttled for 10m: longer than the 5m launch timeout and longer than the 5m minimum instance age proposed
		// for instance garbage collection.
		for range 10 {
			ExpectObjectReconcileFailed(ctx, env.Client, controller, nodeClaim)
			env.Clock.Step(time.Minute)
		}
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(nodeClaim.Status.ProviderID).To(BeEmpty())
		providerID := registry.ProviderID(nodeClaim.UID)
		Expect(providerID).ToNot(BeEmpty())
		Expect(registry.HasProviderID(providerID)).To(BeTrue())

		throttle.Store(false)
		ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.Status.ProviderID).To(Equal(providerID))
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))

		// The entry is dropped once the persisted launch result is observed
		ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		Expect(registry.HasProviderID(providerID)).To(BeFalse())
		Expect(registry.Len()).To(BeZero())
	})
	It("should terminate the launched instance when a NodeClaim is deleted before its providerID is persisted", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: nodePool.Name}}})
		nodeClaim.Status.ProviderID = "" // not launched yet
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		throttle.Store(true)
		ExpectObjectReconcileFailed(ctx, env.Client, controller, nodeClaim)
		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		providerID := registry.ProviderID(nodeClaim.UID)
		Expect(providerID).ToNot(BeEmpty())
		Expect(nodeClaim.Status.ProviderID).To(BeEmpty())
		Expect(cloudProvider.CreatedNodeClaims).To(HaveKey(providerID))

		throttle.Store(false)
		Expect(env.Client.Delete(ctx, nodeClaim)).To(Succeed()) // held by the termination finalizer
		ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, controller, nodeClaim)

		// The first Delete terminates the instance, the second observes it is gone
		Expect(cloudProvider.DeleteCalls).To(HaveLen(2))
		Expect(cloudProvider.CreatedNodeClaims).ToNot(HaveKey(providerID))
		ExpectNotFound(ctx, env.Client, nodeClaim)
		Expect(registry.Len()).To(BeZero())
	})
})

// throttledStatusClient fails NodeClaim status writes with 429 while throttle is set, like an API server shedding load
type throttledStatusClient struct {
	client.Client
	throttle *atomic.Bool
}

func (c *throttledStatusClient) Status() client.SubResourceWriter {
	return &throttledStatusWriter{SubResourceWriter: c.Client.Status(), throttle: c.throttle}
}

type throttledStatusWriter struct {
	client.SubResourceWriter
	throttle *atomic.Bool
}

func (w *throttledStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if _, ok := obj.(*v1.NodeClaim); ok && w.throttle.Load() {
		return apierrors.NewTooManyRequests("throttled", 1)
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}
