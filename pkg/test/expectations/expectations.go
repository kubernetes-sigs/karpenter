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

//nolint:revive
package expectations

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unique"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2" //nolint:revive
	. "github.com/onsi/gomega"    //nolint:revive
	"github.com/prometheus/client_golang/prometheus"
	prometheusmodel "github.com/prometheus/client_model/go"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	policyv1 "k8s.io/api/policy/v1"
	resourcev1 "k8s.io/api/resource/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/metrics"
	pscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/scheduling/dynamicresources"
	"sigs.k8s.io/karpenter/pkg/test"
	testv1alpha1 "sigs.k8s.io/karpenter/pkg/test/v1alpha1"
)

const (
	ReconcilerPropagationTime = 10 * time.Second
	RequestInterval           = 1 * time.Second
)

type ProvisioningResult struct {
	Bindings                        map[*corev1.Pod]*Binding
	ResourceClaimAllocationMetadata map[types.NamespacedName]*dynamicresources.ResourceClaimAllocationMetadata
}

type Binding struct {
	NodeClaim *v1.NodeClaim
	Node      *corev1.Node
}

func (r ProvisioningResult) Get(p *corev1.Pod) *Binding {
	for k, v := range r.Bindings {
		if client.ObjectKeyFromObject(k) == client.ObjectKeyFromObject(p) {
			return v
		}
	}
	return nil
}

func ExpectExists[T client.Object](ctx context.Context, c client.Client, obj T) T {
	GinkgoHelper()
	resp := reflect.New(reflect.TypeOf(*new(T)).Elem()).Interface().(T)
	Expect(c.Get(ctx, client.ObjectKeyFromObject(obj), resp)).To(Succeed())
	return resp
}

func ExpectPodExists(ctx context.Context, c client.Client, name string, namespace string) *corev1.Pod {
	GinkgoHelper()
	return ExpectExists(ctx, c, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
}

func ExpectNodeExists(ctx context.Context, c client.Client, name string) *corev1.Node {
	GinkgoHelper()
	return ExpectExists(ctx, c, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
}

func ExpectNotFound(ctx context.Context, c client.Client, objects ...client.Object) {
	GinkgoHelper()
	for _, object := range objects {
		Eventually(func() bool {
			return errors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: object.GetName(), Namespace: object.GetNamespace()}, object))
		}, ReconcilerPropagationTime, RequestInterval).Should(BeTrue(), func() string {
			return fmt.Sprintf("expected %s/%s to be deleted, but it still exists", lo.Must(apiutil.GVKForObject(object, scheme.Scheme)), client.ObjectKeyFromObject(object))
		})
	}
}

func ExpectScheduled(ctx context.Context, c client.Client, pod *corev1.Pod) *corev1.Node {
	GinkgoHelper()
	p := ExpectPodExists(ctx, c, pod.Name, pod.Namespace)
	Expect(p.Spec.NodeName).ToNot(BeEmpty(), fmt.Sprintf("expected %s/%s to be scheduled", pod.Namespace, pod.Name))
	return ExpectNodeExists(ctx, c, p.Spec.NodeName)
}

func ExpectPodsScheduled(ctx context.Context, c client.Client, pods ...*corev1.Pod) {
	GinkgoHelper()
	for _, p := range pods {
		ExpectScheduled(ctx, c, p)
	}
}

func ExpectNotScheduled(ctx context.Context, c client.Client, pod *corev1.Pod) *corev1.Pod {
	GinkgoHelper()
	p := ExpectPodExists(ctx, c, pod.Name, pod.Namespace)
	Eventually(p.Spec.NodeName).Should(BeEmpty(), fmt.Sprintf("expected %s/%s to not be scheduled", pod.Namespace, pod.Name))
	return p
}

func ExpectApplied(ctx context.Context, c client.Client, objects ...client.Object) {
	GinkgoHelper()
	for _, object := range objects {
		deletionTimestampSet := !object.GetDeletionTimestamp().IsZero()
		current := object.DeepCopyObject().(client.Object)
		statuscopy := object.DeepCopyObject().(client.Object) // Snapshot the status, since create/update may override

		// Create or Update
		if err := c.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
			if errors.IsNotFound(err) {
				Expect(c.Create(ctx, object)).To(Succeed())
			} else {
				Expect(err).ToNot(HaveOccurred())
			}
		} else {
			object.SetResourceVersion(current.GetResourceVersion())
			Expect(c.Update(ctx, object)).To(Succeed())
		}
		// Update status
		statuscopy.SetResourceVersion(object.GetResourceVersion())
		Expect(c.Status().Update(ctx, statuscopy)).To(Or(Succeed(), MatchError("the server could not find the requested resource"))) // Some objects do not have a status

		// Re-get the object to grab the updated spec and status
		Expect(c.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())

		// Set the deletion timestamp by adding a finalizer and deleting
		if deletionTimestampSet {
			ExpectDeletionTimestampSet(ctx, c, object)
		}
	}
}

func ExpectDeleted(ctx context.Context, c client.Client, objects ...client.Object) {
	GinkgoHelper()
	for _, object := range objects {
		if err := c.Delete(ctx, object, &client.DeleteOptions{GracePeriodSeconds: new(int64(0))}); !errors.IsNotFound(err) {
			Expect(err).To(BeNil())
		}
		ExpectNotFound(ctx, c, object)
	}
}

func ExpectReconciled(ctx context.Context, reconciler reconcile.Reconciler, request reconcile.Request) reconcile.Result {
	GinkgoHelper()
	result, err := reconciler.Reconcile(ctx, request)
	Expect(err).ToNot(HaveOccurred())
	return result
}

