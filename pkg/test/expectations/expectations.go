/*
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

//nolint:revive
package expectations

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,stylecheck
	. "github.com/onsi/gomega"    //nolint:revive,stylecheck
	prometheus "github.com/prometheus/client_model/go"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/machine/lifecycle"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
)

const (
	ReconcilerPropagationTime = 10 * time.Second
	RequestInterval           = 1 * time.Second
)

type Bindings map[*v1.Pod]*Binding

type Binding struct {
	Machine *v1alpha5.Machine
	Node    *v1.Node
}

func (b Bindings) Get(p *v1.Pod) *Binding {
	for k, v := range b {
		if client.ObjectKeyFromObject(k) == client.ObjectKeyFromObject(p) {
			return v
		}
	}
	return nil
}

func ExpectExists[T client.Object](ctx context.Context, c client.Client, obj T) T {
	return ExpectExistsWithOffset(1, ctx, c, obj)
}

func ExpectExistsWithOffset[T client.Object](offset int, ctx context.Context, c client.Client, obj T) T {
	resp := reflect.New(reflect.TypeOf(*new(T)).Elem()).Interface().(T)
	ExpectWithOffset(offset+1, c.Get(ctx, client.ObjectKeyFromObject(obj), resp)).To(Succeed())
	return resp
}

func ExpectPodExists(ctx context.Context, c client.Client, name string, namespace string) *v1.Pod {
	return ExpectPodExistsWithOffset(1, ctx, c, name, namespace)
}

func ExpectPodExistsWithOffset(offset int, ctx context.Context, c client.Client, name string, namespace string) *v1.Pod {
	return ExpectExistsWithOffset(offset+1, ctx, c, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
}

func ExpectNodeExists(ctx context.Context, c client.Client, name string) *v1.Node {
	return ExpectNodeExistsWithOffset(1, ctx, c, name)
}

func ExpectNodeExistsWithOffset(offset int, ctx context.Context, c client.Client, name string) *v1.Node {
	return ExpectExistsWithOffset(offset+1, ctx, c, &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
}

func ExpectNotFound(ctx context.Context, c client.Client, objects ...client.Object) {
	ExpectNotFoundWithOffset(1, ctx, c, objects...)
}

func ExpectNotFoundWithOffset(offset int, ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		EventuallyWithOffset(offset+1, func() bool {
			return errors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: object.GetName(), Namespace: object.GetNamespace()}, object))
		}, ReconcilerPropagationTime, RequestInterval).Should(BeTrue(), func() string {
			return fmt.Sprintf("expected %s/%s to be deleted, but it still exists", lo.Must(apiutil.GVKForObject(object, scheme.Scheme)), client.ObjectKeyFromObject(object))
		})
	}
}

func ExpectScheduled(ctx context.Context, c client.Client, pod *v1.Pod) *v1.Node {
	p := ExpectPodExistsWithOffset(1, ctx, c, pod.Name, pod.Namespace)
	ExpectWithOffset(1, p.Spec.NodeName).ToNot(BeEmpty(), fmt.Sprintf("expected %s/%s to be scheduled", pod.Namespace, pod.Name))
	return ExpectNodeExistsWithOffset(1, ctx, c, p.Spec.NodeName)
}

func ExpectNotScheduled(ctx context.Context, c client.Client, pod *v1.Pod) *v1.Pod {
	p := ExpectPodExistsWithOffset(1, ctx, c, pod.Name, pod.Namespace)
	EventuallyWithOffset(1, p.Spec.NodeName).Should(BeEmpty(), fmt.Sprintf("expected %s/%s to not be scheduled", pod.Namespace, pod.Name))
	return p
}

func ExpectApplied(ctx context.Context, c client.Client, objects ...client.Object) {
	ExpectAppliedWithOffset(1, ctx, c, objects...)
}

func ExpectAppliedWithOffset(offset int, ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		deletionTimestampSet := !object.GetDeletionTimestamp().IsZero()
		current := object.DeepCopyObject().(client.Object)
		statuscopy := object.DeepCopyObject().(client.Object) // Snapshot the status, since create/update may override

		// Create or Update
		if err := c.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
			if errors.IsNotFound(err) {
				ExpectWithOffset(offset+1, c.Create(ctx, object)).To(Succeed())
			} else {
				ExpectWithOffset(offset+1, err).ToNot(HaveOccurred())
			}
		} else {
			object.SetResourceVersion(current.GetResourceVersion())
			ExpectWithOffset(offset+1, c.Update(ctx, object)).To(Succeed())
		}
		// Update status
		statuscopy.SetResourceVersion(object.GetResourceVersion())
		ExpectWithOffset(offset+1, c.Status().Update(ctx, statuscopy)).To(Or(Succeed(), MatchError("the server could not find the requested resource"))) // Some objects do not have a status

		// Re-get the object to grab the updated spec and status
		Expect(c.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())

		// Set the deletion timestamp by adding a finalizer and deleting
		if deletionTimestampSet {
			ExpectDeletionTimestampSetWithOffset(1, ctx, c, object)
		}
	}
}

func ExpectDeleted(ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		if err := c.Delete(ctx, object, &client.DeleteOptions{GracePeriodSeconds: ptr.Int64(0)}); !errors.IsNotFound(err) {
			ExpectWithOffset(1, err).To(BeNil())
		}
		ExpectNotFoundWithOffset(1, ctx, c, object)
	}
}

func ExpectDeletionTimestampSet(ctx context.Context, c client.Client, objects ...client.Object) {
	ExpectDeletionTimestampSetWithOffset(1, ctx, c, objects...)
}

// ExpectDeletionTimestampSetWithOffset ensures that the deletion timestamp is set on the objects by adding a finalizer
// and then deleting the object immediately after. This holds the object until the finalizer is patched out in the DeferCleanup
func ExpectDeletionTimestampSetWithOffset(offset int, ctx context.Context, c client.Client, objects ...client.Object) {
	for _, object := range objects {
		ExpectWithOffset(offset+1, c.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
		controllerutil.AddFinalizer(object, v1alpha5.TestingGroup+"/finalizer")
		ExpectWithOffset(offset+1, c.Update(ctx, object)).To(Succeed())
		ExpectWithOffset(offset+1, c.Delete(ctx, object)).To(Succeed())
		DeferCleanup(func(obj client.Object) {
			mergeFrom := client.MergeFrom(obj.DeepCopyObject().(client.Object))
			obj.SetFinalizers([]string{})
			ExpectWithOffset(offset+1, c.Patch(ctx, obj, mergeFrom)).To(Succeed())
		}, object)
	}
}

func ExpectCleanedUp(ctx context.Context, c client.Client) {
	wg := sync.WaitGroup{}
	namespaces := &v1.NamespaceList{}
	ExpectWithOffset(1, c.List(ctx, namespaces)).To(Succeed())
	ExpectFinalizersRemovedFromList(ctx, c, &v1.NodeList{}, &v1alpha5.MachineList{}, &v1.PersistentVolumeClaimList{})
	for _, object := range []client.Object{
		&v1.Pod{},
		&v1.Node{},
		&appsv1.DaemonSet{},
		&policyv1.PodDisruptionBudget{},
		&v1.PersistentVolumeClaim{},
		&v1.PersistentVolume{},
		&storagev1.StorageClass{},
		&v1alpha5.Provisioner{},
		&v1alpha5.Machine{},
	} {
		for _, namespace := range namespaces.Items {
			wg.Add(1)
			go func(object client.Object, namespace string) {
				defer wg.Done()
				defer GinkgoRecover()
				ExpectWithOffset(1, c.DeleteAllOf(ctx, object, client.InNamespace(namespace),
					&client.DeleteAllOfOptions{DeleteOptions: client.DeleteOptions{GracePeriodSeconds: ptr.Int64(0)}})).ToNot(HaveOccurred())
			}(object, namespace.Name)
		}
	}
	wg.Wait()
}

func ExpectFinalizersRemovedFromList(ctx context.Context, c client.Client, objectLists ...client.ObjectList) {
	for _, list := range objectLists {
		ExpectWithOffset(1, c.List(ctx, list)).To(Succeed())
		ExpectWithOffset(1, meta.EachListItem(list, func(o runtime.Object) error {
			obj := o.(client.Object)
			stored := obj.DeepCopyObject().(client.Object)
			obj.SetFinalizers([]string{})
			Expect(client.IgnoreNotFound(c.Patch(ctx, obj, client.MergeFrom(stored)))).To(Succeed())
			return nil
		})).To(Succeed())
	}
}

func ExpectFinalizersRemoved(ctx context.Context, c client.Client, objs ...client.Object) {
	for _, obj := range objs {
		ExpectWithOffset(1, client.IgnoreNotFound(c.Get(ctx, client.ObjectKeyFromObject(obj), obj))).To(Succeed())
		stored := obj.DeepCopyObject().(client.Object)
		obj.SetFinalizers([]string{})
		ExpectWithOffset(1, client.IgnoreNotFound(c.Patch(ctx, obj, client.MergeFrom(stored)))).To(Succeed())
	}
}

func ExpectProvisioned(ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, provisioner *provisioning.Provisioner, pods ...*v1.Pod) Bindings {
	bindings := ExpectProvisionedNoBindingWithOffset(1, ctx, c, cluster, cloudProvider, provisioner, pods...)
	podKeys := sets.NewString(lo.Map(pods, func(p *v1.Pod, _ int) string { return client.ObjectKeyFromObject(p).String() })...)
	for pod, binding := range bindings {
		// Only bind the pods that are passed through
		if podKeys.Has(client.ObjectKeyFromObject(pod).String()) {
			ExpectManualBindingWithOffset(1, ctx, c, pod, binding.Node)
			ExpectWithOffset(1, cluster.UpdatePod(ctx, pod)).To(Succeed()) // track pod bindings
		}
	}
	return bindings
}

func ExpectProvisionedNoBinding(ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, provisioner *provisioning.Provisioner, pods ...*v1.Pod) Bindings {
	return ExpectProvisionedNoBindingWithOffset(1, ctx, c, cluster, cloudProvider, provisioner, pods...)
}

func ExpectProvisionedNoBindingWithOffset(offset int, ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, provisioner *provisioning.Provisioner, pods ...*v1.Pod) Bindings {
	// Persist objects
	for _, pod := range pods {
		ExpectAppliedWithOffset(offset+1, ctx, c, pod)
	}
	// TODO: Check the error on the provisioner scheduling round
	results, err := provisioner.Schedule(ctx)
	bindings := Bindings{}
	if err != nil {
		log.Printf("error provisioning in test, %s", err)
		return bindings
	}
	for _, m := range results.NewMachines {
		// TODO: Check the error on the provisioner launch
		name, err := provisioner.Launch(ctx, m, provisioning.WithReason(metrics.ProvisioningReason))
		if err != nil {
			return bindings
		}
		machine := &v1alpha5.Machine{}
		ExpectWithOffset(offset+1, c.Get(ctx, types.NamespacedName{Name: name}, machine)).To(Succeed())
		machine, node := ExpectMachineDeployedWithOffset(offset+1, ctx, c, cluster, cloudProvider, machine)
		if machine != nil && node != nil {
			for _, pod := range m.Pods {
				bindings[pod] = &Binding{
					Machine: machine,
					Node:    node,
				}
			}
		}
	}
	for _, node := range results.ExistingNodes {
		for _, pod := range node.Pods {
			bindings[pod] = &Binding{
				Node:    node.Node,
				Machine: node.Machine,
			}
		}
	}
	return bindings
}

func ExpectMachineDeployedNoNode(ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, m *v1alpha5.Machine) *v1alpha5.Machine {
	return ExpectMachineDeployedNoNodeWithOffset(1, ctx, c, cluster, cloudProvider, m)
}

func ExpectMachineDeployedNoNodeWithOffset(offset int, ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, m *v1alpha5.Machine) *v1alpha5.Machine {
	resolved, err := cloudProvider.Create(ctx, m)
	// TODO @joinnis: Check this error rather than swallowing it. This is swallowed right now due to how we are doing some testing in the cloudprovider
	if err != nil {
		return nil
	}
	ExpectWithOffset(offset+1, err).To(Succeed())

	// Make the machine ready in the status conditions
	lifecycle.PopulateMachineDetails(m, resolved)
	m.StatusConditions().MarkTrue(v1alpha5.MachineLaunched)
	ExpectAppliedWithOffset(offset+1, ctx, c, m)
	cluster.UpdateMachine(m)
	return m
}

func ExpectMachineDeployed(ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, machine *v1alpha5.Machine) (*v1alpha5.Machine, *v1.Node) {
	return ExpectMachineDeployedWithOffset(1, ctx, c, cluster, cloudProvider, machine)
}

func ExpectMachineDeployedWithOffset(offset int, ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, m *v1alpha5.Machine) (*v1alpha5.Machine, *v1.Node) {
	m = ExpectMachineDeployedNoNodeWithOffset(offset+1, ctx, c, cluster, cloudProvider, m)
	m.StatusConditions().MarkTrue(v1alpha5.MachineRegistered)

	// Mock the machine launch and node joining at the apiserver
	node := test.MachineLinkedNode(m)
	ExpectAppliedWithOffset(offset+1, ctx, c, m, node)
	ExpectWithOffset(offset+1, cluster.UpdateNode(ctx, node)).To(Succeed())
	cluster.UpdateMachine(m)
	return m, node
}

func ExpectMachinesCascadeDeletion(ctx context.Context, c client.Client, machines ...*v1alpha5.Machine) {
	nodes := ExpectNodesWithOffset(1, ctx, c)
	for _, machine := range machines {
		err := c.Get(ctx, client.ObjectKeyFromObject(machine), &v1alpha5.Machine{})
		if !errors.IsNotFound(err) {
			continue
		}
		for _, node := range nodes {
			if node.Spec.ProviderID == machine.Status.ProviderID {
				Expect(c.Delete(ctx, node))
				ExpectFinalizersRemoved(ctx, c, node)
				ExpectNotFound(ctx, c, node)
			}
		}
	}
}

func ExpectMakeMachinesReady(ctx context.Context, c client.Client, machines ...*v1alpha5.Machine) {
	ExpectMakeMachinesReadyWithOffset(1, ctx, c, machines...)
}

func ExpectMakeMachinesReadyWithOffset(offset int, ctx context.Context, c client.Client, machines ...*v1alpha5.Machine) {
	for i := range machines {
		machines[i] = ExpectExistsWithOffset(offset+1, ctx, c, machines[i])
		machines[i].StatusConditions().MarkTrue(v1alpha5.MachineLaunched)
		machines[i].StatusConditions().MarkTrue(v1alpha5.MachineRegistered)
		machines[i].StatusConditions().MarkTrue(v1alpha5.MachineInitialized)
		ExpectAppliedWithOffset(offset+1, ctx, c, machines[i])
	}
}

func ExpectMakeNodesReady(ctx context.Context, c client.Client, nodes ...*v1.Node) {
	ExpectMakeNodesReadyWithOffset(1, ctx, c, nodes...)
}

func ExpectMakeNodesReadyWithOffset(offset int, ctx context.Context, c client.Client, nodes ...*v1.Node) {
	for i := range nodes {
		nodes[i] = ExpectExistsWithOffset(offset+1, ctx, c, nodes[i])
		nodes[i].Status.Phase = v1.NodeRunning
		nodes[i].Status.Conditions = []v1.NodeCondition{
			{
				Type:               v1.NodeReady,
				Status:             v1.ConditionTrue,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "KubeletReady",
			},
		}
		if nodes[i].Labels == nil {
			nodes[i].Labels = map[string]string{}
		}
		nodes[i].Labels[v1alpha5.LabelNodeInitialized] = "true"
		nodes[i].Spec.Taints = nil
		ExpectAppliedWithOffset(offset+1, ctx, c, nodes[i])
	}
}

func ExpectReconcileSucceeded(ctx context.Context, reconciler reconcile.Reconciler, key client.ObjectKey) reconcile.Result {
	return ExpectReconcileSucceededWithOffset(1, ctx, reconciler, key)
}

func ExpectReconcileSucceededWithOffset(offset int, ctx context.Context, reconciler reconcile.Reconciler, key client.ObjectKey) reconcile.Result {
	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	ExpectWithOffset(offset+1, err).ToNot(HaveOccurred())
	return result
}

func ExpectReconcileFailed(ctx context.Context, reconciler reconcile.Reconciler, key client.ObjectKey) {
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	ExpectWithOffset(1, err).To(HaveOccurred())
}

func ExpectStatusConditionExists(obj apis.ConditionsAccessor, t apis.ConditionType) apis.Condition {
	conds := obj.GetConditions()
	cond, ok := lo.Find(conds, func(c apis.Condition) bool {
		return c.Type == t
	})
	ExpectWithOffset(1, ok).To(BeTrue())
	return cond
}

func ExpectOwnerReferenceExists(obj, owner client.Object) metav1.OwnerReference {
	or, found := lo.Find(obj.GetOwnerReferences(), func(o metav1.OwnerReference) bool {
		return o.UID == owner.GetUID()
	})
	Expect(found).To(BeTrue())
	return or
}

// FindMetricWithLabelValues attempts to find a metric with a name with a set of label values
// If no metric is found, the *prometheus.Metric will be nil
func FindMetricWithLabelValues(name string, labelValues map[string]string) (*prometheus.Metric, bool) {
	metrics, err := crmetrics.Registry.Gather()
	ExpectWithOffset(1, err).To(BeNil())

	mf, found := lo.Find(metrics, func(mf *prometheus.MetricFamily) bool {
		return mf.GetName() == name
	})
	if !found {
		return nil, false
	}
	for _, m := range mf.Metric {
		temp := lo.Assign(labelValues)
		for _, labelPair := range m.Label {
			if v, ok := temp[labelPair.GetName()]; ok && v == labelPair.GetValue() {
				delete(temp, labelPair.GetName())
			}
		}
		if len(temp) == 0 {
			return m, true
		}
	}
	return nil, false
}

func ExpectManualBinding(ctx context.Context, c client.Client, pod *v1.Pod, node *v1.Node) {
	ExpectManualBindingWithOffset(1, ctx, c, pod, node)
}

func ExpectManualBindingWithOffset(offset int, ctx context.Context, c client.Client, pod *v1.Pod, node *v1.Node) {
	ExpectWithOffset(offset+1, c.Create(ctx, &v1.Binding{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.ObjectMeta.Name,
			Namespace: pod.ObjectMeta.Namespace,
			UID:       pod.ObjectMeta.UID,
		},
		Target: v1.ObjectReference{
			Name: node.Name,
		},
	})).To(Succeed())
	Eventually(func(g Gomega) {
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		g.Expect(pod.Spec.NodeName).To(Equal(node.Name))
	}).Should(Succeed())
}

func ExpectSkew(ctx context.Context, c client.Client, namespace string, constraint *v1.TopologySpreadConstraint) Assertion {
	nodes := &v1.NodeList{}
	ExpectWithOffset(1, c.List(ctx, nodes)).To(Succeed())
	pods := &v1.PodList{}
	ExpectWithOffset(1, c.List(ctx, pods, scheduling.TopologyListOptions(namespace, constraint.LabelSelector))).To(Succeed())
	skew := map[string]int{}
	for i, pod := range pods.Items {
		if scheduling.IgnoredForTopology(&pods.Items[i]) {
			continue
		}
		for _, node := range nodes.Items {
			if pod.Spec.NodeName == node.Name {
				switch constraint.TopologyKey {
				case v1.LabelHostname:
					skew[node.Name]++ // Check node name since hostname labels aren't applied
				default:
					if key, ok := node.Labels[constraint.TopologyKey]; ok {
						skew[key]++
					}
				}
			}
		}
	}
	return Expect(skew)
}

// ExpectResources expects all the resources in expected to exist in real with the same values
func ExpectResources(expected, real v1.ResourceList) {
	for k, v := range expected {
		realV := real[k]
		ExpectWithOffset(1, v.Value()).To(BeNumerically("~", realV.Value()))
	}
}

func ExpectNodes(ctx context.Context, c client.Client) []*v1.Node {
	return ExpectNodesWithOffset(1, ctx, c)
}

func ExpectNodesWithOffset(offset int, ctx context.Context, c client.Client) []*v1.Node {
	nodeList := &v1.NodeList{}
	ExpectWithOffset(offset+1, c.List(ctx, nodeList)).To(Succeed())
	return lo.Map(nodeList.Items, func(n v1.Node, _ int) *v1.Node {
		return &n
	})
}

func ExpectMachines(ctx context.Context, c client.Client) []*v1alpha5.Machine {
	return ExpectMachinesWithOffset(1, ctx, c)
}

func ExpectMachinesWithOffset(offset int, ctx context.Context, c client.Client) []*v1alpha5.Machine {
	machineList := &v1alpha5.MachineList{}
	ExpectWithOffset(offset+1, c.List(ctx, machineList)).To(Succeed())
	return lo.Map(machineList.Items, func(m v1alpha5.Machine, _ int) *v1alpha5.Machine {
		return &m
	})
}
