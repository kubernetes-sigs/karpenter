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

package prediction

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// SupportedTargets are the workload kinds a pod can be resolved to through its owners, so the only kinds a prediction
// can apply to. Pods of other workloads, e.g. an Argo Rollout, resolve to their ReplicaSet or to no target.
var SupportedTargets = sets.New(
	schema.GroupKind{Group: "apps", Kind: "Deployment"},
	schema.GroupKind{Group: "apps", Kind: "ReplicaSet"},
	schema.GroupKind{Group: "apps", Kind: "StatefulSet"},
	schema.GroupKind{Group: "apps", Kind: "DaemonSet"},
	schema.GroupKind{Group: "", Kind: "ReplicationController"},
	schema.GroupKind{Group: "batch", Kind: "Job"},
	schema.GroupKind{Group: "batch", Kind: "CronJob"},
)

// WorkloadReference identifies the workload a prediction applies to by name rather than UID, matching how a VPA targets
// workloads, so a workload that's deleted and recreated with the same name keeps its prediction.
type WorkloadReference struct {
	schema.GroupKind
	types.NamespacedName
}

// Prediction holds the predicted resource requests for all containers of a workload.
// Maps container name to its predicted resource requests.
type Prediction struct {
	Containers map[string]corev1.ResourceList
}

// workloadEntry pairs a prediction with metadata about its source for tie-breaking.
type workloadEntry struct {
	prediction *Prediction
	source     types.NamespacedName
	createdAt  time.Time
}

// Store is a thread-safe cache of predictions, indexed for O(1) lookup
// by workload. When multiple sources target the same workload,
// the store uses VPA's tie-breaking semantics (earliest creation time wins,
// then lexicographically smallest name) to determine which prediction is active.
type Store struct {
	sync.RWMutex
	// kubeClient resolves a pod's owners to the workload its prediction is stored under
	kubeClient    client.Client
	hydrationCh   chan struct{}
	hydrationOnce sync.Once
	// byWorkload indexes all contending predictions by the workload they apply to.
	// Entries are sorted by strength (strongest first).
	byWorkload map[WorkloadReference][]workloadEntry
	// bySource maps the prediction source identity to its WorkloadReference, for deletion cleanup.
	bySource    map[types.NamespacedName]WorkloadReference
	targetKinds sets.Set[schema.GroupKind]
}

func NewStore(kubeClient client.Client) *Store {
	return &Store{
		kubeClient:  kubeClient,
		hydrationCh: make(chan struct{}),
		byWorkload:  make(map[WorkloadReference][]workloadEntry),
		bySource:    make(map[types.NamespacedName]WorkloadReference),
		targetKinds: sets.New[schema.GroupKind](),
	}
}

func (s *Store) RegisterTargetKinds(ctx context.Context, informers cache.Informers, kinds ...schema.GroupKind) error {
	for _, kind := range kinds {
		if !SupportedTargets.Has(kind) {
			return serrors.Wrap(fmt.Errorf("unsupported prediction target kind"), "kind", kind.Kind)
		}
		s.targetKinds.Insert(kind)
		var owner schema.GroupVersionKind
		switch kind {
		case schema.GroupKind{Group: "apps", Kind: "Deployment"}:
			owner = appsv1.SchemeGroupVersion.WithKind("ReplicaSet")
		case schema.GroupKind{Group: "batch", Kind: "CronJob"}:
			owner = batchv1.SchemeGroupVersion.WithKind("Job")
		default:
			continue
		}
		obj := &metav1.PartialObjectMetadata{}
		obj.SetGroupVersionKind(owner)
		if _, err := informers.GetInformer(ctx, obj); err != nil {
			return serrors.Wrap(fmt.Errorf("registering owner informer, %w", err), "kind", owner.Kind)
		}
	}
	return nil
}

func (s *Store) MarkHydrated() {
	s.hydrationOnce.Do(func() { close(s.hydrationCh) })
}

var HydrationTimeout = time.Minute

func (s *Store) Hydrated(ctx context.Context) bool {
	select {
	case <-s.hydrationCh:
		return true
	case <-ctx.Done():
		return false
	case <-time.After(HydrationTimeout):
		panic(fmt.Sprintf("prediction store hasn't hydrated after %s, check that a prediction source is registered and can list its objects", HydrationTimeout))
	}
}