func ExpectReconciledFailed(ctx context.Context, reconciler reconcile.Reconciler, request reconcile.Request) reconcile.Result {
	GinkgoHelper()
	result, err := reconciler.Reconcile(ctx, request)
	Expect(err).To(HaveOccurred())
	return result
}

func ExpectSingletonReconciled(ctx context.Context, reconciler singleton.Reconciler) reconcile.Result {
	GinkgoHelper()
	result, err := singleton.AsReconciler(reconciler).Reconcile(ctx, reconcile.Request{})
	Expect(err).ToNot(HaveOccurred())
	return result
}

func ExpectSingletonReconcileFailed(ctx context.Context, reconciler singleton.Reconciler) error {
	GinkgoHelper()
	_, err := singleton.AsReconciler(reconciler).Reconcile(ctx, reconcile.Request{})
	Expect(err).To(HaveOccurred())
	return err
}

func ExpectObjectReconciled[T client.Object](ctx context.Context, c client.Client, reconciler reconcile.ObjectReconciler[T], object T) reconcile.Result {
	GinkgoHelper()
	result, err := reconcile.AsReconciler(c, reconciler).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(object)})
	Expect(err).ToNot(HaveOccurred())
	return result
}

func ExpectObjectReconcileFailed[T client.Object](ctx context.Context, c client.Client, reconciler reconcile.ObjectReconciler[T], object T) error {
	GinkgoHelper()
	_, err := reconcile.AsReconciler(c, reconciler).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(object)})
	Expect(err).To(HaveOccurred())
	return err
}

// ExpectDeletionTimestampSetWithOffset ensures that the deletion timestamp is set on the objects by adding a finalizer
// and then deleting the object immediately after. This holds the object until the finalizer is patched out in the DeferCleanup
func ExpectDeletionTimestampSet(ctx context.Context, c client.Client, objects ...client.Object) {
	GinkgoHelper()
	for _, object := range objects {
		Expect(c.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
		controllerutil.AddFinalizer(object, "testing/finalizer")
		Expect(c.Update(ctx, object)).To(Succeed())
		Expect(c.Delete(ctx, object)).To(Succeed())
		DeferCleanup(func(obj client.Object) {
			mergeFrom := client.MergeFrom(obj.DeepCopyObject().(client.Object))
			obj.SetFinalizers([]string{})
			Expect(client.IgnoreNotFound(c.Patch(ctx, obj, mergeFrom))).To(Succeed())
		}, object)
	}
}

func ExpectCleanedUp(ctx context.Context, c client.Client) {
	GinkgoHelper()
	wg := sync.WaitGroup{}
	namespaces := &corev1.NamespaceList{}
	Expect(c.List(ctx, namespaces)).To(Succeed())
	ExpectFinalizersRemovedFromList(ctx, c, &corev1.NodeList{}, &v1.NodeClaimList{}, &corev1.PersistentVolumeClaimList{})
	for _, object := range []client.Object{
		&corev1.Pod{},
		&corev1.Node{},
		&appsv1.DaemonSet{},
		&nodev1.RuntimeClass{},
		&policyv1.PodDisruptionBudget{},
		&corev1.PersistentVolumeClaim{},
		&corev1.PersistentVolume{},
		&storagev1.StorageClass{},
		&v1.NodePool{},
		&testv1alpha1.TestNodeClass{},
		&v1.NodeClaim{},
		&v1alpha1.NodeOverlay{},
		&resourcev1.ResourceClaim{},
		&resourcev1.ResourceClaimTemplate{},
	} {
		for _, namespace := range namespaces.Items {
			wg.Add(1)
			go func(object client.Object, namespace string) {
				GinkgoHelper()
				defer wg.Done()
				defer GinkgoRecover()
				err := c.DeleteAllOf(ctx, object, client.InNamespace(namespace),
					&client.DeleteAllOfOptions{DeleteOptions: client.DeleteOptions{GracePeriodSeconds: new(int64(0))}})
				// Fail open for CRDs that don't exist on this k8s version (e.g. ResourceClaim on < 1.34)
				if err != nil && !meta.IsNoMatchError(err) {
					Expect(err).ToNot(HaveOccurred())
				}
			}(object, namespace.Name)
		}
	}
	// Clean up cluster-scoped DRA objects. These aren't namespaced, so they're deleted once rather than per-namespace.
	// Fail open on clusters where these types aren't served (e.g. K8s < 1.34).
	for _, object := range []client.Object{
		&resourcev1.DeviceClass{},
		&resourcev1.ResourceSlice{},
	} {
		err := c.DeleteAllOf(ctx, object, &client.DeleteAllOfOptions{DeleteOptions: client.DeleteOptions{GracePeriodSeconds: new(int64(0))}})
		if err != nil && !meta.IsNoMatchError(err) {
			Expect(err).ToNot(HaveOccurred())
		}
	}
	wg.Wait()
}

func ExpectFinalizersRemovedFromList(ctx context.Context, c client.Client, objectLists ...client.ObjectList) {
	GinkgoHelper()
	for _, list := range objectLists {
		Expect(c.List(ctx, list)).To(Succeed())
		Expect(meta.EachListItem(list, func(o runtime.Object) error {
			obj := o.(client.Object)
			stored := obj.DeepCopyObject().(client.Object)
			obj.SetFinalizers([]string{})
			Expect(client.IgnoreNotFound(c.Patch(ctx, obj, client.MergeFrom(stored)))).To(Succeed())
			return nil
		})).To(Succeed())
	}
}

func ExpectFinalizersRemoved(ctx context.Context, c client.Client, objs ...client.Object) {
	GinkgoHelper()
	for _, obj := range objs {
		Expect(client.IgnoreNotFound(c.Get(ctx, client.ObjectKeyFromObject(obj), obj))).To(Succeed())
		stored := obj.DeepCopyObject().(client.Object)
		obj.SetFinalizers([]string{})
		Expect(client.IgnoreNotFound(c.Patch(ctx, obj, client.MergeFrom(stored)))).To(Succeed())
	}
}

func ExpectProvisioned(ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, provisioner *provisioning.Provisioner, pods ...*corev1.Pod) ProvisioningResult {
	GinkgoHelper()
	result := ExpectProvisionedNoBinding(ctx, c, cluster, cloudProvider, provisioner, pods...)
	podKeys := sets.NewString(lo.Map(pods, func(p *corev1.Pod, _ int) string { return client.ObjectKeyFromObject(p).String() })...)
	for pod, binding := range result.Bindings {
		// Only bind the pods that are passed through
		if podKeys.Has(client.ObjectKeyFromObject(pod).String()) {
			// We have to manually bind the pod to the node when using a fakeClient by setting the value for pod.Spec.NodeName
			if strings.Contains(reflect.TypeOf(c).String(), "fake") {
				pod.Spec.NodeName = binding.Node.Name
				err := c.Update(ctx, pod)
				Expect(err).ToNot(HaveOccurred())
				ExpectExists(ctx, c, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: binding.Node.Name, Namespace: binding.Node.Namespace}})
			} else {
				ExpectManualBinding(ctx, c, pod, binding.Node)
			}
			Expect(cluster.UpdatePod(ctx, pod)).To(Succeed()) // track pod bindings

			if result.ResourceClaimAllocationMetadata != nil {
				expectDRAClaimsAllocated(ctx, c, pod, binding, result.ResourceClaimAllocationMetadata)
			}
		}
	}
	return result
}

