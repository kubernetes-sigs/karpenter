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

package deviceallocation_test

import (
	"context"
	"iter"
	"testing"
	"time"
	"unique"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	deviceallocation "sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	ctx        context.Context
	env        *test.Environment
	controller *deviceallocation.Controller
)

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "DeviceAllocation")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	if env.Version.Minor() < 34 {
		Skip("ResourceClaims are only available starting in K8s version >= 1.34.x")
	}
	ctx = options.ToContext(ctx, test.Options())
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed())
})

var _ = BeforeEach(func() {
	controller = deviceallocation.NewController(env.Client)
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

// resourceClaim constructs a ResourceClaim in the default namespace. If any results are provided
// they are set as the claim's allocation status. A matching spec request named "request" is
// included so that the allocation result passes API server validation.
func resourceClaim(name string, results ...resourcev1.DeviceRequestAllocationResult) *resourcev1.ResourceClaim {
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name:    "request",
						Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "test-class"},
					},
				},
			},
		},
	}
	if len(results) > 0 {
		claim.Status.Allocation = &resourcev1.AllocationResult{
			Devices: resourcev1.DeviceAllocationResult{
				Results: results,
			},
		}
	}
	return claim
}

// withReservedFor sets the ReservedFor status on a ResourceClaim and returns it for chaining.
func withReservedFor(claim *resourcev1.ResourceClaim, refs ...resourcev1.ResourceClaimConsumerReference) *resourcev1.ResourceClaim {
	claim.Status.ReservedFor = refs
	return claim
}

// podRef creates a ResourceClaimConsumerReference for a pod.
func podRef(name string, uid types.UID) resourcev1.ResourceClaimConsumerReference {
	return resourcev1.ResourceClaimConsumerReference{
		Resource: "pods",
		Name:     name,
		UID:      uid,
	}
}

// nonPodRef creates a ResourceClaimConsumerReference for a non-pod consumer.
func nonPodRef() resourcev1.ResourceClaimConsumerReference {
	return resourcev1.ResourceClaimConsumerReference{
		APIGroup: "example.com",
		Resource: "services",
		Name:     "svc-a",
		UID:      "uid-svc",
	}
}

// deviceResult constructs a DeviceRequestAllocationResult for use in ResourceClaim status.
func deviceResult(device string) resourcev1.DeviceRequestAllocationResult {
	return resourcev1.DeviceRequestAllocationResult{
		Request: "request",
		Driver:  "driver.example.com",
		Pool:    "pool-a",
		Device:  device,
	}
}

// deviceID constructs a cloudprovider.DeviceID using interned handles, matching the controller's representation.
func deviceID(device string) cloudprovider.DeviceID {
	return cloudprovider.DeviceID{
		Driver: unique.Make("driver.example.com"),
		Pool:   unique.Make("pool-a"),
		Device: unique.Make(device),
	}
}

// collectDevices drains an iterator into a map for test assertions.
func collectDevices(seq iter.Seq2[cloudprovider.DeviceID, deviceallocation.DeviceMetadata]) map[cloudprovider.DeviceID]deviceallocation.DeviceMetadata {
	m := make(map[cloudprovider.DeviceID]deviceallocation.DeviceMetadata)
	for id, meta := range seq {
		m[id] = meta
	}
	return m
}

// expectedDevices builds the expected map with zero-value metadata for each device.
func expectedDevices(ids ...cloudprovider.DeviceID) map[cloudprovider.DeviceID]deviceallocation.DeviceMetadata {
	m := make(map[cloudprovider.DeviceID]deviceallocation.DeviceMetadata, len(ids))
	for _, id := range ids {
		m[id] = deviceallocation.DeviceMetadata{}
	}
	return m
}

