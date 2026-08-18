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

package deletioncost

import (
	"context"
	"strconv"
	"time"

	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

// podPatchWorkers caps the parallel-dispatch fan-out for per-pod patches. At
// 50 nodes per cycle and an average of 30 pods per node this bounds concurrent
// in-flight patches to 10 while still delivering roughly a 10x wall-time
// speedup vs the fully-sequential shape. It is deliberately lower than
// state/statenode.go's implicit N=len(nodes) because pods per cycle can be an
// order of magnitude larger than nodes per cycle, and we want this
// alpha-feature controller to stay behind higher-priority controllers when
// the apiserver is under load.
const podPatchWorkers = 10

// podPatchRetryBackoff is the per-pod exponential backoff used for retryable
// patch errors (429 TooManyRequests, server timeouts). Kept slower than
// retry.DefaultBackoff (10ms base, 5x factor) because if the apiserver is
// already throttling us, this controller should back off aggressively and
// yield to higher-priority controllers rather than tighten the retry loop.
var podPatchRetryBackoff = wait.Backoff{
	Steps:    4,
	Duration: 200 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.2,
}

// isRetryableAPIError reports whether err is a transient failure that a
// per-pod retry loop should back off and retry. NotFound and Conflict are
// deliberately NOT retryable: NotFound means the pod is gone (skip) and
// Conflict means another writer raced us (skip and retry on the next
// reconcile cycle instead).
func isRetryableAPIError(err error) bool {
	return apierrors.IsTooManyRequests(err) || apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err)
}

// podOutcome captures the result of a single per-pod dispatch. Exactly one of
// the three counters is incremented per outcome; err is set only when result
// is "errored".
type podOutcome struct {
	result string
	err    error
}

const (
	outcomeUpdated = "updated"
	outcomeSkipped = "skipped"
	outcomeErrored = "errored"
)

// podPatchStats aggregates per-pod patch outcomes across a single reconcile.
// Carried by value through helpers so the outer reconcile can fold per-iteration
// stats with += and emit metrics once at the end.
type podPatchStats struct {
	updated   int
	skipped   int
	podErrors int
}

func (s *podPatchStats) add(other podPatchStats) {
	s.updated += other.updated
	s.skipped += other.skipped
	s.podErrors += other.podErrors
}

// UpdatePodDeletionCosts updates pod deletion cost annotations for all pods on
// the ranked nodes. When the feature gate is on the controller writes
// pod-deletion-cost directly: there is no overwrite-protection for
// customer-set values. Customers steering Karpenter consolidation are expected
// to use karpenter.sh/disruption-cost instead.
//
// Per-pod dispatch runs in parallel via workqueue.ParallelizeUntil, bounded
// by podPatchWorkers. Retryable failures (429 TooManyRequests, server
// timeout, unavailable) are retried per-pod under podPatchRetryBackoff so a
// throttled apiserver does not fail an entire cycle; per-pod errors are
// aggregated via multierr.Append so the caller sees the full set of
// failures.
//
// NotFound and Conflict on the patch are both classified as Skipped: NotFound
// means the pod is already gone, Conflict means another writer raced us and
// we'll retry on the next reconcile. Neither is an operator-visible failure.
func UpdatePodDeletionCosts(ctx context.Context, kubeClient client.Client, nodeRanks []NodeRank) error {
	defer metrics.Measure(annotationDurationSeconds, noLabels)()

	var totals podPatchStats
	var aggErr error

	for _, nodeRank := range nodeRanks {
		// Pods are captured on NodeRank during RankNodes so we do not re-list
		// per node here. If RankNodes ever produces a nil Pods slice (empty
		// node), the helpers treat it as zero pods.
		pods := nodeRank.Pods
		var stats podPatchStats
		var perr error
		if nodeRank.HasDoNotDisrupt {
			stats, perr = clearRanksFromPods(ctx, kubeClient, pods)
		} else {
			stats, perr = applyRankToPods(ctx, kubeClient, pods, nodeRank.Rank)
		}
		totals.add(stats)
		if perr != nil {
			aggErr = multierr.Append(aggErr, perr)
		}
	}

	podsUpdatedTotal.Add(float64(totals.updated), map[string]string{resultLabel: "updated"})
	podsUpdatedTotal.Add(float64(totals.skipped), map[string]string{resultLabel: "skipped_unchanged"})
	podsUpdatedTotal.Add(float64(totals.podErrors), map[string]string{resultLabel: "error"})

	if totals.updated > 0 || totals.podErrors > 0 {
		log.FromContext(ctx).WithValues(
			"updated", totals.updated,
			"skipped", totals.skipped,
			"podErrors", totals.podErrors,
		).V(1).Info("pod deletion cost annotation update completed")
	}
	return aggErr
}

