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

package disruption_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

// podsPerCopyNode is the number of bound pods each benchmark Node carries, so the per-Node resource maps cluster state
// keeps (podRequests, podLimits, podDisruptionCosts) and its host port and volume usage are populated as they are on a
// busy Node.
const podsPerCopyNode = 20

// copyBenchmarkManagedFields returns the managedFields entries a Node or NodeClaim carries in a cluster where the
// kubelet, the node lifecycle controller and Karpenter have all written to the object. The raw FieldsV1 payloads are
// sized like the real ones, since their byte slices are what the deep copy spends its time on.
func copyBenchmarkManagedFields() []metav1.ManagedFieldsEntry {
	entry := func(manager, operation, subresource string, size int) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{
			Manager:     manager,
			Operation:   metav1.ManagedFieldsOperationType(operation),
			APIVersion:  "v1",
			Time:        &metav1.Time{Time: time.Now()},
			FieldsType:  "FieldsV1",
			FieldsV1:    &metav1.FieldsV1{Raw: []byte(fmt.Sprintf(`{"f:metadata":{"f:labels":{"%s":{}}}}`, strings.Repeat("f", size)))},
			Subresource: subresource,
		}
	}
	return []metav1.ManagedFieldsEntry{
		entry("kubelet", "Update", "status", 1500),
		entry("kube-controller-manager", "Update", "", 200),
		entry("karpenter", "Update", "", 400),
	}
}

// copyBenchmarkObjectMeta returns the metadata a managed Node or NodeClaim carries: the well-known Karpenter labels, a
// handful of provider and workload labels, annotations, a finalizer and the managedFields entries above.
func copyBenchmarkObjectMeta(name string) metav1.ObjectMeta {
	meta := metav1.ObjectMeta{
		Name:              name,
		CreationTimestamp: metav1.NewTime(time.Now().Add(-24 * time.Hour)),
		Labels: map[string]string{
			v1.NodePoolLabelKey:            "default",
			v1.NodeInitializedLabelKey:     "true",
			v1.NodeRegisteredLabelKey:      "true",
			v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
			corev1.LabelInstanceTypeStable: "default-instance-type",
			corev1.LabelTopologyZone:       "test-zone-1a",
			corev1.LabelTopologyRegion:     "test-region",
			corev1.LabelOSStable:           "linux",
			corev1.LabelArchStable:         "amd64",
			corev1.LabelHostname:           name,
		},
		Annotations: map[string]string{
			v1.NodeClaimTerminationTimestampAnnotationKey:            time.Now().Format(time.RFC3339),
			"node.alpha.kubernetes.io/ttl":                           "0",
			"volumes.kubernetes.io/controller-managed-attach-detach": "true",
		},
		Finalizers:      []string{v1.TerminationFinalizer},
		ManagedFields:   copyBenchmarkManagedFields(),
		ResourceVersion: "12345",
		UID:             types.UID(name),
	}
	return meta
}

func copyBenchmarkNodeClaim(name string) *v1.NodeClaim {
	nodeClaim := &v1.NodeClaim{
		ObjectMeta: copyBenchmarkObjectMeta(name),
		Spec: v1.NodeClaimSpec{
			Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}},
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-zone-1a", "test-zone-1b", "test-zone-1c"}},
			},
			NodeClassRef: &v1.NodeClassReference{Group: "karpenter.test.sh", Kind: "TestNodeClass", Name: "default"},
		},
		Status: v1.NodeClaimStatus{
			ProviderID:  "fake:///" + name,
			NodeName:    name,
			Capacity:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi"), corev1.ResourcePods: resource.MustParse("110")},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3920m"), corev1.ResourceMemory: resource.MustParse("14Gi"), corev1.ResourcePods: resource.MustParse("110")},
		},
	}
	nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
	nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeRegistered)
	nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
	nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
	nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
	return nodeClaim
}

