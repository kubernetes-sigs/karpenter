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

package node_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
)

// The disruption loop constructs a Candidate for every node in the cluster on every pass, and each Candidate
// construction lists that node's pods. These benchmarks measure that per-pass pod read at cluster scale, across the
// two dimensions that change its cost:
//
//   - deepCopy: whether the read pays the informer cache's per-pod DeepCopy (client.UnsafeDisableDeepCopy clears it).
//   - managedFields: whether the cached Pods still carry managedFields (cache.TransformStripManagedFields drops them).
//
// The reader below reproduces sigs.k8s.io/controller-runtime/pkg/cache/internal.(*CacheReader).List: index lookup,
// per-object DeepCopyObject unless UnsafeDisableDeepCopy is set, then apimeta.SetList into the typed list. The
// controller-runtime fake client always deep-copies and ignores UnsafeDisableDeepCopy, so it cannot measure this.

const (
	benchPodsPerNode   = 8  // measured mean on a 1282-node EKS cluster
	benchPodsPerNodeHi = 20 // a denser node, near the default 110-pod kubelet limit after ENI limits
)

// cacheReaderStub is a client.Client whose List mirrors CacheReader.List's copy semantics over a client-go Indexer.
// Only List is implemented; the embedded nil client.Client panics on anything else, which GetPodsForNode never calls.
type cacheReaderStub struct {
	client.Client
	indexer toolscache.Indexer
}

func newCacheReaderStub(pods []*corev1.Pod) *cacheReaderStub {
	indexer := toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{
		"field:spec.nodeName": func(obj any) ([]string, error) {
			return []string{obj.(*corev1.Pod).Spec.NodeName}, nil
		},
	})
	for _, p := range pods {
		if err := indexer.Add(p); err != nil {
			panic(err)
		}
	}
	return &cacheReaderStub{indexer: indexer}
}

func (c *cacheReaderStub) List(_ context.Context, out client.ObjectList, opts ...client.ListOption) error {
	listOpts := client.ListOptions{}
	listOpts.ApplyOptions(opts)
	nodeName, _ := listOpts.FieldSelector.RequiresExactMatch("spec.nodeName")
	objs, err := c.indexer.ByIndex("field:spec.nodeName", nodeName)
	if err != nil {
		return err
	}
	disableDeepCopy := listOpts.UnsafeDisableDeepCopy != nil && *listOpts.UnsafeDisableDeepCopy
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, item := range objs {
		obj := item.(runtime.Object)
		if !disableDeepCopy {
			obj = obj.DeepCopyObject()
		}
		runtimeObjs = append(runtimeObjs, obj)
	}
	return apimeta.SetList(out, runtimeObjs)
}

func BenchmarkClusterPodRead(b *testing.B) {
	for _, nodes := range []int{4000} {
		for _, podsPerNode := range []int{benchPodsPerNode, benchPodsPerNodeHi} {
			for _, managedFields := range []bool{true, false} {
				for _, deepCopy := range []bool{true, false} {
					name := fmt.Sprintf("nodes=%d/podsPerNode=%d/managedFields=%t/deepCopy=%t",
						nodes, podsPerNode, managedFields, deepCopy)
					b.Run(name, func(b *testing.B) {
						nodeNames, pods := benchCluster(nodes, podsPerNode, managedFields)
						kubeClient := newCacheReaderStub(pods)
						var opts []client.ListOption
						if !deepCopy {
							opts = append(opts, client.UnsafeDisableDeepCopy)
						}
						ctx := context.Background()
						b.ReportAllocs()
						b.ResetTimer()
						for range b.N {
							// One disruption pass: read every node's pods, run the read-only predicates, and keep
							// only the reschedulable ones - which NewCandidate deep-copies on the way onto the
							// Candidate, since they reach the scheduler.
							for _, nodeName := range nodeNames {
								nodePods, err := nodeutils.GetPodsForNode(ctx, kubeClient, nodeName, opts...)
								if err != nil {
									b.Fatal(err)
								}
								for _, p := range nodePods {
									if !podutils.IsReschedulable(p) {
										continue
									}
									if !deepCopy {
										_ = p.DeepCopy()
									}
								}
							}
						}
					})
				}
			}
		}
	}
}