// applyRankToPods writes pod-deletion-cost=rank to each pod. Fans out via
// dispatchPerPod so the apply and clear paths share the same worker pool,
// retry policy, and NotFound/Conflict classification.
func applyRankToPods(ctx context.Context, kubeClient client.Client, pods []*corev1.Pod, rank int) (podPatchStats, error) {
	value := strconv.Itoa(rank)
	return dispatchPerPod(ctx, pods, applyRankOp(kubeClient, value))
}

// clearRanksFromPods removes pod-deletion-cost from each pod. Fans out via
// dispatchPerPod (symmetric with applyRankToPods).
func clearRanksFromPods(ctx context.Context, kubeClient client.Client, pods []*corev1.Pod) (podPatchStats, error) {
	return dispatchPerPod(ctx, pods, clearRankOp(kubeClient))
}

// podOp describes a single logical per-pod annotation operation. The two
// closures split responsibilities so the shared dispatcher can decide
// (a) whether the pod already matches the desired state (skip the API call
// entirely) and (b) how to actually mutate the pod. shouldSkip and patch
// both accept the pod by pointer without mutating it (patch clones
// internally); dispatchPerPod does not need to defensively deep-copy.
//
// notFoundMsg and conflictMsg are the V(1) log messages emitted when the
// apiserver returns NotFound or Conflict respectively; kept literal so the
// two paths read grammatically ("skipping annotation update" vs
// "skipping annotation clear", "conflict updating" vs "conflict clearing")
// rather than templated through a single verb slot.
type podOp struct {
	shouldSkip  func(*corev1.Pod) bool
	patch       func(context.Context, *corev1.Pod) error
	notFoundMsg string
	conflictMsg string
}

// applyRankOp builds the podOp that writes pod-deletion-cost=value.
func applyRankOp(kubeClient client.Client, value string) podOp {
	return podOp{
		shouldSkip:  func(pod *corev1.Pod) bool { return pod.Annotations[corev1.PodDeletionCost] == value },
		patch:       func(ctx context.Context, pod *corev1.Pod) error { return patchAnnotation(ctx, kubeClient, pod, value) },
		notFoundMsg: "pod not found, skipping annotation update",
		conflictMsg: "conflict updating pod annotation, will retry on next reconcile",
	}
}

// clearRankOp builds the podOp that removes pod-deletion-cost.
func clearRankOp(kubeClient client.Client) podOp {
	return podOp{
		shouldSkip: func(pod *corev1.Pod) bool {
			_, ok := pod.Annotations[corev1.PodDeletionCost]
			return !ok
		},
		patch:       func(ctx context.Context, pod *corev1.Pod) error { return clearAnnotation(ctx, kubeClient, pod) },
		notFoundMsg: "pod not found, skipping annotation clear",
		conflictMsg: "conflict clearing pod annotation, will retry on next reconcile",
	}
}

// dispatchPerPod fans op out across pods via runPodOp under a bounded worker
// pool so a large per-node pod list does not blast the apiserver with a burst
// of goroutines. Per-pod outcome classification (retry/skip/error) lives in
// runPodOp; foldOutcomes aggregates. Bound is podPatchWorkers.
func dispatchPerPod(ctx context.Context, pods []*corev1.Pod, op podOp) (podPatchStats, error) {
	outcomes := make([]podOutcome, len(pods))
	workqueue.ParallelizeUntil(ctx, podPatchWorkers, len(pods), func(i int) {
		outcomes[i] = runPodOp(ctx, pods[i], op)
	})
	return foldOutcomes(outcomes)
}