//nolint:gocyclo
func ExpectProvisionedNoBinding(ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, provisioner *provisioning.Provisioner, pods ...*corev1.Pod) ProvisioningResult {
	GinkgoHelper()
	// Persist objects
	for _, pod := range pods {
		ExpectApplied(ctx, c, pod)
	}
	// envtest has no kube-controller-manager, so the controller that generates ResourceClaims from
	// ResourceClaimTemplates and records them in pod status does not run. Emulate it so the provisioner's claim
	// resolution sees resolvable claims.
	for _, pod := range pods {
		ExpectResourceClaimsProcessed(ctx, c, pod)
	}
	// TODO: Check the error on the provisioner scheduling round
	results, err := provisioner.Schedule(ctx)
	result := ProvisioningResult{Bindings: map[*corev1.Pod]*Binding{}}
	if err != nil {
		log.Printf("error provisioning in test, %s", err)
		return result
	}
	for _, m := range results.NewNodeClaims {
		// TODO: Check the error on the provisioner launch
		nodeClaimName, err := provisioner.Create(ctx, m, provisioning.WithReason(metrics.ProvisionedReason))
		if err != nil {
			return result
		}
		nodeClaim := &v1.NodeClaim{}
		Expect(c.Get(ctx, types.NamespacedName{Name: nodeClaimName}, nodeClaim)).To(Succeed())
		nodeClaim, node := ExpectNodeClaimDeployedAndStateUpdated(ctx, c, cluster, cloudProvider, nodeClaim)
		if nodeClaim != nil && node != nil {
			for _, pod := range m.Pods {
				result.Bindings[pod] = &Binding{
					NodeClaim: nodeClaim,
					Node:      node,
				}
			}
		}
	}
	for _, node := range results.ExistingNodes {
		for _, pod := range node.Pods {
			result.Bindings[pod] = &Binding{
				Node: node.Node,
			}
			if node.NodeClaim != nil {
				result.Bindings[pod].NodeClaim = node.NodeClaim
			}
		}
	}
	result.ResourceClaimAllocationMetadata = results.DRAClaimAllocationMetadata
	return result
}

func ExpectProvisionedResults(ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, provisioner *provisioning.Provisioner, pods ...*corev1.Pod) scheduling.Results {
	GinkgoHelper()
	// Persist objects
	for _, pod := range pods {
		ExpectApplied(ctx, c, pod)
	}
	results, _ := provisioner.Schedule(ctx)
	return results
}

func ExpectNodeClaimDeployedNoNode(ctx context.Context, c client.Client, cloudProvider cloudprovider.CloudProvider, nc *v1.NodeClaim) (*v1.NodeClaim, error) {
	GinkgoHelper()

	resolved, err := cloudProvider.Create(ctx, nc)
	// TODO @joinnis: Check this error rather than swallowing it. This is swallowed right now due to how we are doing some testing in the cloudprovider
	if err != nil {
		return nc, err
	}
	Expect(err).To(Succeed())

	// Make the nodeclaim ready in the status conditions
	nc = lifecycle.PopulateNodeClaimDetails(nc, resolved)
	nc.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
	ExpectApplied(ctx, c, nc)
	return nc, nil
}

func ExpectNodeClaimDeployed(ctx context.Context, c client.Client, cloudProvider cloudprovider.CloudProvider, nc *v1.NodeClaim) (*v1.NodeClaim, *corev1.Node, error) {
	GinkgoHelper()

	nc, err := ExpectNodeClaimDeployedNoNode(ctx, c, cloudProvider, nc)
	if err != nil {
		return nc, nil, err
	}
	nc.StatusConditions().SetTrue(v1.ConditionTypeRegistered)

	// Mock the nodeclaim launch and node joining at the apiserver
	node := test.NodeClaimLinkedNode(nc)
	node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t corev1.Taint, _ int) bool { return t.MatchTaint(&v1.UnregisteredNoExecuteTaint) })
	node.Labels = lo.Assign(node.Labels, map[string]string{v1.NodeRegisteredLabelKey: "true"})
	nc.Status.NodeName = node.Name
	ExpectApplied(ctx, c, nc, node)
	expectResourceSlicesCreated(ctx, c, cloudProvider, nc, node)
	return nc, node, nil
}

