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

package scheduling_test

import (
	"context"
	"sync"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// countingSink is a logr.LogSink that counts Error calls carrying the
// "could not schedule pod" message produced by Results.recordPodErrors. It
// lets tests assert how many distinct error logs were actually emitted,
// without relying on private cache state.
type countingSink struct {
	mu    sync.Mutex
	count int
}

const podSchedulingErrorMsg = "could not schedule pod"

func (s *countingSink) Init(logr.RuntimeInfo)            {}
func (s *countingSink) Enabled(int) bool                 { return true }
func (s *countingSink) Info(int, string, ...interface{}) {}
func (s *countingSink) WithName(string) logr.LogSink     { return s }
func (s *countingSink) WithValues(...interface{}) logr.LogSink {
	return s
}
func (s *countingSink) Error(_ error, msg string, _ ...interface{}) {
	if msg != podSchedulingErrorMsg {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
}

func (s *countingSink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// withCountingLogger wraps ctx with a counting logr sink and returns both the
// new context and the sink so the test can later assert on the count.
func withCountingLogger(ctx context.Context) (context.Context, *countingSink) {
	sink := &countingSink{}
	return log.IntoContext(ctx, logr.New(sink)), sink
}

var _ = Describe("Log Deduplication", func() {
	It("deduplicates identical pod+error logs across separate scheduling cycles", func() {
		nodePool := test.NodePool()
		nodePool.Spec.Template.Spec.Taints = []corev1.Taint{
			{Key: "special-workload", Value: "true", Effect: corev1.TaintEffectNoSchedule},
		}
		ExpectApplied(ctx, env.Client, nodePool)

		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})

		// Drive provisioning across multiple Schedule cycles. Each call
		// constructs a fresh Scheduler under the hood, so this exercises the
		// previously-broken behavior where the dedup cache was created per
		// Solve() and never actually deduplicated.
		testCtx, sink := withCountingLogger(ctx)
		for i := 0; i < 5; i++ {
			ExpectProvisioned(testCtx, env.Client, cluster, cloudProvider, prov, pod)
		}
		ExpectNotScheduled(ctx, env.Client, pod)
		Expect(sink.Count()).To(Equal(1), "the same pod+error should only be logged once across scheduling cycles")
	})

	It("logs distinct pods independently", func() {
		nodePool := test.NodePool()
		nodePool.Spec.Template.Spec.Taints = []corev1.Taint{
			{Key: "special-workload", Value: "true", Effect: corev1.TaintEffectNoSchedule},
		}
		ExpectApplied(ctx, env.Client, nodePool)

		pod1 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})
		pod2 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			},
		})

		testCtx, sink := withCountingLogger(ctx)
		ExpectProvisioned(testCtx, env.Client, cluster, cloudProvider, prov, pod1, pod2)
		ExpectNotScheduled(ctx, env.Client, pod1)
		ExpectNotScheduled(ctx, env.Client, pod2)
		Expect(sink.Count()).To(Equal(2), "two distinct pods should each produce one error log")
	})

	It("logs distinct errors for the same pod independently", func() {
		// The cache key includes the error message, so changing the failure
		// reason for a pod must result in a fresh log line — otherwise users
		// would miss new failure modes.
		nodePool := test.NodePool()
		nodePool.Spec.Template.Spec.Taints = []corev1.Taint{
			{Key: "special-workload", Value: "true", Effect: corev1.TaintEffectNoSchedule},
		}
		ExpectApplied(ctx, env.Client, nodePool)

		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		})

		testCtx, sink := withCountingLogger(ctx)
		ExpectProvisioned(testCtx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
		firstCount := sink.Count()
		Expect(firstCount).To(BeNumerically(">=", 1))

		// Replace the taint so the same pod now fails with a different error
		// message. The shared cache should NOT suppress this new log line.
		ExpectDeleted(ctx, env.Client, nodePool)
		nodePool2 := test.NodePool()
		nodePool2.Spec.Template.Spec.Taints = []corev1.Taint{
			{Key: "another-taint", Value: "true", Effect: corev1.TaintEffectNoSchedule},
		}
		ExpectApplied(ctx, env.Client, nodePool2)

		ExpectProvisioned(testCtx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
		Expect(sink.Count()).To(BeNumerically(">", firstCount),
			"a different error for the same pod should produce an additional log line")
	})
})