func BenchmarkPodDeepCopy(b *testing.B) {
	for _, managedFields := range []bool{true, false} {
		b.Run(fmt.Sprintf("managedFields=%t", managedFields), func(b *testing.B) {
			p := benchPod("node-0", 0, true, managedFields)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = p.DeepCopy()
			}
		})
	}
}

func benchCluster(nodes, podsPerNode int, managedFields bool) ([]string, []*corev1.Pod) {
	nodeNames := make([]string, 0, nodes)
	pods := make([]*corev1.Pod, 0, nodes*podsPerNode)
	for n := range nodes {
		nodeName := fmt.Sprintf("i-%016x.ec2.internal", n)
		nodeNames = append(nodeNames, nodeName)
		for i := range podsPerNode {
			// Matching the measured 1282-node cluster: all but one pod per node is DaemonSet-owned, so almost every
			// pod is dropped by IsReschedulable after being copied.
			pods = append(pods, benchPod(nodeName, i, i != 0, managedFields))
		}
	}
	return nodeNames, pods
}

// benchPod builds a Pod shaped like the ones measured on a 1282-node EKS cluster. Per-pod element counts and
// serialized sizes from that sample, which this fixture reproduces: managedFields 2 entries / 4283 B,
// spec.containers 1.4 / 2178 B with 6.3 env and 5.6 volumeMounts each, status.containerStatuses 1552 B,
// spec.volumes 6.1 / 775 B, status.conditions 649 B, spec.tolerations 6.4 / 521 B, spec.initContainers 0.3 / 330 B,
// spec.affinity 213 B, labels 5.1 / 174 B, ownerReferences 160 B, annotations 1.3 / 85 B.
func benchPod(nodeName string, index int, ownedByDaemonSet, managedFields bool) *corev1.Pod {
	name := fmt.Sprintf("workload-%d-%s", index, nodeName)
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kube-system",
			UID:       types.UID(fmt.Sprintf("8f0a1b2c-3d4e-5f60-7182-%012d", index)),
			Labels: map[string]string{
				"app.kubernetes.io/name":     fmt.Sprintf("workload-%d", index),
				"app.kubernetes.io/instance": "release",
				"controller-revision-hash":   "5d7f9c4b86",
				"pod-template-generation":    "7",
				"k8s-app":                    fmt.Sprintf("workload-%d", index),
			},
			Annotations: map[string]string{
				"kubectl.kubernetes.io/restartedAt": "2026-01-01T00:00:00Z",
				"prometheus.io/port":                "9090",
				"prometheus.io/scrape":              "true",
			},
			ResourceVersion: "1234567890",
		},
		Spec: corev1.PodSpec{
			NodeName:           nodeName,
			ServiceAccountName: "workload",
			Tolerations: []corev1.Toleration{
				{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
				{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
				{Key: "node.kubernetes.io/disk-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
				{Key: "node.kubernetes.io/memory-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
				{Key: "node.kubernetes.io/pid-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
				{Key: "node.kubernetes.io/unschedulable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			},
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
						{Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64", "arm64"}},
					},
				}}},
			}},
			InitContainers: []corev1.Container{{
				Name:      "init",
				Image:     "123456789012.dkr.ecr.us-east-1.amazonaws.com/workload-init:1.2.3",
				Command:   []string{"/bin/sh", "-c", "install -m 0755 /bin/agent /host/opt/bin/agent"},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "hostproc", MountPath: "/host/opt/bin"},
				},
			}},
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "123456789012.dkr.ecr.us-east-1.amazonaws.com/workload:1.2.3",
				Args:  []string{"--config=/etc/workload/config.yaml", "--log-level=info", "--metrics-addr=:9090"},
				Env: []corev1.EnvVar{
					{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
					{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
					{Name: "AWS_REGION", Value: "us-east-1"},
					{Name: "AWS_STS_REGIONAL_ENDPOINTS", Value: "regional"},
					{Name: "AWS_ROLE_ARN", Value: "arn:aws:iam::123456789012:role/workload-irsa-role"},
					{Name: "AWS_WEB_IDENTITY_TOKEN_FILE", Value: "/var/run/secrets/eks.amazonaws.com/serviceaccount/token"},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "config", MountPath: "/etc/workload"},
					{Name: "secrets", MountPath: "/etc/workload/secrets", ReadOnly: true},
					{Name: "hostproc", MountPath: "/host/proc", ReadOnly: true},
					{Name: "hostsys", MountPath: "/host/sys", ReadOnly: true},
					{Name: "varlog", MountPath: "/var/log", ReadOnly: true},
					{Name: "kube-api-access", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
				},
			}, {
				Name:  "sidecar",
				Image: "123456789012.dkr.ecr.us-east-1.amazonaws.com/config-reloader:0.9.1",
				Args:  []string{"--watched-dir=/etc/workload", "--reload-url=http://127.0.0.1:9090/-/reload"},
				Env: []corev1.EnvVar{
					{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
					{Name: "SHARD", Value: "0"},
				},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("32Mi"),
				}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "config", MountPath: "/etc/workload", ReadOnly: true},
					{Name: "kube-api-access", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "workload-config"}}}},
				{Name: "secrets", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "workload-secrets"}}},
				{Name: "hostproc", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/proc"}}},
				{Name: "hostsys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys"}}},
				{Name: "varlog", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"}}},
				{Name: "kube-api-access", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: new(int64(3607))}},
					{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
					{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}}},
				}}}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastProbeTime: benchTime, LastTransitionTime: benchTime},
				{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionTrue, LastProbeTime: benchTime, LastTransitionTime: benchTime},
				{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastProbeTime: benchTime, LastTransitionTime: benchTime},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastProbeTime: benchTime, LastTransitionTime: benchTime},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastProbeTime: benchTime, LastTransitionTime: benchTime},
			},
			StartTime: &metav1.Time{},
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:        "init",
				Image:       "123456789012.dkr.ecr.us-east-1.amazonaws.com/workload-init:1.2.3",
				ImageID:     "123456789012.dkr.ecr.us-east-1.amazonaws.com/workload-init@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
				ContainerID: "containerd://fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
				Started:     new(false),
				State:       corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{
				benchContainerStatus("main", "workload:1.2.3"),
				benchContainerStatus("sidecar", "config-reloader:0.9.1"),
			},
		},
	}
	if ownedByDaemonSet {
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Kind:       "DaemonSet",
			Name:       fmt.Sprintf("workload-%d", index),
			UID:        types.UID(fmt.Sprintf("1a2b3c4d-5e6f-7081-92a3-%012d", index)),
			Controller: new(true),
		}}
	}
	if managedFields {
		p.ManagedFields = []metav1.ManagedFieldsEntry{
			{Manager: "kube-controller-manager", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: benchFieldsV1Raw(2100)}},
			{Manager: "kubelet", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "v1", FieldsType: "FieldsV1", Subresource: "status", FieldsV1: &metav1.FieldsV1{Raw: benchFieldsV1Raw(2100)}},
		}
	}
	return p
}

