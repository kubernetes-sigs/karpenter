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
func collectDevices(seq iter.Seq2[cloudprovider.DeviceID, deviceallocation.Metadata]) map[cloudprovider.DeviceID]deviceallocation.Metadata {
	m := make(map[cloudprovider.DeviceID]deviceallocation.Metadata)
	for id, meta := range seq {
		m[id] = meta
	}
	return m
}

// expectedDevices builds the expected map with zero-value metadata for each device.
func expectedDevices(ids ...cloudprovider.DeviceID) map[cloudprovider.DeviceID]deviceallocation.Metadata {
	m := make(map[cloudprovider.DeviceID]deviceallocation.Metadata, len(ids))
	for _, id := range ids {
		m[id] = deviceallocation.Metadata{}
	}
	return m
}

// triggerHydration reconciles a no-allocation claim to fire the controller's hydrationOnce, making
// subsequent AllocatedDevices calls non-blocking. Call this at the start of any test that is not
// explicitly testing the hydration blocking behavior.
func triggerHydration() {
	dummy := resourceClaim("hydration-trigger")
	ExpectApplied(ctx, env.Client, dummy)
	ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(dummy))
}

var _ = Describe("DeviceAllocation Controller", func() {
	Describe("Hydration", func() {
		It("blocks until the first reconcile completes", func() {
			results := make(chan map[cloudprovider.DeviceID]deviceallocation.Metadata, 1)
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
					deviceallocation.Metadata{Releasable: true, PodUIDs: []types.UID{"uid-a"}},
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
					deviceallocation.Metadata{Releasable: true, PodUIDs: []types.UID{"uid-a", "uid-b"}},
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
					deviceallocation.Metadata{Releasable: false},
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
					deviceallocation.Metadata{Releasable: true, PodUIDs: []types.UID{"uid-a"}},
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
					deviceallocation.Metadata{Releasable: true, PodUIDs: []types.UID{"uid-a"}},
				))
			})
		})
	})
})