func expectResourceSlicesCreated(ctx context.Context, c client.Client, cp cloudprovider.CloudProvider, nc *v1.NodeClaim, node *corev1.Node) {
	GinkgoHelper()

	instanceTypeName := nc.Labels[corev1.LabelInstanceTypeStable]
	if instanceTypeName == "" {
		return
	}
	np := &v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: nc.Labels[v1.NodePoolLabelKey]}}
	np.Spec.Template.Spec.NodeClassRef = nc.Spec.NodeClassRef
	instanceTypes, err := cp.GetInstanceTypes(ctx, np)
	Expect(err).ToNot(HaveOccurred())
	it, found := lo.Find(instanceTypes, func(it *cloudprovider.InstanceType) bool {
		return it.Name == instanceTypeName
	})
	Expect(found).To(BeTrue(), "instance type %q not found in cloud provider", instanceTypeName)
	if len(it.DynamicResources.ResourceSliceTemplates) == 0 {
		return
	}
	Expect(node.UID).ToNot(BeEmpty(), "node %q has no UID, cannot create owner reference", node.Name)

	// Group templates by driver to set ResourceSliceCount correctly
	templatesByDriver := map[string][]*cloudprovider.ResourceSliceTemplate{}
	for _, t := range it.DynamicResources.ResourceSliceTemplates {
		templatesByDriver[t.Driver.Value()] = append(templatesByDriver[t.Driver.Value()], t)
	}

	for driver, templates := range templatesByDriver {
		poolName := test.NodeLocalPoolName(driver, node.Name)
		sliceCount := int64(len(templates))
		for i, t := range templates {
			devices := make([]resourcev1.Device, len(t.Devices))
			for j, d := range t.Devices {
				device := resourcev1.Device{
					Name:             d.Name.Value(),
					Attributes:       d.Attributes,
					Capacity:         d.Capacity,
					ConsumesCounters: d.ConsumesCounters,
				}
				// Only set AllowMultipleAllocations when true; leaving it nil for exclusive devices preserves the
				// pre-capacity publish behavior (and a capacity RequestPolicy requires it to be true).
				if d.AllowMultipleAllocations {
					device.AllowMultipleAllocations = lo.ToPtr(true)
				}
				devices[j] = device
			}

			sliceName := fmt.Sprintf("%s-%s", node.Name, strings.ReplaceAll(strings.ReplaceAll(driver, ".", "-"), "/", "-"))
			if sliceCount > 1 {
				sliceName = fmt.Sprintf("%s-%d", sliceName, i)
			}
			slice := &resourcev1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name: sliceName,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "v1",
						Kind:       "Node",
						Name:       node.Name,
						UID:        node.UID,
					}},
				},
				Spec: resourcev1.ResourceSliceSpec{
					Driver:   driver,
					NodeName: &node.Name,
					Pool: resourcev1.ResourcePool{
						Name:               poolName,
						Generation:         1,
						ResourceSliceCount: sliceCount,
					},
					Devices:        devices,
					SharedCounters: t.SharedCounters,
				},
			}
			ExpectApplied(ctx, c, slice)
		}
	}
}

// ExpectResourceClaimsProcessed emulates the in-cluster ResourceClaim controller for a pod. For each of the pod's
// ResourceClaim references it ensures pod.Status.ResourceClaimStatuses is populated:
//   - A direct ResourceClaimName reference must already exist on the cluster; otherwise the expectation fails.
//   - A ResourceClaimTemplateName reference causes a ResourceClaim to be generated from the named template (if it
//     hasn't been already) and recorded in the pod status.
//
// This runs before provisioner.Schedule so the allocator's claim resolution can resolve every referenced claim.
func ExpectResourceClaimsProcessed(ctx context.Context, c client.Client, pod *corev1.Pod) {
	GinkgoHelper()
	if len(pod.Spec.ResourceClaims) == 0 {
		return
	}
	existingStatuses := sets.New(lo.Map(pod.Status.ResourceClaimStatuses, func(s corev1.PodResourceClaimStatus, _ int) string {
		return s.Name
	})...)
	for i := range pod.Spec.ResourceClaims {
		pc := &pod.Spec.ResourceClaims[i]
		if existingStatuses.Has(pc.Name) {
			continue
		}
		var claimName string
		switch {
		case pc.ResourceClaimName != nil:
			// The claim must have been created by the test beforehand.
			claim := &resourcev1.ResourceClaim{}
			Expect(c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: *pc.ResourceClaimName}, claim)).To(Succeed(),
				"referenced ResourceClaim %s/%s must exist before provisioning", pod.Namespace, *pc.ResourceClaimName)
			claimName = *pc.ResourceClaimName
		case pc.ResourceClaimTemplateName != nil:
			template := &resourcev1.ResourceClaimTemplate{}
			Expect(c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: *pc.ResourceClaimTemplateName}, template)).To(Succeed(),
				"referenced ResourceClaimTemplate %s/%s must exist before provisioning", pod.Namespace, *pc.ResourceClaimTemplateName)
			claimName = fmt.Sprintf("%s-%s", pod.Name, pc.Name)
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   pod.Namespace,
					Name:        claimName,
					Labels:      template.Spec.Labels,
					Annotations: template.Spec.Annotations,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "v1",
						Kind:       "Pod",
						Name:       pod.Name,
						UID:        pod.UID,
					}},
				},
				Spec: template.Spec.Spec,
			}
			ExpectApplied(ctx, c, claim)
		default:
			Fail(fmt.Sprintf("pod %s/%s claim reference %q has neither ResourceClaimName nor ResourceClaimTemplateName", pod.Namespace, pod.Name, pc.Name))
		}
		pod.Status.ResourceClaimStatuses = append(pod.Status.ResourceClaimStatuses, corev1.PodResourceClaimStatus{
			Name:              pc.Name,
			ResourceClaimName: lo.ToPtr(claimName),
		})
	}
	ExpectApplied(ctx, c, pod)
}