// runPodOp classifies a single-pod operation into a podOutcome. Retryable
// failures (429, server timeout, unavailable) are retried under
// podPatchRetryBackoff; NotFound and Conflict short-circuit as skips
// (client.IgnoreNotFound would silently swallow both, but we need to count
// skips and log at V(1)); anything else surfaces as outcomeErrored so the
// caller can aggregate via multierr. 429s intentionally do not fall through
// to the controller-runtime rate limiter — per-pod retry keeps the burden on
// the individual pod instead of failing the whole cycle.
func runPodOp(ctx context.Context, pod *corev1.Pod, op podOp) podOutcome {
	if op.shouldSkip(pod) {
		return podOutcome{result: outcomeSkipped}
	}
	err := retry.OnError(podPatchRetryBackoff, isRetryableAPIError, func() error {
		return op.patch(ctx, pod)
	})
	if err == nil {
		return podOutcome{result: outcomeUpdated}
	}
	if apierrors.IsNotFound(err) {
		log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info(op.notFoundMsg)
		return podOutcome{result: outcomeSkipped}
	}
	if apierrors.IsConflict(err) {
		log.FromContext(ctx).V(1).WithValues("pod", klog.KObj(pod)).Info(op.conflictMsg)
		return podOutcome{result: outcomeSkipped}
	}
	return podOutcome{result: outcomeErrored, err: err}
}

// foldOutcomes rolls a slice of per-pod outcomes into a single podPatchStats
// and aggregated error. Each outcome contributes to exactly one of the three
// counters; errored outcomes also append their err to the multierr.
func foldOutcomes(outcomes []podOutcome) (podPatchStats, error) {
	var stats podPatchStats
	var err error
	for i := range outcomes {
		switch outcomes[i].result {
		case outcomeUpdated:
			stats.updated++
		case outcomeSkipped:
			stats.skipped++
		case outcomeErrored:
			stats.podErrors++
			err = multierr.Append(err, outcomes[i].err)
		}
	}
	return stats, err
}

// clearAnnotation removes the pod-deletion-cost annotation from a pod via a
// merge patch with optimistic-lock. Returns the raw error from the apiserver;
// classification (NotFound/Conflict/retryable/hard-error) lives in runPodOp,
// symmetric with patchAnnotation and the write path.
//
// Uses MergeFromWithOptimisticLock because we want to detect races with other
// writers of pod-deletion-cost (customer kubectl, third-party HPAs, admission
// webhooks) via 409 Conflict. The Conflict-skip semantics allow us to
// converge on the next reconcile without overwriting a value that was written
// between our read and write. This is different from the more-common
// rationale in this codebase (list-merge-replace protection); see
// pkg/controllers/nodeclaim/lifecycle/controller.go:295-309 for a similar
// annotation-race precedent.
func clearAnnotation(ctx context.Context, kubeClient client.Client, pod *corev1.Pod) error {
	updated := pod.DeepCopy()
	delete(updated.Annotations, corev1.PodDeletionCost)
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	return kubeClient.Patch(ctx, updated, patch)
}

// patchAnnotation sets the pod-deletion-cost annotation on a pod via a merge
// patch with optimistic-lock. The caller's pod object is not mutated; we
// mutate a deep-copy.
//
// Uses MergeFromWithOptimisticLock because we want to detect races with other
// writers of pod-deletion-cost (customer kubectl, third-party HPAs, admission
// webhooks) via 409 Conflict. The Conflict-skip semantics allow us to
// converge on the next reconcile without overwriting a value that was written
// between our read and write. This is different from the more-common
// rationale in this codebase (list-merge-replace protection, e.g.
// state/statenode.go:523-526); see
// pkg/controllers/nodeclaim/lifecycle/controller.go:295-309 for a similar
// annotation-race precedent.
func patchAnnotation(ctx context.Context, kubeClient client.Client, pod *corev1.Pod, value string) error {
	updated := pod.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[corev1.PodDeletionCost] = value
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	return kubeClient.Patch(ctx, updated, patch)
}