// Reset removes all predictions and reports whether any were active.
func (s *Store) Reset() bool {
	s.Lock()
	defer s.Unlock()
	changed := len(s.byWorkload) > 0
	s.byWorkload = make(map[WorkloadReference][]workloadEntry)
	s.bySource = make(map[types.NamespacedName]WorkloadReference)
	return changed
}

func (s *Store) Len() int {
	s.RLock()
	defer s.RUnlock()
	return len(s.byWorkload)
}

// Set stores a prediction from the given source for the given workload and reports whether the active prediction of any
// workload changed. If the source previously targeted a different workload, the old entry is removed.
func (s *Store) Set(source types.NamespacedName, workload WorkloadReference, p *Prediction, createdAt time.Time) bool {
	s.Lock()
	defer s.Unlock()

	changed := false
	if prev, ok := s.bySource[source]; ok && prev != workload {
		changed = s.removeEntry(prev, source)
	}
	before, _ := s.active(workload)

	s.bySource[source] = workload

	entries := s.byWorkload[workload]
	found := false
	for i := range entries {
		if entries[i].source == source {
			entries[i].prediction = p
			entries[i].createdAt = createdAt
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, workloadEntry{
			prediction: p,
			source:     source,
			createdAt:  createdAt,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return stronger(entries[i], entries[j])
	})
	s.byWorkload[workload] = entries
	after, _ := s.active(workload)
	// Compare by value since sources recompute their prediction on every update, even when it's unchanged
	return !equality.Semantic.DeepEqual(before, after) || changed
}

// Delete removes the prediction from the given source and reports whether the workload's active prediction changed. If
// other sources target the same workload, the next-strongest is automatically promoted.
func (s *Store) Delete(source types.NamespacedName) bool {
	s.Lock()
	defer s.Unlock()

	workload, ok := s.bySource[source]
	if !ok {
		return false
	}
	delete(s.bySource, source)
	return s.removeEntry(workload, source)
}

// Get returns the active (strongest) prediction for the given workload.
// Callers should not mutate the returned Prediction.
func (s *Store) Get(workload WorkloadReference) (*Prediction, bool) {
	s.RLock()
	defer s.RUnlock()
	return s.active(workload)
}

// removeEntry removes the entry for the given source from the workload's list and reports whether the workload's active
// prediction changed. If the list becomes empty, the workload is removed from the map.
func (s *Store) removeEntry(workload WorkloadReference, source types.NamespacedName) bool {
	before, _ := s.active(workload)
	entries := s.byWorkload[workload]
	for i := range entries {
		if entries[i].source == source {
			entries = append(entries[:i], entries[i+1:]...)
			break
		}
	}
	if len(entries) == 0 {
		delete(s.byWorkload, workload)
	} else {
		s.byWorkload[workload] = entries
	}
	after, _ := s.active(workload)
	return !equality.Semantic.DeepEqual(before, after)
}

func (s *Store) active(workload WorkloadReference) (*Prediction, bool) {
	entries := s.byWorkload[workload]
	if len(entries) == 0 {
		return nil, false
	}
	return entries[0].prediction, true
}

func stronger(a, b workloadEntry) bool {
	if !a.createdAt.Equal(b.createdAt) {
		return a.createdAt.Before(b.createdAt)
	}
	return a.source.String() < b.source.String()
}

var podCountOne = resource.MustParse("1")

// OwnerResolutions is only valid for a single scheduling run, since owners come and go, e.g. a new ReplicaSet on every
// rollout.
type OwnerResolutions map[types.UID]WorkloadReference

// PredictedRequests returns the total resource requests of the pods with predictions applied. For each container with
// a prediction, it replaces current requests with the predicted value. Containers without a prediction, and pods
// whose workload has no prediction, keep their current requests. cachedOwnerResolutions may be nil.
func (s *Store) PredictedRequests(ctx context.Context, cachedOwnerResolutions OwnerResolutions, pods ...*corev1.Pod) corev1.ResourceList {
	if s.Len() == 0 {
		return resources.RequestsForPods(pods...)
	}
	return resources.Merge(lo.Map(pods, func(pod *corev1.Pod, _ int) corev1.ResourceList {
		workload, ok := s.resolveWorkload(ctx, pod, cachedOwnerResolutions)
		if !ok {
			return resources.RequestsForPods(pod)
		}
		pred, ok := s.Get(workload)
		if !ok {
			return resources.RequestsForPods(pod)
		}
		result := resources.RequestsForSpec(&corev1.PodSpec{
			Containers:     applyPredictions(pod.Spec.Containers, pred),
			InitContainers: applyPredictions(pod.Spec.InitContainers, pred),
			Overhead:       pod.Spec.Overhead,
			Resources:      pod.Spec.Resources,
		})
		result[corev1.ResourcePods] = podCountOne
		return result
	})...)
}

// resolveWorkload resolves a pod to the workload its prediction is stored under, which is always one of
// SupportedTargets. Keep the two in sync.
//
//nolint:gocyclo
func (s *Store) resolveWorkload(ctx context.Context, pod *corev1.Pod, cachedOwnerResolutions OwnerResolutions) (WorkloadReference, bool) {
	ref := metav1.GetControllerOfNoCopy(pod)
	if ref == nil {
		return WorkloadReference{}, false
	}
	if workload, ok := cachedOwnerResolutions[ref.UID]; ok {
		return workload, workload != WorkloadReference{}
	}
	var workloadRef *metav1.OwnerReference
	switch ref.Kind {
	case "ReplicaSet":
		if !s.targetKinds.Has(schema.GroupKind{Group: "apps", Kind: "Deployment"}) {
			workloadRef = ref
			break
		}
		rs := &metav1.PartialObjectMetadata{}
		rs.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"})
		if err := s.kubeClient.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ref.Name}, rs); err != nil {
			if !apierrors.IsNotFound(err) {
				log.FromContext(ctx).WithValues("Pod", klog.KObj(pod), "ReplicaSet", klog.KRef(pod.Namespace, ref.Name)).V(1).Info("failed resolving pod owner, skipping prediction", "error", err)
			}
			break
		}
		workloadRef = ref
		if owner := metav1.GetControllerOfNoCopy(rs); owner != nil && owner.Kind == "Deployment" {
			workloadRef = owner
		}
	case "StatefulSet", "DaemonSet", "ReplicationController":
		workloadRef = ref
	case "Job":
		if !s.targetKinds.Has(schema.GroupKind{Group: "batch", Kind: "CronJob"}) {
			workloadRef = ref
			break
		}
		job := &metav1.PartialObjectMetadata{}
		job.SetGroupVersionKind(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"})
		if err := s.kubeClient.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ref.Name}, job); err != nil {
			if !apierrors.IsNotFound(err) {
				log.FromContext(ctx).WithValues("Pod", klog.KObj(pod), "Job", klog.KRef(pod.Namespace, ref.Name)).V(1).Info("failed resolving pod owner, skipping prediction", "error", err)
			}
			break
		}
		workloadRef = ref
		if owner := metav1.GetControllerOfNoCopy(job); owner != nil && owner.Kind == "CronJob" {
			workloadRef = owner
		}
	}
	var workload WorkloadReference
	if workloadRef != nil {
		workload = WorkloadReference{
			GroupKind:      schema.FromAPIVersionAndKind(workloadRef.APIVersion, workloadRef.Kind).GroupKind(),
			NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: workloadRef.Name},
		}
	}
	if !s.targetKinds.Has(workload.GroupKind) {
		if cachedOwnerResolutions != nil {
			cachedOwnerResolutions[ref.UID] = WorkloadReference{}
		}
		return WorkloadReference{}, false
	}
	if cachedOwnerResolutions != nil {
		cachedOwnerResolutions[ref.UID] = workload
	}
	return workload, true
}

func applyPredictions(containers []corev1.Container, pred *Prediction) []corev1.Container {
	result := make([]corev1.Container, len(containers))
	for i, c := range containers {
		result[i] = c
		predicted, ok := pred.Containers[c.Name]
		if !ok {
			continue
		}
		merged := make(corev1.ResourceList, len(c.Resources.Requests)+len(predicted))
		maps.Copy(merged, c.Resources.Requests)
		maps.Copy(merged, predicted)
		result[i].Resources = corev1.ResourceRequirements{Requests: merged, Limits: c.Resources.Limits}
	}
	return result
}