// ExpectDeviceAllocationReconciled hydrates the deviceallocation controller (unblocking AllocatedDevices) and reconciles
// every ResourceClaim currently on the cluster. In production, hydration is triggered by a manager runnable after cache
// sync; in envtest there is no running controller manager, so tests must call this explicitly before a provisioning
// round that relies on the in-cluster allocated-device set.
func ExpectDeviceAllocationReconciled(ctx context.Context, c client.Client, controller *deviceallocation.Controller) {
	GinkgoHelper()
	// Hydrate is guarded by sync.Once internally, so calling it multiple times is safe.
	controller.Hydrate(ctx)
	// Reconcile every existing claim to pick up allocation-status changes committed by prior provisioning runs.
	claimList := &resourcev1.ResourceClaimList{}
	Expect(c.List(ctx, claimList)).To(Succeed())
	for i := range claimList.Items {
		ExpectReconciled(ctx, controller, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&claimList.Items[i])})
	}
}

// ExpectNodeClaimDRADrivers asserts the NodeClaim carries the karpenter.sh/requested-dra-drivers annotation listing exactly the
// expected driver names (order-independent).
func ExpectNodeClaimDRADrivers(nc *v1.NodeClaim, drivers ...string) {
	GinkgoHelper()
	annotation, ok := nc.Annotations[v1.DRADriversAnnotationKey]
	Expect(ok).To(BeTrue(), "NodeClaim %s missing %s annotation", nc.Name, v1.DRADriversAnnotationKey)
	Expect(strings.Split(annotation, ",")).To(ConsistOf(drivers))
}

// ExpectResourceClaimAllocated asserts a ResourceClaim has an allocation referencing the expected driver, and that
// every allocated device belongs to that driver. Returns the pool-qualified device identities ("pool/device") for
// further assertions. Pool qualification matters because node-local template devices reuse the same device name
// across nodes — only the pool distinguishes them.
func ExpectResourceClaimAllocated(ctx context.Context, c client.Client, namespace, name, driver string) []string {
	GinkgoHelper()
	claim := &resourcev1.ResourceClaim{}
	Expect(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, claim)).To(Succeed())
	Expect(claim.Status.Allocation).ToNot(BeNil(), "ResourceClaim %s/%s has no allocation", namespace, name)
	results := claim.Status.Allocation.Devices.Results
	Expect(results).ToNot(BeEmpty())
	devices := make([]string, 0, len(results))
	for _, r := range results {
		Expect(r.Driver).To(Equal(driver))
		devices = append(devices, fmt.Sprintf("%s/%s", r.Pool, r.Device))
	}
	return devices
}

// ExpectInstanceTypeOptionNames returns the names of a NodeClaim's candidate instance type options.
func ExpectInstanceTypeOptionNames(nc *scheduling.NodeClaim) []string {
	GinkgoHelper()
	return lo.Map(nc.InstanceTypeOptions, func(it *cloudprovider.InstanceType, _ int) string { return it.Name })
}

func expectDRAClaimsAllocated(ctx context.Context, c client.Client, pod *corev1.Pod, binding *Binding, claimMetadata map[types.NamespacedName]*dynamicresources.ResourceClaimAllocationMetadata) {
	GinkgoHelper()

	instanceTypeName := binding.Node.Labels[corev1.LabelInstanceTypeStable]
	itID := unique.Make(instanceTypeName)

	for i := range pod.Spec.ResourceClaims {
		podClaim := &pod.Spec.ResourceClaims[i]
		// Resolve the backing claim name the same way the allocator does: a direct ResourceClaimName, otherwise the
		// generated name recorded in pod status (for ResourceClaimTemplate references). A reference with no generated
		// claim is skipped — there is nothing to allocate.
		claimName, ok := resolvedClaimName(pod, podClaim)
		if !ok {
			continue
		}
		key := types.NamespacedName{Namespace: pod.Namespace, Name: claimName}

		claim := &resourcev1.ResourceClaim{}
		Expect(c.Get(ctx, key, claim)).To(Succeed())
		if claim.Status.Allocation != nil {
			continue
		}

		meta, ok := claimMetadata[key]
		Expect(ok).To(BeTrue(), "missing DRA allocation metadata for claim %s", key)
		devices, ok := meta.Devices[itID]
		Expect(ok).To(BeTrue(), "no device allocation for instance type %q in claim %s", instanceTypeName, key)

		// The API server requires each DeviceRequestAllocationResult.Request to name a real request in the claim. The
		// allocator metadata only carries the ordered devices, not their owning request, but the allocator allocates
		// requests in spec order, so we reconstruct the request name by consuming each request's device count in order.
		requestNames := requestNamesForDevices(claim, len(devices))
		results := make([]resourcev1.DeviceRequestAllocationResult, len(devices))
		for i, device := range devices {
			poolName := device.DeviceID.Pool.Value()
			if device.DeviceID.Template {
				poolName = test.NodeLocalPoolName(device.DeviceID.Driver.Value(), binding.Node.Name)
			}
			results[i] = resourcev1.DeviceRequestAllocationResult{
				Request: requestNames[i],
				Driver:  device.DeviceID.Driver.Value(),
				Pool:    poolName,
				Device:  device.DeviceID.Device.Value(),
			}
		}

		claim.Status.Allocation = &resourcev1.AllocationResult{
			Devices: resourcev1.DeviceAllocationResult{
				Results: results,
			},
		}
		ExpectApplied(ctx, c, claim)
	}
}