// benchFieldsV1Raw produces a FieldsV1 payload of roughly the requested size, matching the ~2.1 KB per entry measured
// on real pods. FieldsV1.Raw is a []byte, so its DeepCopy cost is one allocation plus a copy of this length.
func benchFieldsV1Raw(size int) []byte {
	raw := make([]byte, 0, size+64)
	raw = append(raw, `{"f:metadata":{"f:annotations":{".":{}`...)
	for i := 0; len(raw) < size; i++ {
		raw = append(raw, fmt.Sprintf(`,"f:field-%03d":{}`, i)...)
	}
	raw = append(raw, `}}}`...)
	return raw
}

// benchTime is a fixed non-zero timestamp so conditions and statuses serialize like real ones.
var benchTime = metav1.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func benchContainerStatus(name, image string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:         name,
		Image:        "123456789012.dkr.ecr.us-east-1.amazonaws.com/" + image,
		ImageID:      "123456789012.dkr.ecr.us-east-1.amazonaws.com/" + image + "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ContainerID:  "containerd://0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Ready:        true,
		Started:      new(true),
		RestartCount: 1,
		State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: benchTime}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, Reason: "OOMKilled", StartedAt: benchTime, FinishedAt: benchTime,
			ContainerID: "containerd://fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		}},
		AllocatedResources: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		}},
	}
}