func copyBenchmarkNode(name string) *corev1.Node {
	condition := func(conditionType corev1.NodeConditionType, status corev1.ConditionStatus, reason string) corev1.NodeCondition {
		return corev1.NodeCondition{
			Type: conditionType, Status: status, Reason: reason,
			Message:            "kubelet is posting ready status",
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.NewTime(time.Now().Add(-24 * time.Hour)),
		}
	}
	return &corev1.Node{
		ObjectMeta: copyBenchmarkObjectMeta(name),
		Spec:       corev1.NodeSpec{ProviderID: "fake:///" + name},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi"), corev1.ResourcePods: resource.MustParse("110")},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3920m"), corev1.ResourceMemory: resource.MustParse("14Gi"), corev1.ResourcePods: resource.MustParse("110")},
			Conditions: []corev1.NodeCondition{
				condition(corev1.NodeReady, corev1.ConditionTrue, "KubeletReady"),
				condition(corev1.NodeMemoryPressure, corev1.ConditionFalse, "KubeletHasSufficientMemory"),
				condition(corev1.NodeDiskPressure, corev1.ConditionFalse, "KubeletHasNoDiskPressure"),
				condition(corev1.NodePIDPressure, corev1.ConditionFalse, "KubeletHasSufficientPID"),
			},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: corev1.NodeHostName, Address: name},
			},
			NodeInfo: corev1.NodeSystemInfo{
				MachineID: "ec2abcdef0123456789", SystemUUID: "ec2abcdef-0123-4567-89ab-cdef01234567",
				KernelVersion: "6.1.0", OSImage: "Amazon Linux 2023", ContainerRuntimeVersion: "containerd://1.7.0",
				KubeletVersion: "v1.33.0", KubeProxyVersion: "v1.33.0", OperatingSystem: "linux", Architecture: "amd64",
			},
		},
	}
}

func copyBenchmarkPod(nodeName string, i int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-pod-%d", nodeName, i), Namespace: "default",
			UID: types.UID(fmt.Sprintf("%s-pod-%d", nodeName, i)),
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "image",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// copyBenchmarkContext carries the options cluster state reads while tracking pods.
func copyBenchmarkContext() context.Context {
	return options.ToContext(context.Background(), test.Options())
}

// copyBenchmarkCluster returns cluster state holding n initialized, managed Nodes with their NodeClaims and
// podsPerCopyNode bound pods each.
func copyBenchmarkCluster(ctx context.Context, tb testing.TB, n int) *state.Cluster {
	tb.Helper()
	kubeClient := fakecr.NewClientBuilder().WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*corev1.Pod).Spec.NodeName}
	}).Build()
	clusterState := state.NewCluster(clock.NewFakeClock(time.Now()), kubeClient, fake.NewCloudProvider())
	for i := range n {
		name := fmt.Sprintf("node-%d", i)
		clusterState.UpdateNodeClaim(copyBenchmarkNodeClaim(name))
		if err := clusterState.UpdateNode(ctx, copyBenchmarkNode(name)); err != nil {
			tb.Fatal(err)
		}
		for j := range podsPerCopyNode {
			if err := clusterState.UpdatePod(ctx, copyBenchmarkPod(name, j)); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return clusterState
}

var (
	benchmarkStateNodes state.StateNodes
	benchmarkNumNodes   map[string]int
)

// BenchmarkDeepCopyNodes measures one full copy of cluster state, which the disruption controller takes at least four
// times per round: once to clear stale taints, then once per GetCandidates and once per disruption budget mapping for
// every disruption method it evaluates, plus once per scheduling simulation.
func BenchmarkDeepCopyNodes(b *testing.B) {
	ctx := copyBenchmarkContext()
	for _, n := range []int{1000, 4000} {
		b.Run(fmt.Sprintf("Nodes=%d", n), func(b *testing.B) {
			clusterState := copyBenchmarkCluster(ctx, b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				benchmarkStateNodes = clusterState.DeepCopyNodes()
			}
		})
	}
}

// BenchmarkNodePoolStats measures the per-NodePool node and disrupting counts the disruption controller recomputes once
// per disruption method per round, which only reads each state node.
func BenchmarkNodePoolStats(b *testing.B) {
	ctx := copyBenchmarkContext()
	for _, n := range []int{1000, 4000} {
		b.Run(fmt.Sprintf("Nodes=%d", n), func(b *testing.B) {
			clusterState := copyBenchmarkCluster(ctx, b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				benchmarkNumNodes, _ = disruption.NodePoolStats(clusterState, v1.DisruptionReasonDrifted)
			}
		})
	}
}