// resolvedClaimName returns the name of the ResourceClaim backing a pod's claim reference: a direct ResourceClaimName,
// otherwise the generated name recorded in pod.Status.ResourceClaimStatuses for a ResourceClaimTemplate reference. The
// second return is false when no claim was generated for the reference. This mirrors the allocator's own resolution so
// the test framework allocates the same claims the allocator did.
func resolvedClaimName(pod *corev1.Pod, pc *corev1.PodResourceClaim) (string, bool) {
	if pc.ResourceClaimName != nil {
		return *pc.ResourceClaimName, true
	}
	for i := range pod.Status.ResourceClaimStatuses {
		status := &pod.Status.ResourceClaimStatuses[i]
		if status.Name == pc.Name {
			if status.ResourceClaimName == nil {
				return "", false
			}
			return *status.ResourceClaimName, true
		}
	}
	return "", false
}

// requestNamesForDevices maps each of the deviceCount allocated devices (in allocation order) to the name of the claim
// request that owns it. Requests are consumed in spec order: an ExactCount request claims its Count devices; an All
// request (and any leftover devices) claim the remainder. This mirrors the allocator's request-ordered DFS so the
// reconstructed request names are valid for API server validation.
// TODO: Consider storing the associated request in ResourceClaimAllocationMetadata to avoid reverse engineering the
// order. This isn't currently done since it would only be useful for integration tests.
func requestNamesForDevices(claim *resourcev1.ResourceClaim, deviceCount int) []string {
	names := make([]string, 0, deviceCount)
	for _, req := range claim.Spec.Devices.Requests {
		if len(names) >= deviceCount {
			break
		}
		count := 1
		if req.Exactly != nil {
			switch {
			case req.Exactly.AllocationMode == resourcev1.DeviceAllocationModeAll:
				count = deviceCount - len(names) // claim all remaining devices
			case req.Exactly.Count > 0:
				count = int(req.Exactly.Count)
			}
		}
		for i := 0; i < count && len(names) < deviceCount; i++ {
			names = append(names, req.Name)
		}
	}
	// Fallback: if requests didn't account for every device (shouldn't happen), pad with the last request name.
	for len(names) < deviceCount && len(claim.Spec.Devices.Requests) > 0 {
		names = append(names, claim.Spec.Devices.Requests[len(claim.Spec.Devices.Requests)-1].Name)
	}
	return names
}

func ExpectNodeClaimDeployedAndStateUpdated(ctx context.Context, c client.Client, cluster *state.Cluster, cloudProvider cloudprovider.CloudProvider, nc *v1.NodeClaim) (*v1.NodeClaim, *corev1.Node) {
	GinkgoHelper()

	nc, node, err := ExpectNodeClaimDeployed(ctx, c, cloudProvider, nc)
	cluster.UpdateNodeClaim(nc)
	if err != nil {
		return nc, nil
	}
	Expect(cluster.UpdateNode(ctx, node)).To(Succeed())
	return nc, node
}

func ExpectNodeClaimsCascadeDeletion(ctx context.Context, c client.Client, nodeClaims ...*v1.NodeClaim) {
	GinkgoHelper()
	nodes := ExpectNodes(ctx, c)
	for _, nodeClaim := range nodeClaims {
		err := c.Get(ctx, client.ObjectKeyFromObject(nodeClaim), &v1.NodeClaim{})
		if !errors.IsNotFound(err) {
			continue
		}
		for _, node := range nodes {
			if node.Spec.ProviderID == nodeClaim.Status.ProviderID {
				Expect(c.Delete(ctx, node))
				ExpectFinalizersRemoved(ctx, c, node)
				ExpectNotFound(ctx, c, node)
			}
		}
	}
}

func ExpectMakeNodeClaimsInitialized(ctx context.Context, c client.Client, clk clock.Clock, nodeClaims ...*v1.NodeClaim) {
	GinkgoHelper()
	for i := range nodeClaims {
		nodeClaims[i] = ExpectExists(ctx, c, nodeClaims[i])
		nodeClaims[i].StatusConditions(status.WithClock(clk)).SetTrue(v1.ConditionTypeLaunched)
		nodeClaims[i].StatusConditions(status.WithClock(clk)).SetTrue(v1.ConditionTypeRegistered)
		nodeClaims[i].StatusConditions(status.WithClock(clk)).SetTrue(v1.ConditionTypeInitialized)
		ExpectApplied(ctx, c, nodeClaims[i])
	}
}

func ExpectMakeNodesInitialized(ctx context.Context, c client.Client, clk clock.Clock, nodes ...*corev1.Node) {
	GinkgoHelper()
	ExpectMakeNodesReady(ctx, c, clk, nodes...)

	for i := range nodes {
		nodes[i].Spec.Taints = lo.Reject(nodes[i].Spec.Taints, func(t corev1.Taint, _ int) bool { return t.MatchTaint(&v1.UnregisteredNoExecuteTaint) })
		nodes[i].Labels[v1.NodeRegisteredLabelKey] = "true"
		nodes[i].Labels[v1.NodeInitializedLabelKey] = "true"
		ExpectApplied(ctx, c, nodes[i])
	}
}

