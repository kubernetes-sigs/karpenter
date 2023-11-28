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

package lifecycle

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	corecontroller "sigs.k8s.io/karpenter/pkg/operator/controller"
	nodepoolutil "sigs.k8s.io/karpenter/pkg/utils/nodepool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Controller is hash controller that constructs a hash based on the fields that are considered for static drift.
// The hash is placed in the metadata for increased observability and should be found on each object.
type Controller struct {
	kubeClient client.Client
    dynamicClient dynamic.DynamicClient
}

func NewController(kubeClient client.Client,dynamicClient dynamic.DynamicClient) *Controller {
	return &Controller{
		kubeClient: kubeClient,
        dynamicClient: dynamicClient,
	}
}


func (c *Controller) Reconcile(ctx context.Context, np *v1beta1.NodePool) (reconcile.Result, error) {
    nodeClassRef := np.Spec.Template.Spec.NodeClassRef
    if nodeClassRef == nil {
        return reconcile.Result{}, fmt.Errorf("nodeClassRef is nil")
    }

    group,version,found:= strings.Cut(nodeClassRef.APIVersion, "/")

    if !found {
        return reconcile.Result{}, fmt.Errorf("failed to parse apiVersion: %v", nodeClassRef.APIVersion)
    }

    gvr := schema.GroupVersionResource{
        Group:    group,
        Version:  version,
        Resource: strings.ToLower(nodeClassRef.Kind) + "es", 
    }

    nodeClassUnstructured, err := c.dynamicClient.Resource(gvr).Namespace(np.Namespace).Get(ctx, nodeClassRef.Name, metav1.GetOptions{})
    if err != nil {
        return reconcile.Result{}, fmt.Errorf("failed to get resource: %v", err)
    }

    // Check if the resource is ready (or perform your readiness checks here)
    if isResourceReady(nodeClassUnstructured) {
        nodepoolutil.UpdateStatusCondition(ctx, c.kubeClient, np, v1beta1.NodeClassReady, v1.ConditionTrue)
    } else {
        nodepoolutil.UpdateStatusCondition(ctx, c.kubeClient, np, v1beta1.NodeClassReady, v1.ConditionFalse)
    }

    return reconcile.Result{}, nil
}

func isResourceReady(nodeClassUnstructured *unstructured.Unstructured) bool {

    status, found, err := unstructured.NestedFieldCopy(nodeClassUnstructured.Object, "status")
    if err != nil || !found {
        return false
    }

    conditions, found, err := unstructured.NestedSlice(status.(map[string]interface{}), "conditions")
    if err != nil || !found {
        return false
    }

    for _, condition := range conditions {
        conditionMap, ok := condition.(map[string]interface{})
        if !ok {
            continue
        }

        conditionType, typeOk := conditionMap["type"].(string)
        if !typeOk {
            continue
        }
        conditionStatus, _ := conditionMap["status"].(string)

        if conditionStatus == "True" && conditionType == "Ready" {
            return true
        }
    }
        return false
}

type NodePoolController struct {
	*Controller
}

func NewNodePoolController(kubeClient client.Client,dynamicClient dynamic.DynamicClient) corecontroller.Controller {
	return corecontroller.Typed[*v1beta1.NodePool](kubeClient, &NodePoolController{
		Controller: NewController(kubeClient,dynamicClient),
	})
}

func (c *NodePoolController) Name() string {
	return "nodepool.lifecycle"
}

func (c *NodePoolController) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.Adapt(controllerruntime.
		NewControllerManagedBy(m).
		WithEventFilter(predicate.Funcs{
            UpdateFunc: func(e event.UpdateEvent) bool { 
                gvk := e.ObjectNew.GetObjectKind().GroupVersionKind()
                return gvk.Kind == "EC2NodeClass"
             },
        }).
		For(&v1beta1.NodePool{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}),
	)
}
