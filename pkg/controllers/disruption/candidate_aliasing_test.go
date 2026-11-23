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

// NewCandidate lists a node's pods with client.UnsafeDisableDeepCopy, so those pods alias the informer cache and a
// write through any of them corrupts the cache for every controller in the process. The specs here pin the two halves
// of that contract: the read asks for no copy, and nothing the Candidate keeps or that the read path touches aliases
// the cache afterwards.

package disruption

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/pdb"
)

// aliasingPodClient is a client.Client whose List mirrors CacheReader.List under UnsafeDisableDeepCopy: it
// shallow-copies the objects it holds into the typed list, so every map, slice and pointer in a returned Pod is shared
// with what this client keeps. envtest and the controller-runtime fake client both decode or deep-copy per List, so
// neither can observe a write back through such a Pod.
type aliasingPodClient struct {
	client.Client

	pods         []*corev1.Pod
	sawListOpts  []client.ListOptions
	deepCopyList bool
}

func (c *aliasingPodClient) List(_ context.Context, out client.ObjectList, opts ...client.ListOption) error {
	listOpts := client.ListOptions{}
	listOpts.ApplyOptions(opts)
	c.sawListOpts = append(c.sawListOpts, listOpts)

	podList, ok := out.(*corev1.PodList)
	if !ok {
		return nil
	}
	runtimeObjs := make([]runtime.Object, 0, len(c.pods))
	for _, p := range c.pods {
		if c.deepCopyList {
			runtimeObjs = append(runtimeObjs, p.DeepCopyObject())
			continue
		}
		runtimeObjs = append(runtimeObjs, p)
	}
	return apimeta.SetList(podList, runtimeObjs)
}

var _ = Describe("Candidate pod aliasing", func() {
	var ctx context.Context
	var clk *clock.FakeClock
	var kubeClient *aliasingPodClient
	var stateNode *state.StateNode
	var nodePool *v1.NodePool
	var nodePoolMap map[string]*v1.NodePool
	var instanceTypesMap map[string]map[string]*cloudprovider.InstanceType
	var reschedulablePod, daemonSetPod *corev1.Pod

	BeforeEach(func() {
		ctx = options.ToContext(context.Background(), &options.Options{})
		clk = clock.NewFakeClock(metav1.Now().Time)
		nodePool = &v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
		nodePoolMap = map[string]*v1.NodePool{nodePool.Name: nodePool}
		instanceTypesMap = map[string]map[string]*cloudprovider.InstanceType{
			nodePool.Name: {"default-instance-type": {Name: "default-instance-type"}},
		}

		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				v1.NodeRegisteredLabelKey:      "true",
				v1.NodeInitializedLabelKey:     "true",
				corev1.LabelInstanceTypeStable: "default-instance-type",
				corev1.LabelTopologyZone:       "test-zone-1",
				v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
			},
		}, Spec: corev1.NodeSpec{ProviderID: "fake://node-1"}}
		nodeClaim := &v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "nodeclaim-1"},
			Status:     v1.NodeClaimStatus{ProviderID: "fake://node-1"},
		}
		stateNode = &state.StateNode{Node: node, NodeClaim: nodeClaim}

		// Every pod field a shallow struct copy leaves shared with the cache: a label map, an annotation map, an
		// owner-reference slice, a container slice, a pointer-valued Spec.Affinity, and a topology-spread slice.
		reschedulablePod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "reschedulable",
				Namespace: "default",
				Labels:    map[string]string{"app": "workload"},
				Annotations: map[string]string{
					"prometheus.io/scrape": "true",
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: appsv1.SchemeGroupVersion.String(),
					Kind:       "ReplicaSet",
					Name:       "rs",
					UID:        "11111111-1111-1111-1111-111111111111",
					Controller: lo.ToPtr(true),
				}},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
				Containers: []corev1.Container{{
					Name:  "main",
					Image: "workload:1.0.0",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					}},
				}},
				Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
						Weight: 1,
						Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-zone-3"},
						}}},
					}},
				}},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "workload"}},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		daemonSetPod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "daemon",
				Namespace: "kube-system",
				Labels:    map[string]string{"k8s-app": "agent"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: appsv1.SchemeGroupVersion.String(),
					Kind:       "DaemonSet",
					Name:       "ds",
					UID:        "22222222-2222-2222-2222-222222222222",
					Controller: lo.ToPtr(true),
				}},
			},
			Spec: corev1.PodSpec{
				NodeName:   "node-1",
				Containers: []corev1.Container{{Name: "agent", Image: "agent:1.0.0"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		kubeClient = &aliasingPodClient{pods: []*corev1.Pod{reschedulablePod, daemonSetPod}}
	})

	newCandidate := func() *Candidate {
		c, err := NewCandidate(ctx, kubeClient, events.NewRecorder(&noopEventRecorder{}), clk, stateNode, pdb.Limits{},
			nodePoolMap, instanceTypesMap, &Queue{}, GracefulDisruptionClass)
		Expect(err).ToNot(HaveOccurred())
		return c
	}

	It("asks the cache not to deep-copy the pods it only reads", func() {
		newCandidate()
		Expect(kubeClient.sawListOpts).ToNot(BeEmpty())
		Expect(lo.FromPtr(kubeClient.sawListOpts[0].UnsafeDisableDeepCopy)).To(BeTrue())
	})

	It("doesn't mutate the aliased pods", func() {
		before := []*corev1.Pod{reschedulablePod.DeepCopy(), daemonSetPod.DeepCopy()}
		newCandidate()
		Expect(reschedulablePod).To(Equal(before[0]))
		Expect(daemonSetPod).To(Equal(before[1]))
	})

	It("stores reschedulable pods that don't alias the cache", func() {
		candidate := newCandidate()
		Expect(candidate.reschedulablePods).To(HaveLen(1))
		stored := candidate.reschedulablePods[0]
		Expect(stored.Name).To(Equal(reschedulablePod.Name))
		Expect(stored).ToNot(BeIdenticalTo(reschedulablePod))

		// Each of these is a write the scheduler is known to make: Inject replaces Spec.TopologySpreadConstraints and
		// Relax rewrites Spec.Affinity's nested terms. None may reach the aliased pod.
		before := reschedulablePod.DeepCopy()
		stored.Labels["mutated"] = "true"
		stored.Annotations["mutated"] = "true"
		stored.Spec.TopologySpreadConstraints = nil
		stored.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = nil
		stored.Spec.Containers[0].Image = "mutated:1.0.0"
		stored.OwnerReferences[0].Name = "mutated"
		Expect(reschedulablePod).To(Equal(before))
	})

	It("computes the same candidate whether or not the read deep-copies", func() {
		noCopy := newCandidate()
		kubeClient.deepCopyList = true
		deepCopied := newCandidate()
		Expect(noCopy.DisruptionCost).To(Equal(deepCopied.DisruptionCost))
		Expect(noCopy.RescheduleDisruptionCost).To(Equal(deepCopied.RescheduleDisruptionCost))
		Expect(noCopy.reschedulablePods).To(Equal(deepCopied.reschedulablePods))
	})
})

// noopEventRecorder is a no-op record.EventRecorder; NewCandidate only publishes events on paths these specs
// don't take.
type noopEventRecorder struct{}

func (r *noopEventRecorder) Event(runtime.Object, string, string, string)          {}
func (r *noopEventRecorder) Eventf(runtime.Object, string, string, string, ...any) {}
func (r *noopEventRecorder) AnnotatedEventf(runtime.Object, map[string]string, string, string, string, ...any) {
}