func ExpectMakeNodesNotReady(ctx context.Context, c client.Client, clk clock.Clock, nodes ...*corev1.Node) {
	for i := range nodes {
		nodes[i] = ExpectExists(ctx, c, nodes[i])
		nodes[i].Status.Phase = corev1.NodeRunning
		nodes[i].Status.Conditions = []corev1.NodeCondition{
			{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionFalse,
				LastHeartbeatTime:  metav1.NewTime(clk.Now()),
				LastTransitionTime: metav1.NewTime(clk.Now()),
				Reason:             "NotReady",
			},
		}
		if nodes[i].Labels == nil {
			nodes[i].Labels = map[string]string{}
		}
		ExpectApplied(ctx, c, nodes[i])
	}
}

func ExpectMakeNodesReady(ctx context.Context, c client.Client, clk clock.Clock, nodes ...*corev1.Node) {
	for i := range nodes {
		nodes[i] = ExpectExists(ctx, c, nodes[i])
		nodes[i].Status.Phase = corev1.NodeRunning
		nodes[i].Status.Conditions = []corev1.NodeCondition{
			{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionTrue,
				LastHeartbeatTime:  metav1.NewTime(clk.Now()),
				LastTransitionTime: metav1.NewTime(clk.Now()),
				Reason:             "KubeletReady",
			},
		}
		if nodes[i].Labels == nil {
			nodes[i].Labels = map[string]string{}
		}
		// Remove any of the known ephemeral taints to make the Node ready
		nodes[i].Spec.Taints = lo.Reject(nodes[i].Spec.Taints, func(taint corev1.Taint, _ int) bool {
			_, found := lo.Find(pscheduling.KnownEphemeralTaints, func(t corev1.Taint) bool {
				return t.MatchTaint(&taint)
			})
			return found
		})
		ExpectApplied(ctx, c, nodes[i])
	}
}

func ExpectReconcileSucceeded(ctx context.Context, reconciler reconcile.Reconciler, key client.ObjectKey) reconcile.Result {
	GinkgoHelper()
	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	Expect(err).ToNot(HaveOccurred())
	return result
}

func ExpectStatusConditionExists(obj status.Object, t string) status.Condition {
	GinkgoHelper()
	conds := obj.GetConditions()
	cond, ok := lo.Find(conds, func(c status.Condition) bool {
		return c.Type == t
	})
	Expect(ok).To(BeTrue())
	return cond
}

func ExpectOwnerReferenceExists(obj, owner client.Object) metav1.OwnerReference {
	or, found := lo.Find(obj.GetOwnerReferences(), func(o metav1.OwnerReference) bool {
		return o.UID == owner.GetUID()
	})
	Expect(found).To(BeTrue())
	return or
}

// ExpectMetricName attempts to resolve a metric name from a collector. This function will work so long as the fully
// qualified name is a single metric name. This holds true for the built in types, but may not for custom collectors.
func ExpectMetricName(collector prometheus.Collector) string {
	GinkgoHelper()

	// Prometheus defines an async method to resolve the description for a collector. This is simpler than it looks,
	// Describe just returns a string through the provided channel.
	result := make(chan *prometheus.Desc)
	var desc *prometheus.Desc
	go func() {
		collector.Describe(result)
	}()
	select {
	case desc = <-result:
	// Add a timeout so a failure doesn't result in stalling the entire test suite. This should never occur.
	case <-time.After(time.Second):
	}
	Expect(desc).ToNot(BeNil())

	// Extract the fully qualified name from the description string. This is just different enough from json that we
	// need to parse with regex.
	rgx := regexp.MustCompile(`^.*fqName:\s*"([^"]*).*$`)
	matches := rgx.FindStringSubmatch(desc.String())
	Expect(len(matches)).To(Equal(2))
	return matches[1]
}