// expectCapacity asserts that the actual capacity map has the same keys and semantically equal quantities as expected.
func expectCapacity(actual, expected map[resourcev1.QualifiedName]resource.Quantity) {
	GinkgoHelper()
	Expect(actual).To(HaveLen(len(expected)))
	for name, expectedQty := range expected {
		Expect(actual).To(HaveKey(name))
		actualQty := actual[name]
		Expect(actualQty.Cmp(expectedQty)).To(Equal(0), "capacity %q: got %s, want %s", name, actualQty.String(), expectedQty.String())
	}
}

// deviceResultWithCapacity constructs a DeviceRequestAllocationResult with consumed capacity.
func deviceResultWithCapacity(device string, capacity map[resourcev1.QualifiedName]resource.Quantity) resourcev1.DeviceRequestAllocationResult {
	return resourcev1.DeviceRequestAllocationResult{
		Request:          "request",
		Driver:           "driver.example.com",
		Pool:             "pool-a",
		Device:           device,
		ConsumedCapacity: capacity,
	}
}

// triggerHydration calls Hydrate() directly to close hydrationCh, making subsequent AllocatedDevices
// calls non-blocking. In production this is triggered by a manager runnable after cache sync. Call
// this at the start of any test that is not explicitly testing the hydration blocking behavior.
func triggerHydration() {
	controller.Hydrate(ctx)
}

