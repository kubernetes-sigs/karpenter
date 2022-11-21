package machine

import (
	"context"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	operatorcontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha6"
)

// Controller for the resource
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController is a constructor
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

// Reconcile a control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	// Read
	persisted := &v1alpha6.Machine{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, persisted); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// Reconcile
	machine := persisted.DeepCopy()
	var result reconcile.Result
	var err error
	if machine.DeletionTimestamp.IsZero() {
		result, err = c.reconcile(ctx, machine)
	} else {
		result, err = c.finalize(ctx, machine)
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	// Update
	if err := c.kubeClient.Status().Patch(ctx, machine, client.MergeFrom(persisted)); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	return result, nil
}

func (c *Controller) reconcile(ctx context.Context, machine *v1alpha6.Machine) (reconcile.Result, error) {
	// Get Machine
	status, err := c.cloudProvider.Get(ctx, machine)
	if err != nil {
		if errors.IsNotFound(err) {
			if _, err := c.cloudProvider.Create(ctx, machine); err != nil {
				return reconcile.Result{}, err
			} else {
				return reconcile.Result{Requeue: true}, nil
			}
		}
	}
	machine.StatusConditions().MarkTrue(v1alpha6.MachineCreated)
	machine.Status.ProviderID = status.ProviderID
	machine.Status.Allocatable = status.Allocatable
	machine.Status.Labels = status.Labels

	// Get Node
	nodeList := v1.NodeList{}
	if err := c.kubeClient.List(ctx, &nodeList, client.MatchingLabels{v1alpha5.MachineNameLabelKey: machine.Name}); err != nil {
		return reconcile.Result{}, err
	}
	if len(nodeList.Items) > 1 {
		machine.StatusConditions().MarkFalse(v1alpha6.MachineHealthy, "MultipleNodesFound", "invariant violated, machine matched multiple nodes %s",
			lo.Map(nodeList.Items, func(node v1.Node, _ int) string { return node.Name }))
		return reconcile.Result{}, nil
	}
	if len(nodeList.Items) == 0 {
		return reconcile.Result{}, nil
	}
	node := nodeList.Items[1]


	// Check initialized
	for key, expected := range machine.Status.Labels {
		if actual, ok := node.Labels[key]; !ok {
			return reconcile.Result{}, nil
		} else if actual != expected {
			machine.StatusConditions().MarkFalse(v1alpha6.MachineHealthy, "LabelMismatch", "expected label %s=%s to have value %s", key, expected, actual)
			return reconcile.Result{}, nil
		}
	}

	for resourceName, expected := range machine.Status.Allocatable {
		if actual, ok := node.Status.Allocatable[resourceName]; !ok {
			return reconcile.Result{}, nil
		} else if actual.Cmp(expected) < 0 {
			machine.StatusConditions().MarkFalse(v1alpha6.MachineHealthy, "AllocatableMismatch", "expected resource %s to be at least %s, but had %s ", resourceName, expected, actual)
			return reconcile.Result{}, nil
		}
	}

	machine.StatusConditions().MarkTrue(v1alpha6.MachineInitialized)
	machine.Status.Allocatable = node.Status.Allocatable
	machine.Status.Labels = node.Labels
	return reconcile.Result{}, nil
}

func (c *Controller) finalize(ctx context.Context, machine *v1alpha6.Machine) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) operatorcontroller.Builder {
	return controllerruntime.
		NewControllerManagedBy(m).
		For(&v1alpha6.Machine{}).
		Watches(
			&source.Kind{Type: &v1.Node{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				if name, ok := o.GetLabels()[v1alpha5.MachineNameLabelKey]; ok {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
				}
				return nil
			}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10})
}