// FindMetricWithLabelValues attempts to find a metric with a name with a set of label values
// If no metric is found, the *prometheusmodel.Metric will be nil
func FindMetricWithLabelValues(name string, labelValues map[string]string) (*prometheusmodel.Metric, bool) {
	GinkgoHelper()
	metrics, err := crmetrics.Registry.Gather()
	Expect(err).To(BeNil())

	mf, found := lo.Find(metrics, func(mf *prometheusmodel.MetricFamily) bool {
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

func ExpectMetricGaugeValue(collector opmetrics.GaugeMetric, expectedValue float64, labels map[string]string) {
	GinkgoHelper()
	metricName := ExpectMetricName(collector.(*opmetrics.PrometheusGauge))
	metric, ok := FindMetricWithLabelValues(metricName, labels)
	Expect(ok).To(BeTrue(), "Metric "+metricName+" should be available")
	Expect(lo.FromPtr(metric.Gauge.Value)).To(Equal(expectedValue), "Metric "+metricName+" should have the expected value")
}

func ExpectMetricCounterValue(collector opmetrics.CounterMetric, expectedValue float64, labels map[string]string) {
	GinkgoHelper()
	metricName := ExpectMetricName(collector.(*opmetrics.PrometheusCounter))
	metric, ok := FindMetricWithLabelValues(metricName, labels)
	Expect(ok).To(BeTrue(), "Metric "+metricName+" should be available")
	Expect(lo.FromPtr(metric.Counter.Value)).To(Equal(expectedValue), "Metric "+metricName+" should have the expected value")
}

func ExpectMetricHistogramSampleCountValue(metricName string, expectedValue uint64, labels map[string]string) {
	GinkgoHelper()
	metric, ok := FindMetricWithLabelValues(metricName, labels)
	Expect(ok).To(BeTrue(), "Metric "+metricName+" should be available")
	Expect(lo.FromPtr(metric.Histogram.SampleCount)).To(Equal(expectedValue), "Metric "+metricName+" should have the expected value")
}

func ExpectManualBinding(ctx context.Context, c client.Client, pod *corev1.Pod, node *corev1.Node) {
	GinkgoHelper()
	Expect(c.Create(ctx, &corev1.Binding{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.ObjectMeta.Name,
			Namespace: pod.ObjectMeta.Namespace,
			UID:       pod.ObjectMeta.UID,
		},
		Target: corev1.ObjectReference{
			Name: node.Name,
		},
	})).To(Succeed())
	Eventually(func(g Gomega) {
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		g.Expect(pod.Spec.NodeName).To(Equal(node.Name))
	}).Should(Succeed())
}

func ExpectSkew(ctx context.Context, c client.Client, namespace string, constraint *corev1.TopologySpreadConstraint) Assertion {
	GinkgoHelper()
	nodes := &corev1.NodeList{}
	Expect(c.List(ctx, nodes)).To(Succeed())
	pods := &corev1.PodList{}
	Expect(c.List(ctx, pods, scheduling.TopologyListOptions(namespace, constraint.LabelSelector))).To(Succeed())
	skew := map[string]int{}
	for i, pod := range pods.Items {
		if scheduling.IgnoredForTopology(&pods.Items[i]) {
			continue
		}
		for _, node := range nodes.Items {
			if pod.Spec.NodeName == node.Name {
				switch constraint.TopologyKey {
				case corev1.LabelHostname:
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
func ExpectResources(expected, real corev1.ResourceList) {
	GinkgoHelper()
	for k, v := range expected {
		realV := real[k]
		Expect(v.Value()).To(BeNumerically("~", realV.Value()))
	}
}

func ExpectNodes(ctx context.Context, c client.Client) []*corev1.Node {
	GinkgoHelper()
	nodeList := &corev1.NodeList{}
	Expect(c.List(ctx, nodeList)).To(Succeed())
	return lo.ToSlicePtr(nodeList.Items)
}

func ExpectNodeClaims(ctx context.Context, c client.Client) []*v1.NodeClaim {
	GinkgoHelper()
	nodeClaims := &v1.NodeClaimList{}
	Expect(c.List(ctx, nodeClaims)).To(Succeed())
	return lo.ToSlicePtr(nodeClaims.Items)
}

func ExpectStateNodeExists(cluster *state.Cluster, node *corev1.Node) *state.StateNode {
	GinkgoHelper()
	var ret *state.StateNode
	for n := range cluster.Nodes() {
		if n.Node.Name == node.Name {
			ret = n.DeepCopy()
			break
		}
	}
	Expect(ret).ToNot(BeNil())
	return ret
}

func ExpectStateNodeExistsForNodeClaim(cluster *state.Cluster, nodeClaim *v1.NodeClaim) *state.StateNode {
	GinkgoHelper()
	var ret *state.StateNode
	for n := range cluster.Nodes() {
		if n.NodeClaim.Status.ProviderID == nodeClaim.Status.ProviderID {
			ret = n.DeepCopy()
			break
		}
	}
	Expect(ret).ToNot(BeNil())
	return ret
}

func ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx context.Context, c client.Client, clk clock.Clock, nodeStateController *informer.NodeController, nodeClaimStateController *informer.NodeClaimController, nodes []*corev1.Node, nodeClaims []*v1.NodeClaim) {
	GinkgoHelper()

	ExpectMakeNodesInitialized(ctx, c, clk, nodes...)
	ExpectMakeNodeClaimsInitialized(ctx, c, clk, nodeClaims...)

	// Inform cluster state about node and nodeclaim readiness
	for _, n := range nodes {
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
	}
	for _, m := range nodeClaims {
		ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(m))
	}
}

// ExpectEvicted triggers an eviction call for all the passed pods
func ExpectEvicted(ctx context.Context, c client.Client, pods ...*corev1.Pod) {
	GinkgoHelper()

	for _, pod := range pods {
		Expect(c.SubResource("eviction").Create(ctx, pod, &policyv1.Eviction{})).To(Succeed())
	}
	EventuallyExpectTerminating(ctx, c, lo.Map(pods, func(p *corev1.Pod, _ int) client.Object { return p })...)
}

// EventuallyExpectTerminating ensures that the deletion timestamp is eventually set
// We need this since there is some propagation time for the eviction API to set the deletionTimestamp
func EventuallyExpectTerminating(ctx context.Context, c client.Client, objs ...client.Object) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		for _, obj := range objs {
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
			g.Expect(obj.GetDeletionTimestamp().IsZero()).ToNot(BeTrue())
		}
	}, time.Second).Should(Succeed())
}

// ConsistentlyExpectNotTerminating ensures that the deletion timestamp is not set
// We need this since there is some propagation time for the eviction API to set the deletionTimestamp
func ConsistentlyExpectNotTerminating(ctx context.Context, c client.Client, objs ...client.Object) {
	GinkgoHelper()

	Consistently(func(g Gomega) {
		for _, obj := range objs {
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
			g.Expect(obj.GetDeletionTimestamp().IsZero()).To(BeTrue())
		}
	}, time.Second).Should(Succeed())
}

func ExpectParallelized(fs ...func()) {
	wg := sync.WaitGroup{}
	wg.Add(len(fs))
	for _, f := range fs {
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			f()
		}()
	}
	wg.Wait()
}

func ExpectStateNodePoolCount(cluster *state.Cluster, npName string, r, d, pd int) {
	GinkgoHelper()

	running, deleting, pendingdisruption := cluster.NodePoolState.GetNodeCount(npName)
	Expect(running).To(Equal(r))
	Expect(deleting).To(Equal(d))
	Expect(pendingdisruption).To(Equal(pd))
}