var _ = Describe("DeviceAllocation Controller", func() {
	Describe("Hydration", func() {
		It("blocks until hydration completes", func() {
			results := make(chan map[cloudprovider.DeviceID]deviceallocation.DeviceMetadata, 1)
			go func() {
				defer GinkgoRecover()
				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				results <- collectDevices(seq)
			}()

			Consistently(results).WithTimeout(100 * time.Millisecond).WithPolling(10 * time.Millisecond).ShouldNot(Receive())

			triggerHydration()

			Eventually(results).Should(Receive(BeEmpty()))
		})
		It("hydration includes devices from ResourceClaims that existed before the first reconcile", func() {
			// Apply claims before any reconcile has run — these must be picked up by Hydrate()
			claimA := resourceClaim("claim-a", deviceResult("device-0"))
			claimB := resourceClaim("claim-b", deviceResult("device-1"))
			ExpectApplied(ctx, env.Client, claimA, claimB)

			triggerHydration()

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(Equal(expectedDevices(
				deviceID("device-0"),
				deviceID("device-1"),
			)))
		})
		It("returns a copy that is not affected by subsequent reconciliations", func() {
			triggerHydration()

			claim := resourceClaim("claim-a", deviceResult("device-0"))
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			snapshot := collectDevices(seq)

			// Reconcile a new claim to mutate the controller's internal state after the snapshot was taken
			claim = resourceClaim("claim-b", deviceResult("device-1"))
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			Expect(snapshot).To(Equal(expectedDevices(deviceID("device-0"))))
		})
		It("does not block on subsequent calls after hydration", func() {
			triggerHydration()

			// Calling AllocatedDevices synchronously would deadlock if it still blocked.
			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(BeEmpty())
		})
		It("returns a context error when the context is canceled before hydration completes", func() {
			cancelCtx, cancel := context.WithCancel(ctx)
			cancel()

			_, err := controller.AllocatedDevices(cancelCtx)
			Expect(err).To(Equal(context.Canceled))
		})
	})

	Describe("Basic allocation", func() {
		BeforeEach(func() {
			triggerHydration()
		})
		It("returns an empty set when a claim has no allocation", func() {
			claim := resourceClaim("no-alloc-claim")
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(BeEmpty())
		})
		It("returns a single device from a ResourceClaim with one allocation", func() {
			claim := resourceClaim("single-claim", deviceResult("device-0"))
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(Equal(expectedDevices(deviceID("device-0"))))
		})
		It("returns all devices from a ResourceClaim with multiple allocations", func() {
			claim := resourceClaim("multi-device-claim",
				deviceResult("device-0"),
				deviceResult("device-1"),
			)
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(Equal(expectedDevices(
				deviceID("device-0"),
				deviceID("device-1"),
			)))
		})
		It("returns the union of distinct devices across multiple ResourceClaims", func() {
			claimA := resourceClaim("claim-a", deviceResult("device-0"))
			claimB := resourceClaim("claim-b", deviceResult("device-1"))
			ExpectApplied(ctx, env.Client, claimA, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(Equal(expectedDevices(
				deviceID("device-0"),
				deviceID("device-1"),
			)))
		})
		It("returns a shared device only once when multiple ResourceClaims allocate it", func() {
			claimA := resourceClaim("claim-a", deviceResult("device-0"))
			claimB := resourceClaim("claim-b", deviceResult("device-0"))
			ExpectApplied(ctx, env.Client, claimA, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(Equal(expectedDevices(deviceID("device-0"))))
		})
	})

	Describe("Mutation", func() {
		BeforeEach(func() {
			triggerHydration()
		})
		It("removes a device when its only ResourceClaim is deleted", func() {
			claim := resourceClaim("single-claim", deviceResult("device-0"))
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			ExpectDeleted(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(BeEmpty())
		})
		It("removes only the deleted claim's devices when a subset of claims is deleted", func() {
			claimA := resourceClaim("claim-a", deviceResult("device-0"))
			claimB := resourceClaim("claim-b", deviceResult("device-1"))
			ExpectApplied(ctx, env.Client, claimA, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			ExpectDeleted(ctx, env.Client, claimA)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(Equal(expectedDevices(deviceID("device-1"))))
		})
		It("does not remove a shared device when one of multiple claims that allocate it is deleted", func() {
			claimA := resourceClaim("claim-a", deviceResult("device-0"))
			claimB := resourceClaim("claim-b", deviceResult("device-0"))
			ExpectApplied(ctx, env.Client, claimA, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			ExpectDeleted(ctx, env.Client, claimA)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(Equal(expectedDevices(deviceID("device-0"))))
		})
		It("removes devices when a claim is deleted and recreated without an allocation", func() {
			// claim-b holds device-1 throughout the test; it is the shared device
			claimB := resourceClaim("claim-b", deviceResult("device-1"))
			ExpectApplied(ctx, env.Client, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			// my-claim initially allocates device-0 (distinct) and device-1 (shared with claim-b)
			claim := resourceClaim("my-claim",
				deviceResult("device-0"),
				deviceResult("device-1"),
			)
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			// Don't re-reconcile after deleting. devicesPerClaim["my-claim"] still holds device-0
			// and device-1 when the recreated claim is reconciled.
			ExpectDeleted(ctx, env.Client, claim)

			// Recreate with no allocation — reconcile hits the nil allocation path (controller.go:86)
			// and calls finalizeClaim, which drains devicesPerClaim["my-claim"]
			claim = resourceClaim("my-claim")
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			// device-0 removed (was distinct to my-claim, last reference gone)
			// device-1 persists (still held by claim-b)
			Expect(devices).To(Equal(expectedDevices(deviceID("device-1"))))
		})
		It("replaces devices when a claim is deleted and recreated with a different allocation", func() {
			claimB := resourceClaim("claim-b", deviceResult("device-1"))
			ExpectApplied(ctx, env.Client, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			// my-claim initially allocates device-0 (distinct to my-claim) and device-1 (shared with claim-b)
			claim := resourceClaim("my-claim",
				deviceResult("device-0"),
				deviceResult("device-1"),
			)
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			// Don't re-reconcile after deleting. This exercises the mutation path where
			// devicesPerClaim["my-claim"] still holds device-0 and device-1 when the
			// recreated claim is reconciled.
			ExpectDeleted(ctx, env.Client, claim)

			// Recreate with the same name: drops device-0 (distinct, should be removed),
			// keeps device-1 (shared with claim-b, must persist), adds device-2 (new distinct)
			claim = resourceClaim("my-claim",
				deviceResult("device-1"),
				deviceResult("device-2"),
			)
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(Equal(expectedDevices(
				deviceID("device-1"),
				deviceID("device-2"),
			)))
		})
		It("removes stale devices and adds new ones when a claim is recreated with different devices", func() {
			claim := resourceClaim("claim-a", deviceResult("device-0"), deviceResult("device-1"))
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			ExpectDeleted(ctx, env.Client, claim)
			claim = resourceClaim("claim-a", deviceResult("device-1"), deviceResult("device-2"))
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).ToNot(HaveKey(deviceID("device-0")))
			Expect(devices).To(HaveKey(deviceID("device-1")))
			Expect(devices).To(HaveKey(deviceID("device-2")))
		})
		It("retains a shared device when one claim drops it but another claim still references it", func() {
			claimA := resourceClaim("claim-a", deviceResult("device-0"), deviceResult("device-1"))
			claimB := resourceClaim("claim-b", deviceResult("device-0"))
			ExpectApplied(ctx, env.Client, claimA, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			ExpectDeleted(ctx, env.Client, claimA)
			claimA = resourceClaim("claim-a", deviceResult("device-1"))
			ExpectApplied(ctx, env.Client, claimA)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(HaveKey(deviceID("device-0")))
			Expect(devices).To(HaveKey(deviceID("device-1")))
		})
		It("clears the allocation entirely when a claim's allocation is set to nil", func() {
			claim := resourceClaim("claim-a", deviceResult("device-0"))
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			claim.Status.Allocation = nil
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			Expect(devices).To(BeEmpty())
		})
	})

	Describe("Metadata", func() {
		BeforeEach(func() {
			triggerHydration()
		})

		Describe("Releasable", func() {
			It("is true when a claim is reserved for a single pod", func() {
				claim := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					podRef("pod-a", "uid-a"),
				)
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				Expect(devices).To(HaveKeyWithValue(
					deviceID("device-0"),
					deviceallocation.DeviceMetadata{Releasable: true, PodUIDs: []types.UID{"uid-a"}},
				))
			})
			It("is true when a claim is reserved for multiple pods", func() {
				claim := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					podRef("pod-a", "uid-a"),
					podRef("pod-b", "uid-b"),
				)
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				Expect(devices).To(HaveKeyWithValue(
					deviceID("device-0"),
					deviceallocation.DeviceMetadata{Releasable: true, PodUIDs: []types.UID{"uid-a", "uid-b"}},
				))
			})
			It("is true when two claims sharing a device are both reserved only for pods", func() {
				claimA := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					podRef("pod-a", "uid-a"),
				)
				claimB := withReservedFor(
					resourceClaim("claim-b", deviceResult("device-0")),
					podRef("pod-b", "uid-b"),
				)
				ExpectApplied(ctx, env.Client, claimA, claimB)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				meta := devices[deviceID("device-0")]
				Expect(meta.Releasable).To(BeTrue())
				Expect(meta.PodUIDs).To(ConsistOf(types.UID("uid-a"), types.UID("uid-b")))
			})
			// This shouldn't occur since KCM should atomically remove the allocation when it removes the last reservation. If
			// this does occur, we should be conservative and consider the device allocated.
			It("is false when a claim has an empty ReservedFor", func() {
				claim := resourceClaim("claim-a", deviceResult("device-0"))
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				Expect(devices).To(HaveKeyWithValue(
					deviceID("device-0"),
					deviceallocation.DeviceMetadata{Releasable: false},
				))
			})
			It("is false when a claim is reserved for a non-pod consumer", func() {
				claim := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					nonPodRef(),
				)
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				meta := devices[deviceID("device-0")]
				Expect(meta.Releasable).To(BeFalse())
				Expect(meta.PodUIDs).To(BeNil())
			})
			It("is false when a claim is reserved for a mix of pod and non-pod consumers", func() {
				claim := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					podRef("pod-a", "uid-a"),
					nonPodRef(),
				)
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				meta := devices[deviceID("device-0")]
				Expect(meta.Releasable).To(BeFalse())
				Expect(meta.PodUIDs).To(ConsistOf(types.UID("uid-a")))
			})
			It("is false when two claims share a device and one has a non-pod consumer", func() {
				claimA := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					podRef("pod-a", "uid-a"),
				)
				claimB := withReservedFor(
					resourceClaim("claim-b", deviceResult("device-0")),
					nonPodRef(),
				)
				ExpectApplied(ctx, env.Client, claimA, claimB)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				meta := devices[deviceID("device-0")]
				Expect(meta.Releasable).To(BeFalse())
				Expect(meta.PodUIDs).To(ConsistOf(types.UID("uid-a")))
			})
		})

		Describe("Mutation", func() {
			It("becomes releasable when a claim is updated from non-pod to pod-only reservation", func() {
				claim := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					nonPodRef(),
				)
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				Expect(devices[deviceID("device-0")].Releasable).To(BeFalse())

				// Update to pod-only reservation
				claim.Status.ReservedFor = []resourcev1.ResourceClaimConsumerReference{
					{Resource: "pods", Name: "pod-a", UID: "uid-a"},
				}
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err = controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices = collectDevices(seq)
				Expect(devices).To(HaveKeyWithValue(
					deviceID("device-0"),
					deviceallocation.DeviceMetadata{Releasable: true, PodUIDs: []types.UID{"uid-a"}},
				))
			})
			It("becomes releasable when the only non-pod claim sharing a device is deleted", func() {
				claimA := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					podRef("pod-a", "uid-a"),
				)
				claimB := withReservedFor(
					resourceClaim("claim-b", deviceResult("device-0")),
					nonPodRef(),
				)
				ExpectApplied(ctx, env.Client, claimA, claimB)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				Expect(devices[deviceID("device-0")].Releasable).To(BeFalse())

				// Delete the non-pod claim
				ExpectDeleted(ctx, env.Client, claimB)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

				seq, err = controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices = collectDevices(seq)
				Expect(devices).To(HaveKeyWithValue(
					deviceID("device-0"),
					deviceallocation.DeviceMetadata{Releasable: true, PodUIDs: []types.UID{"uid-a"}},
				))
			})
			It("becomes non-releasable when a claim's reservations are cleared", func() {
				claim := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					podRef("pod-a", "uid-a"),
				)
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				Expect(devices[deviceID("device-0")].Releasable).To(BeTrue())

				claim.Status.ReservedFor = nil
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err = controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices = collectDevices(seq)
				Expect(devices).To(HaveKeyWithValue(
					deviceID("device-0"),
					deviceallocation.DeviceMetadata{Releasable: false},
				))
			})
		})
	})

	Describe("Consumable Capacity", func() {
		BeforeEach(func() {
			if env.Version.Minor() < 36 {
				Skip("ConsumedCapacity requires K8s version >= 1.36.x (DRAConsumableCapacity feature gate)")
			}
			triggerHydration()
		})

		Describe("Hydration", func() {
			It("computes shared and consumed capacity for pre-existing claims", func() {
				claimA := withReservedFor(
					resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
						"memory": resource.MustParse("256Mi"),
					})),
					podRef("pod-a", "uid-a"),
				)
				claimB := withReservedFor(
					resourceClaim("claim-b", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
						"memory": resource.MustParse("128Mi"),
					})),
					podRef("pod-b", "uid-b"),
				)
				ExpectApplied(ctx, env.Client, claimA, claimB)

				// triggerHydration() already called in BeforeEach; re-create controller to test fresh hydration
				controller = deviceallocation.NewController(env.Client)
				triggerHydration()

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				meta := devices[deviceID("device-0")]
				Expect(meta.Shared).To(BeTrue())
				Expect(meta.Releasable).To(BeTrue())
				Expect(meta.PodUIDs).To(ConsistOf(types.UID("uid-a"), types.UID("uid-b")))
				expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("384Mi"),
				})
			})
		})

		It("marks a device as shared with aggregated consumed capacity when a claim has ConsumedCapacity", func() {
			claim := withReservedFor(
				resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
					"memory":      resource.MustParse("512Mi"),
					"connections": resource.MustParse("2"),
				})),
				podRef("pod-a", "uid-a"),
			)
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			meta := devices[deviceID("device-0")]
			Expect(meta.Releasable).To(BeTrue())
			Expect(meta.PodUIDs).To(ConsistOf(types.UID("uid-a")))
			Expect(meta.Shared).To(BeTrue())
			expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
				"memory":      resource.MustParse("512Mi"),
				"connections": resource.MustParse("2"),
			})
		})

		It("aggregates consumed capacity across multiple claims referencing the same device", func() {
			claimA := withReservedFor(
				resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
					"memory":      resource.MustParse("256Mi"),
					"connections": resource.MustParse("1"),
				})),
				podRef("pod-a", "uid-a"),
			)
			claimB := withReservedFor(
				resourceClaim("claim-b", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
					"memory":      resource.MustParse("128Mi"),
					"connections": resource.MustParse("3"),
				})),
				podRef("pod-b", "uid-b"),
			)
			ExpectApplied(ctx, env.Client, claimA, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			meta := devices[deviceID("device-0")]
			Expect(meta.Shared).To(BeTrue())
			Expect(meta.Releasable).To(BeTrue())
			Expect(meta.PodUIDs).To(ConsistOf(types.UID("uid-a"), types.UID("uid-b")))
			expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
				"memory":      resource.MustParse("384Mi"),
				"connections": resource.MustParse("4"),
			})
		})

		It("surfaces per-claim contributions paired with their reserving pods", func() {
			claimA := withReservedFor(
				resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
					"memory":      resource.MustParse("256Mi"),
					"connections": resource.MustParse("1"),
				})),
				podRef("pod-a", "uid-a"),
			)
			claimB := withReservedFor(
				resourceClaim("claim-b", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
					"memory":      resource.MustParse("128Mi"),
					"connections": resource.MustParse("3"),
				})),
				podRef("pod-b", "uid-b"),
			)
			ExpectApplied(ctx, env.Client, claimA, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			meta := devices[deviceID("device-0")]
			Expect(meta.Shared).To(BeTrue())
			// One contribution per referencing claim, each attributed to that claim's reserving pod and carrying only
			// that claim's capacity (not the aggregate).
			Expect(meta.Contributions).To(HaveLen(2))
			contributionForPod := func(uid types.UID) deviceallocation.ContributionMetadata {
				for _, c := range meta.Contributions {
					for _, podUID := range c.PodUIDs {
						if podUID == uid {
							return c
						}
					}
				}
				return deviceallocation.ContributionMetadata{}
			}
			expectCapacity(contributionForPod("uid-a").ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
				"memory":      resource.MustParse("256Mi"),
				"connections": resource.MustParse("1"),
			})
			expectCapacity(contributionForPod("uid-b").ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
				"memory":      resource.MustParse("128Mi"),
				"connections": resource.MustParse("3"),
			})
		})

		It("does not mark a device as shared when ConsumedCapacity is nil (exclusive allocation)", func() {
			claim := withReservedFor(
				resourceClaim("claim-a", deviceResult("device-0")),
				podRef("pod-a", "uid-a"),
			)
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			meta := devices[deviceID("device-0")]
			Expect(meta.Shared).To(BeFalse())
			Expect(meta.ConsumedCapacity).To(BeNil())
			Expect(meta.Contributions).To(BeEmpty())
		})

		It("handles a mix of shared and exclusive devices in the same claim", func() {
			claim := withReservedFor(
				resourceClaim("claim-a",
					deviceResultWithCapacity("shared-dev", map[resourcev1.QualifiedName]resource.Quantity{
						"slots": resource.MustParse("4"),
					}),
					deviceResult("exclusive-dev"),
				),
				podRef("pod-a", "uid-a"),
			)
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)

			sharedMeta := devices[deviceID("shared-dev")]
			Expect(sharedMeta.Shared).To(BeTrue())
			expectCapacity(sharedMeta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
				"slots": resource.MustParse("4"),
			})

			exclusiveMeta := devices[deviceID("exclusive-dev")]
			Expect(exclusiveMeta.Shared).To(BeFalse())
			Expect(exclusiveMeta.ConsumedCapacity).To(BeNil())
		})

		It("aggregates capacity with partially overlapping keys from different claims", func() {
			claimA := withReservedFor(
				resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("256Mi"),
					"slots":  resource.MustParse("2"),
				})),
				podRef("pod-a", "uid-a"),
			)
			claimB := withReservedFor(
				resourceClaim("claim-b", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
					"memory":      resource.MustParse("128Mi"),
					"connections": resource.MustParse("1"),
				})),
				podRef("pod-b", "uid-b"),
			)
			ExpectApplied(ctx, env.Client, claimA, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			meta := devices[deviceID("device-0")]
			Expect(meta.Shared).To(BeTrue())
			expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
				"memory":      resource.MustParse("384Mi"),
				"slots":       resource.MustParse("2"),
				"connections": resource.MustParse("1"),
			})
		})

		It("aggregates capacity with distinct keys from different claims", func() {
			claimA := withReservedFor(
				resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("256Mi"),
				})),
				podRef("pod-a", "uid-a"),
			)
			claimB := withReservedFor(
				resourceClaim("claim-b", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
					"connections": resource.MustParse("5"),
				})),
				podRef("pod-b", "uid-b"),
			)
			ExpectApplied(ctx, env.Client, claimA, claimB)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			meta := devices[deviceID("device-0")]
			Expect(meta.Shared).To(BeTrue())
			expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
				"memory":      resource.MustParse("256Mi"),
				"connections": resource.MustParse("5"),
			})
		})

		It("is non-releasable when a shared device has empty ReservedFor", func() {
			claim := resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
				"memory": resource.MustParse("128Mi"),
			}))
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			meta := devices[deviceID("device-0")]
			Expect(meta.Releasable).To(BeFalse())
			Expect(meta.Shared).To(BeTrue())
			expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
				"memory": resource.MustParse("128Mi"),
			})
		})

		It("is non-releasable with capacity when a shared device has a non-pod consumer", func() {
			claim := withReservedFor(
				resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("256Mi"),
				})),
				nonPodRef(),
			)
			ExpectApplied(ctx, env.Client, claim)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

			seq, err := controller.AllocatedDevices(ctx)
			Expect(err).ToNot(HaveOccurred())
			devices := collectDevices(seq)
			meta := devices[deviceID("device-0")]
			Expect(meta.Releasable).To(BeFalse())
			Expect(meta.Shared).To(BeTrue())
			Expect(meta.PodUIDs).To(BeNil())
			expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
				"memory": resource.MustParse("256Mi"),
			})
		})

		Describe("Allocation Changes", func() {
			It("reduces aggregated capacity when one of multiple shared claims is deleted", func() {
				claimA := withReservedFor(
					resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
						"memory": resource.MustParse("256Mi"),
					})),
					podRef("pod-a", "uid-a"),
				)
				claimB := withReservedFor(
					resourceClaim("claim-b", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
						"memory": resource.MustParse("128Mi"),
					})),
					podRef("pod-b", "uid-b"),
				)
				ExpectApplied(ctx, env.Client, claimA, claimB)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

				ExpectDeleted(ctx, env.Client, claimB)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				meta := devices[deviceID("device-0")]
				Expect(meta.Shared).To(BeTrue())
				Expect(meta.PodUIDs).To(ConsistOf(types.UID("uid-a")))
				expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("256Mi"),
				})
			})

			It("removes shared device entirely when all claims with capacity are deleted", func() {
				claimA := withReservedFor(
					resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
						"memory": resource.MustParse("256Mi"),
					})),
					podRef("pod-a", "uid-a"),
				)
				ExpectApplied(ctx, env.Client, claimA)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))

				ExpectDeleted(ctx, env.Client, claimA)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				Expect(devices).To(BeEmpty())
			})

			It("updates capacity when a claim is recreated with more capacity on the same device", func() {
				claim := withReservedFor(
					resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
						"memory": resource.MustParse("128Mi"),
					})),
					podRef("pod-a", "uid-a"),
				)
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				ExpectDeleted(ctx, env.Client, claim)
				claim = withReservedFor(
					resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
						"memory": resource.MustParse("512Mi"),
					})),
					podRef("pod-a", "uid-a"),
				)
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				meta := devices[deviceID("device-0")]
				Expect(meta.Shared).To(BeTrue())
				expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("512Mi"),
				})
			})

			It("removes stale devices when a claim's allocation replaces one shared device with another", func() {
				claim := withReservedFor(
					resourceClaim("claim-a", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
						"slots": resource.MustParse("2"),
					})),
					podRef("pod-a", "uid-a"),
				)
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				ExpectDeleted(ctx, env.Client, claim)
				claim = withReservedFor(
					resourceClaim("claim-a", deviceResultWithCapacity("device-1", map[resourcev1.QualifiedName]resource.Quantity{
						"slots": resource.MustParse("3"),
					})),
					podRef("pod-a", "uid-a"),
				)
				ExpectApplied(ctx, env.Client, claim)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claim))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				Expect(devices).ToNot(HaveKey(deviceID("device-0")))
				meta := devices[deviceID("device-1")]
				Expect(meta.Releasable).To(BeTrue())
				Expect(meta.PodUIDs).To(ConsistOf(types.UID("uid-a")))
				Expect(meta.Shared).To(BeTrue())
				expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
					"slots": resource.MustParse("3"),
				})
			})

			It("transitions a device from shared to exclusive when all capacity claims are deleted", func() {
				claimA := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					podRef("pod-a", "uid-a"),
				)
				claimB := withReservedFor(
					resourceClaim("claim-b", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
						"memory": resource.MustParse("128Mi"),
					})),
					podRef("pod-b", "uid-b"),
				)
				ExpectApplied(ctx, env.Client, claimA, claimB)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				Expect(devices[deviceID("device-0")].Shared).To(BeTrue())

				ExpectDeleted(ctx, env.Client, claimB)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

				seq, err = controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices = collectDevices(seq)
				meta := devices[deviceID("device-0")]
				Expect(meta.Shared).To(BeFalse())
				Expect(meta.ConsumedCapacity).To(BeNil())
				Expect(meta.Releasable).To(BeTrue())
				Expect(meta.PodUIDs).To(ConsistOf(types.UID("uid-a")))
			})

			It("transitions a device from exclusive to shared when a second claim with capacity references it", func() {
				claimA := withReservedFor(
					resourceClaim("claim-a", deviceResult("device-0")),
					podRef("pod-a", "uid-a"),
				)
				ExpectApplied(ctx, env.Client, claimA)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimA))

				seq, err := controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices := collectDevices(seq)
				Expect(devices[deviceID("device-0")].Shared).To(BeFalse())

				claimB := withReservedFor(
					resourceClaim("claim-b", deviceResultWithCapacity("device-0", map[resourcev1.QualifiedName]resource.Quantity{
						"memory": resource.MustParse("128Mi"),
					})),
					podRef("pod-b", "uid-b"),
				)
				ExpectApplied(ctx, env.Client, claimB)
				ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(claimB))

				seq, err = controller.AllocatedDevices(ctx)
				Expect(err).ToNot(HaveOccurred())
				devices = collectDevices(seq)
				meta := devices[deviceID("device-0")]
				Expect(meta.Shared).To(BeTrue())
				expectCapacity(meta.ConsumedCapacity, map[resourcev1.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("128Mi"),
				})
				Expect(meta.PodUIDs).To(ConsistOf(types.UID("uid-a"), types.UID("uid-b")))
			})
		})
	})

})
